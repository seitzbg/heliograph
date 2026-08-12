// Package pgstore is a TimescaleDB (PostgreSQL) implementation of store.Store.
// Each measurement round is one row in the `samples` hypertable, keeping the
// raw per-round sample array so smoke bands can be computed from the full
// distribution — the durable equivalent of SmokePing's RRD, minus the fixed
// schema (see codemap 07 §4).
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
)

// writeTimeout bounds a single sample-batch write. The collector writes on a
// process-level context, so without a per-write deadline a hung DB connection could
// pin a scheduler worker indefinitely (the target is held in flight through the write
// + alert eval). 10s is far above a healthy batch insert (CODE_REVIEW #5).
const writeTimeout = 10 * time.Second

type PGStore struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	histCap int
	onErr   func(error)
	// writeFails counts sample-batch writes that failed after retries/deadline — a
	// persistent-write health signal exposed on /metrics (writes are fire-and-forget
	// from the round's point of view, so this is how an operator sees them failing).
	writeFails atomic.Int64
}

// New connects to the DSN, applies the schema, and returns a store. histCap
// bounds how many recent rounds History returns per target. onErr (optional)
// receives async write errors; defaults to ignoring them.
func New(ctx context.Context, dsn string, histCap int, onErr func(error)) (*PGStore, error) {
	if histCap <= 0 {
		histCap = 1024
	}
	if onErr == nil {
		onErr = func(error) {}
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	s := &PGStore{ctx: ctx, pool: pool, histCap: histCap, onErr: onErr}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *PGStore) Close() { s.pool.Close() }

const schema = `
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE TABLE IF NOT EXISTS samples (
	ts             timestamptz        NOT NULL,
	target         text               NOT NULL,
	probe          text               NOT NULL,
	host           text               NOT NULL,
	vantage        text               NOT NULL DEFAULT 'local',
	pings          smallint           NOT NULL,
	loss           smallint           NOT NULL,
	median_seconds double precision,
	rtts_seconds   double precision[] NOT NULL,
	err            text,
	duration_ms    double precision
);
SELECT create_hypertable('samples', 'ts', if_not_exists => TRUE);
-- Upgrade a pre-federation database (created before the vantage column existed):
-- CREATE TABLE IF NOT EXISTS above never alters a table that already exists, so a
-- 0.1-era 'samples' would still lack the column the unique index below references.
-- ADD COLUMN IF NOT EXISTS is a no-op on a fresh table (the CREATE already has the
-- column) and, on an old one, adds it and backfills every existing row to 'local'
-- (all pre-federation rows were hub-local measurements) — which also keeps
-- renameLegacyVantage a no-op for them.
ALTER TABLE samples ADD COLUMN IF NOT EXISTS vantage text NOT NULL DEFAULT 'local';
CREATE UNIQUE INDEX IF NOT EXISTS samples_target_vantage_ts ON samples (target, vantage, ts);
DROP INDEX IF EXISTS samples_target_ts;
`

func (s *PGStore) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
	return s.renameLegacyVantage(ctx)
}

// renameLegacyVantage renames hub rows written before the vantage was named 'local'
// (the column shipped defaulting to 'master'). It runs once: it keys off the column's
// current default, so after it flips the default to 'local' every later boot skips —
// which also means it never rewrites a legitimate future 'master' value.
func (s *PGStore) renameLegacyVantage(ctx context.Context) error {
	var def string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		  FROM pg_attribute a
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE a.attrelid = 'samples'::regclass AND a.attname = 'vantage'`).Scan(&def); err != nil {
		return fmt.Errorf("pgstore: read vantage default: %w", err)
	}
	if !strings.Contains(def, "'master'") {
		return nil // already on 'local' — nothing to rename
	}
	for _, q := range []string{
		`UPDATE samples SET vantage = 'local' WHERE vantage = 'master'`,
		`ALTER TABLE samples ALTER COLUMN vantage SET DEFAULT 'local'`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: rename legacy vantage: %w", err)
		}
	}
	return nil
}

// downsampling statements (each must run outside an explicit transaction).
var downsampleStmts = []string{
	`CREATE MATERIALIZED VIEW IF NOT EXISTS samples_hourly
	 WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
	 SELECT time_bucket('1 hour', ts) AS bucket,
	        target,
	        vantage,
	        avg(median_seconds) AS median_avg,
	        min(median_seconds) AS median_min,
	        max(median_seconds) AS median_max,
	        avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
	        count(*) AS rounds,
	        count(median_seconds) AS median_rounds
	 FROM samples
	 GROUP BY bucket, target, vantage
	 WITH NO DATA`,
	`SELECT add_continuous_aggregate_policy('samples_hourly',
	        start_offset => INTERVAL '3 days',
	        end_offset => INTERVAL '1 hour',
	        schedule_interval => INTERVAL '1 hour',
	        if_not_exists => TRUE)`,
	`CREATE MATERIALIZED VIEW IF NOT EXISTS samples_daily
	 WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
	 SELECT time_bucket('1 day', ts) AS bucket,
	        target,
	        vantage,
	        avg(median_seconds) AS median_avg,
	        min(median_seconds) AS median_min,
	        max(median_seconds) AS median_max,
	        avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
	        count(*) AS rounds,
	        count(median_seconds) AS median_rounds
	 FROM samples
	 GROUP BY bucket, target, vantage
	 WITH NO DATA`,
	// Refresh the trailing 30 days of daily buckets hourly, so each day is
	// materialized well before the 30-day raw retention drops its source rows —
	// the daily tier then persists for the 400d range even though raw does not.
	`SELECT add_continuous_aggregate_policy('samples_daily',
	        start_offset => INTERVAL '30 days',
	        end_offset => INTERVAL '1 hour',
	        schedule_interval => INTERVAL '1 hour',
	        if_not_exists => TRUE)`,
	`SELECT add_retention_policy('samples', INTERVAL '30 days', if_not_exists => TRUE)`,
}

// EnableDownsampling creates the hourly and daily continuous aggregates (median
// avg/min/max + loss per target per vantage per bucket — the coarse tiers a
// long-range view reads, akin to SmokePing's RRAs; daily feeds the 400d range),
// their refresh policies, and a 30-day retention policy on the raw samples.
// Idempotent.
func (s *PGStore) EnableDownsampling(ctx context.Context) error {
	existed, err := s.aggregateExists(ctx)
	if err != nil {
		return err
	}
	dropped, err := s.migrateAggregates(ctx)
	if err != nil {
		return err
	}
	for _, q := range downsampleStmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: downsampling: %w", err)
		}
	}
	// On first creation (fresh DB) or after a shape-change recreate, the aggregates start
	// empty and the refresh policies only cover the trailing 3d (hourly) / 30d (daily) — so
	// still-present older raw history (the 10d hourly view; daily up to the 30d raw retention)
	// would never materialize. Run a one-time bounded initial refresh to backfill it
	// (CODE_REVIEW #4). New data thereafter is materialized by the policies.
	if !existed || dropped {
		if err := s.backfillAggregates(ctx); err != nil {
			return err
		}
	}
	return nil
}

// aggregateExists reports whether BOTH continuous aggregates (samples_hourly and samples_daily) are
// already present. Requiring both — not just hourly — means EnableDownsampling runs its one-time
// backfill whenever EITHER is missing: an hourly-present-but-daily-absent DB would otherwise recreate
// samples_daily empty (WITH NO DATA) and skip the initial backfill, leaving daily history incomplete
// until trailing refreshes catch up (CODE_REVIEW L1).
func (s *PGStore) aggregateExists(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('samples_hourly') IS NOT NULL AND to_regclass('samples_daily') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("pgstore: check aggregates: %w", err)
	}
	return ok, nil
}

// backfillAggregates materializes existing raw history into the aggregates over the ranges the
// long views promise (10 days hourly; up to the 30-day raw retention for daily) — what the
// trailing refresh policies never cover after a fresh create/recreate. Bounded and one-time
// (EnableDownsampling only calls it on create/recreate). Must run outside an explicit txn.
//
// Runs through execWithRetry (the same SQLSTATE 55P03 lock_not_available retry RefreshAggregates
// uses) rather than a raw s.pool.Exec: a background continuous-aggregate refresh policy can hold
// the same cagg's refresh lock at the moment this CALL runs, and TimescaleDB aborts the loser
// instead of queuing it. Without the retry, that ordinary contention surfaced as a startup
// fatal() (EnableDownsampling is called from cold start) and an intermittent flake in
// TestDailyRollup / TestRollupMedianRoundsExcludesLostRounds.
func (s *PGStore) backfillAggregates(ctx context.Context) error {
	for _, q := range []string{
		`CALL refresh_continuous_aggregate('samples_hourly', now() - INTERVAL '10 days', now() - INTERVAL '1 hour')`,
		`CALL refresh_continuous_aggregate('samples_daily', now() - INTERVAL '30 days', now() - INTERVAL '1 hour')`,
	} {
		if err := s.execWithRetry(ctx, q); err != nil {
			return fmt.Errorf("pgstore: backfill aggregates: %w", err)
		}
	}
	return nil
}

// migrateAggregates drops the existing continuous aggregates when their stored shape no
// longer matches the current definition, so the recreate below can rebuild them. It fires
// when an existing samples_hourly lacks either the vantage dimension (federation) or the
// median_rounds column (CODE_REVIEW #6 / P2-6). One-time per change: after the recreate the
// columns exist and the guard is false forever after (a fresh DB has no view yet — no-op).
//
// A TimescaleDB continuous aggregate's SELECT/GROUP BY can't be altered in place, so this
// drop+recreate is unavoidable — but on an existing DB it irreversibly loses every daily
// rollup bucket older than the 30-day raw retention window (the raw rows behind those
// buckets are already gone, so they can never be rebuilt). This is a one-time long-range-
// history loss the first time an older hub upgrades and calls EnableDownsampling; see the
// slog.Warn below, logged only when the drop actually fires.
// migrateAggregates returns true if it dropped the aggregates for a recreate (so the caller
// backfills the rebuilt views).
func (s *PGStore) migrateAggregates(ctx context.Context) (bool, error) {
	var hasView bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('samples_hourly') IS NOT NULL`).Scan(&hasView); err != nil {
		return false, fmt.Errorf("pgstore: check aggregate: %w", err)
	}
	if !hasView {
		return false, nil
	}
	var hasVantage, hasMedianRounds bool
	if err := s.pool.QueryRow(ctx, `
		SELECT bool_or(column_name='vantage'), bool_or(column_name='median_rounds')
		  FROM information_schema.columns WHERE table_name='samples_hourly'`).Scan(&hasVantage, &hasMedianRounds); err != nil {
		return false, fmt.Errorf("pgstore: check aggregate columns: %w", err)
	}
	if hasVantage && hasMedianRounds {
		return false, nil
	}
	slog.Warn("pgstore: rebuilding continuous aggregates to match the current definition " +
		"(vantage dimension and/or median_rounds); daily rollup buckets older than the raw " +
		"retention window cannot be rebuilt and will be lost (one-time migration)")
	for _, q := range []string{
		`DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE`,
		`DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return false, fmt.Errorf("pgstore: migrate aggregates: %w", err)
		}
	}
	return true, nil
}

// nanToNil converts NaN -> nil so lost/median gaps are stored as SQL NULL.
func nanToNil(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

func centeredToDB(c []float64) []*float64 {
	out := make([]*float64, len(c))
	for i, v := range c {
		out[i] = nanToNil(v)
	}
	return out
}

func dbToCentered(a []*float64) []float64 {
	out := make([]float64, len(a))
	for i, p := range a {
		if p == nil {
			out[i] = math.NaN()
		} else {
			out[i] = *p
		}
	}
	return out
}

// drainBatch runs each queued statement in br (calling onRow with the 0-based index and that
// statement's RowsAffected), then finalizes the batch, returning the first error. Close is ALWAYS
// called so the pooled connection is released — and its error is checked and returned when the
// per-row loop otherwise succeeded, because pgx runs SendBatch in an implicit transaction and
// reports finalization/commit errors from Close AFTER the last CommandComplete. Without that
// check a batch that failed to commit could be reported as durable, breaking the store-before-
// alert guarantee (CODE_REVIEW: batch finalization). Callers must treat any error as "nothing
// persisted" and (for ingest) retry, relying on the ON CONFLICT idempotency.
func drainBatch(br pgx.BatchResults, n int, onRow func(i int, rowsAffected int64)) error {
	var loopErr error
	for i := 0; i < n; i++ {
		tag, err := br.Exec()
		if err != nil {
			loopErr = err
			break
		}
		if onRow != nil {
			onRow(i, tag.RowsAffected())
		}
	}
	closeErr := br.Close()
	if loopErr != nil {
		return loopErr
	}
	return closeErr
}

// buildBatch turns outcomes into an idempotent insert batch. Shared by the
// fire-and-forget local Add and the error-returning AddResults.
func buildBatch(outcomes []scheduler.Outcome) *pgx.Batch {
	batch := &pgx.Batch{}
	for _, o := range outcomes {
		var errText *string
		if o.Err != nil {
			t := o.Err.Error()
			errText = &t
		}
		batch.Queue(
			`INSERT INTO samples
			   (ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 ON CONFLICT (target, vantage, ts) DO NOTHING`,
			o.When.UTC(), o.Target.Name, o.ProbeName, o.Target.Host, store.VantageOf(o),
			o.Computed.Pings, o.Computed.Loss, nanToNil(o.Computed.Median),
			centeredToDB(o.Computed.Centered), errText,
			float64(o.Duration.Microseconds())/1000.0,
		)
	}
	return batch
}

func (s *PGStore) Add(outcomes []scheduler.Outcome) {
	if len(outcomes) == 0 {
		return
	}
	// Bound the write with a fresh timeout context, NOT the process context: on a graceful
	// shutdown the process context is already cancelled while the dispatcher drains in-flight
	// probes, whose completed rounds reach Add. Deriving the deadline from the process context
	// cancelled those final writes immediately, losing the last local rounds (CODE_REVIEW #10 /
	// P2-10). writeTimeout still bounds a stalled DB, and the pool is closed only after the
	// drain (disp.Wait) returns, so a drained write has time to land.
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	br := s.pool.SendBatch(ctx, buildBatch(outcomes))
	// Checking the finalization (Close) error too, so a local round reported as written but not
	// committed is surfaced through the same failure metric/handler (CODE_REVIEW: batch finalization).
	if err := drainBatch(br, len(outcomes), nil); err != nil {
		s.writeFails.Add(1)
		s.onErr(fmt.Errorf("pgstore: insert: %w", err))
	}
}

// AddResults persists an agent-ingested batch, returning the first write error so
// the ingest handler can answer 503 and the agent can retain + retry — the caller-
// visible feedback the fire-and-forget Add deliberately does not give the local
// collector. Idempotent via ON CONFLICT (target,vantage,ts) DO NOTHING.
//
// It returns the subset of outcomes that were NEWLY inserted: each queued INSERT reports
// RowsAffected 1 when it stored a row and 0 when it conflicted, so the caller can evaluate
// alerts over only the fresh rounds. Without this, a replayed round (an HTTP retry, or the
// deliberate resend when a split batch's later half fails transiently) would be a no-op in
// the table yet still re-advance alert hysteresis — a false FIRING (CODE_REVIEW #4/replay).
func (s *PGStore) AddResults(ctx context.Context, outcomes []scheduler.Outcome) ([]scheduler.Outcome, error) {
	if len(outcomes) == 0 {
		return nil, nil
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	br := s.pool.SendBatch(wctx, buildBatch(outcomes))
	var inserted []scheduler.Outcome
	// drainBatch also checks the batch's finalization (Close) error — pgx runs SendBatch in an
	// implicit transaction and reports a commit/finalization failure there, after the per-row
	// command tags — so a batch that didn't actually commit is never reported as durable. On any
	// error we return no inserted rows: the handler answers 503, the agent retries, and alerts
	// are not evaluated over unpersisted data (CODE_REVIEW: batch finalization).
	err := drainBatch(br, len(outcomes), func(i int, rows int64) {
		if rows == 1 { // 0 => ON CONFLICT DO NOTHING skipped a duplicate
			inserted = append(inserted, outcomes[i])
		}
	})
	if err != nil {
		s.writeFails.Add(1)
		return nil, fmt.Errorf("pgstore: ingest write: %w", err)
	}
	return inserted, nil
}

// WriteMetrics appends the persistent-write health counter in Prometheus text format.
// Wired into /metrics via api.Server.ExtraMetrics so a database that is rejecting or
// timing out writes is scrapeable, not merely logged (CODE_REVIEW #5).
func (s *PGStore) WriteMetrics(b *strings.Builder) {
	fmt.Fprintf(b, "# HELP heliograph_store_write_failures_total Sample-batch writes that failed (a persistent store error or write deadline).\n# TYPE heliograph_store_write_failures_total counter\nheliograph_store_write_failures_total %d\n", s.writeFails.Load())
}

func (s *PGStore) Keys() ([]string, error) {
	rows, err := s.pool.Query(s.ctx, `SELECT target FROM samples GROUP BY target ORDER BY min(ts)`)
	if err != nil {
		s.onErr(err)
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			s.onErr(err)
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		s.onErr(err)
		return nil, err
	}
	return keys, nil
}

// Latest returns the target's most recent LOCAL round — warm-start and any other
// caller of the base Store interface must never seed from a remote (agent) vantage.
// Agent-ingested rounds from other vantages are reached via LatestAll(vantage).
func (s *PGStore) Latest(key string) (scheduler.Outcome, bool) {
	row := s.pool.QueryRow(s.ctx,
		`SELECT ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 AND vantage=$2 ORDER BY ts DESC LIMIT 1`, key, store.DefaultVantage)
	o, err := scanOutcome(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.onErr(err)
		}
		return scheduler.Outcome{}, false
	}
	return o, true
}

// History returns the target's recent LOCAL rounds (the hub's own vantage), capped to
// histCap. It delegates to HistoryVantage so the local and per-vantage paths share one query.
func (s *PGStore) History(key string) ([]scheduler.Outcome, error) {
	return s.HistoryVantage(s.ctx, key, store.DefaultVantage)
}

// HistoryVantage returns the target's most recent rounds for a specific vantage, capped to
// histCap — the vantage-aware form History delegates to, and the no-window /api/series read
// so ?vantage=nyc returns nyc's recent rounds rather than local (CODE_REVIEW #7 / P2-7).
func (s *PGStore) HistoryVantage(ctx context.Context, target, vantage string) ([]scheduler.Outcome, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 AND vantage=$2 ORDER BY ts DESC LIMIT $3`, target, store.VantageOrDefault(vantage), s.histCap)
	if err != nil {
		s.onErr(err)
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		s.onErr(err)
		return nil, err
	}
	// query is DESC; History returns oldest->newest to match MemStore.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanOutcome(row scannable) (scheduler.Outcome, error) {
	var (
		o          scheduler.Outcome
		median     *float64
		centered   []*float64
		errText    *string
		durationMs *float64
	)
	if err := row.Scan(
		&o.When, &o.Target.Name, &o.ProbeName, &o.Target.Host, &o.Vantage,
		&o.Computed.Pings, &o.Computed.Loss, &median, &centered, &errText, &durationMs,
	); err != nil {
		return o, err
	}
	if durationMs != nil {
		o.Duration = msToDuration(*durationMs)
	}
	o.Computed.Median = math.NaN()
	if median != nil {
		o.Computed.Median = *median
	}
	o.Computed.Centered = dbToCentered(centered)
	o.Computed.Sorted = sortedNonNaN(o.Computed.Centered)
	if errText != nil {
		o.Err = errors.New(*errText)
	}
	return o, nil
}

// msToDuration converts stored fractional milliseconds back to a Duration. It is
// the inverse of the insert's float64(Duration.Microseconds())/1000.0.
func msToDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}

func sortedNonNaN(c []float64) []float64 {
	out := make([]float64, 0, len(c))
	for _, v := range c {
		if !math.IsNaN(v) {
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}

// Rollup returns the downsampled buckets for a target from the continuous
// aggregate selected by resolution ("1h" default, or "1d"). Requires
// EnableDownsampling to have run.
func (s *PGStore) Rollup(ctx context.Context, target, vantage, resolution string, since, until time.Time) ([]store.RollupPoint, error) {
	// Map resolution to a fixed view name here — never interpolate caller input
	// into the query. Unknown resolutions are a programming error (the API
	// validates the param before reaching this point).
	var view string
	switch resolution {
	case "", "1h":
		view = "samples_hourly"
	case "1d":
		view = "samples_daily"
	default:
		return nil, fmt.Errorf("pgstore: unknown rollup resolution %q", resolution)
	}
	// Bound to buckets in [since, until]; a zero since means "from the start" and a zero
	// until means "through now", so a long-range view doesn't transfer every retained
	// bucket and a drag-zoom fetches only its sub-range. Placeholders are numbered from
	// the args length so either bound can be absent.
	q := `SELECT bucket, median_avg, median_min, median_max, loss_frac, rounds, median_rounds
	        FROM ` + view + ` WHERE target=$1 AND vantage=$2`
	args := []any{target, store.VantageOrDefault(vantage)}
	if !since.IsZero() {
		args = append(args, since.UTC())
		q += fmt.Sprintf(` AND bucket >= $%d`, len(args))
	}
	if !until.IsZero() {
		args = append(args, until.UTC())
		q += fmt.Sprintf(` AND bucket <= $%d`, len(args))
	}
	q += ` ORDER BY bucket`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		// The continuous aggregate was never created (started without -downsample):
		// report it as "not available" so the API answers 501, not a generic 503.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return nil, store.ErrRollupUnavailable
		}
		return nil, err
	}
	defer rows.Close()
	var out []store.RollupPoint
	for rows.Next() {
		var (
			p                      store.RollupPoint
			mAvg, mMin, mMax, loss *float64
		)
		if err := rows.Scan(&p.Bucket, &mAvg, &mMin, &mMax, &loss, &p.Rounds, &p.MedianRounds); err != nil {
			return nil, err
		}
		p.MedianAvg = nanIfNil(mAvg)
		p.MedianMin = nanIfNil(mMin)
		p.MedianMax = nanIfNil(mMax)
		p.LossFrac = nanIfNil(loss)
		out = append(out, p)
	}
	return out, rows.Err()
}

func nanIfNil(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

// Availability aggregates availability over the window [cutoff, now) directly in
// SQL, so it covers the full requested window regardless of the History cap.
func (s *PGStore) Availability(ctx context.Context, target, vantage string, cutoff time.Time, maxLossPct *float64) (store.AvailabilityStat, error) {
	// "up" is at least one reply by default; a maxLossPct tightens it to a loss
	// ceiling. Both forms are fixed strings chosen here (no user input), with the
	// threshold passed as a bound parameter.
	upCond := "loss < pings"
	args := []any{target, store.VantageOrDefault(vantage), cutoff.UTC()}
	if maxLossPct != nil {
		upCond = "loss * 100.0 <= $4 * pings"
		args = append(args, *maxLossPct)
	}
	q := `SELECT count(*),
	             count(*) FILTER (WHERE ` + upCond + `),
	             coalesce(sum(loss::float / NULLIF(pings,0) * 100), 0),
	             min(ts), max(ts)
	        FROM samples WHERE target=$1 AND vantage=$2 AND ts >= $3`
	var (
		st             store.AvailabilityStat
		oldest, latest *time.Time
	)
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&st.Measured, &st.Up, &st.SumLossPct, &oldest, &latest); err != nil {
		return store.AvailabilityStat{}, err
	}
	if oldest != nil {
		st.Oldest = *oldest
	}
	if latest != nil {
		st.Latest = *latest
	}
	return st, nil
}

// maxRangeRounds bounds a windowed single-target series read. It comfortably covers
// the largest documented raw range (the 30h drill-down) even at the 1s minimum step —
// 30h/1s = 108,000 rounds — so that view never silently truncates (the old 20,000 cap
// dropped ~1h53m of a 30h view at a 5s step). It stays a backstop against a pathological
// long window (raw retention caps the absolute at 30 days); if a query still exceeds it,
// the newest maxRangeRounds are returned and the truncation is logged, not silent. A var
// so tests can lower it (CODE_REVIEW #3).
var maxRangeRounds = 150_000

// HistorySince returns the target's rounds at or after cutoff, oldest->newest,
// unbounded by histCap — so a long raw range (e.g. the 30h drill-down) reads the
// whole window instead of just the last histCap rounds.
func (s *PGStore) HistorySince(ctx context.Context, target, vantage string, cutoff time.Time) ([]scheduler.Outcome, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 AND vantage=$2 AND ts >= $3 ORDER BY ts DESC LIMIT $4`,
		target, store.VantageOrDefault(vantage), cutoff.UTC(), maxRangeRounds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == maxRangeRounds {
		slog.Warn("pgstore: windowed series hit the row cap; oldest rounds omitted — narrow the window or raise the step",
			"target", target, "cap", maxRangeRounds, "cutoff", cutoff.UTC())
	}
	// query is DESC; return oldest->newest to match History/MemStore.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// HistoryBetween returns the target's rounds in [from, to], oldest->newest, unbounded by
// histCap — the drag-zoom path fetching an arbitrary historical sub-range. Same
// row-cap backstop as HistorySince.
func (s *PGStore) HistoryBetween(ctx context.Context, target, vantage string, from, to time.Time) ([]scheduler.Outcome, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 AND vantage=$2 AND ts >= $3 AND ts <= $4 ORDER BY ts DESC LIMIT $5`,
		target, store.VantageOrDefault(vantage), from.UTC(), to.UTC(), maxRangeRounds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == maxRangeRounds {
		slog.Warn("pgstore: windowed series hit the row cap; oldest rounds omitted — narrow the window or raise the step",
			"target", target, "cap", maxRangeRounds, "from", from.UTC(), "to", to.UTC())
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// LatestAll returns every target's most recent outcome in one query (DISTINCT ON),
// so the live endpoints don't fan out to one Latest per target. A query/scan error
// is returned (not swallowed) so the API can answer 503 instead of a false-empty
// success.
func (s *PGStore) LatestAll(vantage string) (map[string]scheduler.Outcome, error) {
	rows, err := s.pool.Query(s.ctx,
		`SELECT DISTINCT ON (target) ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE vantage=$1 ORDER BY target, ts DESC`, store.VantageOrDefault(vantage))
	if err != nil {
		s.onErr(err)
		return nil, err
	}
	defer rows.Close()
	out := map[string]scheduler.Outcome{}
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return nil, err
		}
		out[o.Target.Name] = o
	}
	if err := rows.Err(); err != nil {
		s.onErr(err)
		return nil, err
	}
	return out, nil
}

// AvailabilityAll aggregates every target over [cutoff, now) in one grouped query —
// the bulk form of Availability, so /api/sla is a single scan, not one per target.
func (s *PGStore) AvailabilityAll(ctx context.Context, vantage string, cutoff time.Time, maxLossPct *float64) (map[string]store.AvailabilityStat, error) {
	upCond := "loss < pings"
	args := []any{store.VantageOrDefault(vantage), cutoff.UTC()}
	if maxLossPct != nil {
		upCond = "loss * 100.0 <= $3 * pings"
		args = append(args, *maxLossPct)
	}
	q := `SELECT target, count(*),
	             count(*) FILTER (WHERE ` + upCond + `),
	             coalesce(sum(loss::float / NULLIF(pings,0) * 100), 0),
	             min(ts), max(ts)
	        FROM samples WHERE vantage=$1 AND ts >= $2 GROUP BY target`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]store.AvailabilityStat{}
	for rows.Next() {
		var (
			target         string
			st             store.AvailabilityStat
			oldest, latest *time.Time
		)
		if err := rows.Scan(&target, &st.Measured, &st.Up, &st.SumLossPct, &oldest, &latest); err != nil {
			return nil, err
		}
		if oldest != nil {
			st.Oldest = *oldest
		}
		if latest != nil {
			st.Latest = *latest
		}
		out[target] = st
	}
	return out, rows.Err()
}

var _ store.Store = (*PGStore)(nil)

// maxSeriesAllPerTarget caps how many of the newest rounds the bulk grid read returns
// PER TARGET. The endpoint allows a window up to 48h, so without a bound a short probe
// step across many targets could multiply into a multi-million-row response. It
// comfortably covers the grid's documented 3h window even at the 1s minimum step
// (3h/1s = 10,800), so the live grid never truncates — it only bounds a pathological
// request, keeping every target represented (unlike a plain LIMIT, which would drop
// whole trailing targets). A var so tests can lower it (CODE_REVIEW #8).
var maxSeriesAllPerTarget = 20_000

// SeriesAll returns every target's rounds strictly after cutoff in one query,
// grouped by target and oldest->newest (ts ascending) — the bulk, incremental read
// behind the Graphs grid: one query per refresh, and with cutoff = the client's
// watermark it returns only rounds newer than that instead of the whole window each
// tick. Targets with no rounds after cutoff are absent from the map. Each target is
// capped to its newest maxSeriesAllPerTarget rounds. A query/scan error is returned
// (not swallowed) so the API answers 503, not a false-empty grid.
func (s *PGStore) SeriesAll(ctx context.Context, vantage string, cutoff time.Time, maxTotal int) (map[string][]scheduler.Outcome, bool, error) {
	v := store.VantageOrDefault(vantage)
	// Bound the query fairly BEFORE it runs: cap each target at min(maxSeriesAllPerTarget,
	// maxTotal/targets), so the server never sorts an unbounded windowed set and the client never
	// builds an unbounded map for a many-target request, while every target keeps its newest rounds
	// (CODE_REVIEW M5 — the deeper, store-query bound on top of the per-target cap). The distinct-
	// target count and the fetch are separate reads, so the split is approximate under concurrent
	// writes — fine for a DoS bound.
	perTarget := maxSeriesAllPerTarget
	globalTruncated := false
	if maxTotal > 0 {
		var nTargets int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(DISTINCT target) FROM samples WHERE vantage=$1 AND ts > $2`, v, cutoff.UTC()).Scan(&nTargets); err != nil {
			s.onErr(err)
			return nil, false, err
		}
		if nTargets > 0 {
			if fair := maxTotal / nTargets; fair < perTarget {
				perTarget = fair
				if perTarget < 1 {
					perTarget = 1
				}
				globalTruncated = true
			}
		}
	}
	// row_number() per target keeps the newest N of each (so no target is dropped whole,
	// as a bare LIMIT would); the outer query then returns them oldest->newest.
	rows, err := s.pool.Query(ctx,
		`SELECT ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM (
		     SELECT *, row_number() OVER (PARTITION BY target ORDER BY ts DESC) AS rn
		       FROM samples WHERE vantage=$1 AND ts > $2
		   ) q
		   WHERE rn <= $3
		   ORDER BY target, ts`, v, cutoff.UTC(), perTarget)
	if err != nil {
		s.onErr(err)
		return nil, false, err
	}
	defer rows.Close()
	out := map[string][]scheduler.Outcome{}
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return nil, false, err
		}
		out[o.Target.Name] = append(out[o.Target.Name], o)
	}
	if err := rows.Err(); err != nil {
		s.onErr(err)
		return nil, false, err
	}
	perTargetTruncated := false
	for _, hist := range out {
		if len(hist) == perTarget {
			perTargetTruncated = true
			break
		}
	}
	truncated := globalTruncated || perTargetTruncated
	if truncated {
		slog.Warn("pgstore: bulk series truncated; oldest rounds omitted — narrow the window or raise the step",
			"per_target_cap", perTarget, "global_budget", maxTotal, "global_bound_hit", globalTruncated)
	}
	return out, truncated, nil
}

var _ interface {
	store.Rollupper
	store.Availabler
	store.RangeHistorier
	store.RecentHistorier
	store.LatestAller
	store.AvailabilityAller
	store.SeriesAller
	store.ResultIngester
} = (*PGStore)(nil)
