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
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
)

type PGStore struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	histCap int
	onErr   func(error)
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
	vantage        text               NOT NULL DEFAULT 'master',
	pings          smallint           NOT NULL,
	loss           smallint           NOT NULL,
	median_seconds double precision,
	rtts_seconds   double precision[] NOT NULL,
	err            text,
	duration_ms    double precision
);
SELECT create_hypertable('samples', 'ts', if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS samples_target_ts ON samples (target, ts DESC);
`

func (s *PGStore) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
	return nil
}

// downsampling statements (each must run outside an explicit transaction).
var downsampleStmts = []string{
	`CREATE MATERIALIZED VIEW IF NOT EXISTS samples_hourly
	 WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
	 SELECT time_bucket('1 hour', ts) AS bucket,
	        target,
	        avg(median_seconds) AS median_avg,
	        min(median_seconds) AS median_min,
	        max(median_seconds) AS median_max,
	        avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
	        count(*) AS rounds
	 FROM samples
	 GROUP BY bucket, target
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
	        avg(median_seconds) AS median_avg,
	        min(median_seconds) AS median_min,
	        max(median_seconds) AS median_max,
	        avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
	        count(*) AS rounds
	 FROM samples
	 GROUP BY bucket, target
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
// avg/min/max + loss per target per bucket — the coarse tiers a long-range view
// reads, akin to SmokePing's RRAs; daily feeds the 400d range), their refresh
// policies, and a 30-day retention policy on the raw samples. Idempotent.
func (s *PGStore) EnableDownsampling(ctx context.Context) error {
	for _, q := range downsampleStmts {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("pgstore: downsampling: %w", err)
		}
	}
	return nil
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

func (s *PGStore) Add(outcomes []scheduler.Outcome) {
	if len(outcomes) == 0 {
		return
	}
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
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			o.When.UTC(), o.Target.Name, o.ProbeName, o.Target.Host, "master",
			o.Computed.Pings, o.Computed.Loss, nanToNil(o.Computed.Median),
			centeredToDB(o.Computed.Centered), errText,
			float64(o.Duration.Microseconds())/1000.0,
		)
	}
	br := s.pool.SendBatch(s.ctx, batch)
	defer br.Close()
	for range outcomes {
		if _, err := br.Exec(); err != nil {
			s.onErr(fmt.Errorf("pgstore: insert: %w", err))
			return
		}
	}
}

func (s *PGStore) Keys() []string {
	rows, err := s.pool.Query(s.ctx, `SELECT target FROM samples GROUP BY target ORDER BY min(ts)`)
	if err != nil {
		s.onErr(err)
		return nil
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			s.onErr(err)
			return keys
		}
		keys = append(keys, k)
	}
	return keys
}

func (s *PGStore) Latest(key string) (scheduler.Outcome, bool) {
	row := s.pool.QueryRow(s.ctx,
		`SELECT ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 ORDER BY ts DESC LIMIT 1`, key)
	o, err := scanOutcome(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.onErr(err)
		}
		return scheduler.Outcome{}, false
	}
	return o, true
}

func (s *PGStore) History(key string) []scheduler.Outcome {
	rows, err := s.pool.Query(s.ctx,
		`SELECT ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 ORDER BY ts DESC LIMIT $2`, key, s.histCap)
	if err != nil {
		s.onErr(err)
		return nil
	}
	defer rows.Close()
	var out []scheduler.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return out
		}
		out = append(out, o)
	}
	// query is DESC; History returns oldest->newest to match MemStore.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
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
		&o.When, &o.Target.Name, &o.ProbeName, &o.Target.Host,
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
func (s *PGStore) Rollup(ctx context.Context, target, resolution string) ([]store.RollupPoint, error) {
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
	rows, err := s.pool.Query(ctx,
		`SELECT bucket, median_avg, median_min, median_max, loss_frac, rounds
		   FROM `+view+` WHERE target=$1 ORDER BY bucket`, target)
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
		if err := rows.Scan(&p.Bucket, &mAvg, &mMin, &mMax, &loss, &p.Rounds); err != nil {
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
func (s *PGStore) Availability(ctx context.Context, target string, cutoff time.Time, maxLossPct *float64) (store.AvailabilityStat, error) {
	// "up" is at least one reply by default; a maxLossPct tightens it to a loss
	// ceiling. Both forms are fixed strings chosen here (no user input), with the
	// threshold passed as a bound parameter.
	upCond := "loss < pings"
	args := []any{target, cutoff.UTC()}
	if maxLossPct != nil {
		upCond = "loss * 100.0 <= $3 * pings"
		args = append(args, *maxLossPct)
	}
	q := `SELECT count(*),
	             count(*) FILTER (WHERE ` + upCond + `),
	             coalesce(sum(loss::float / NULLIF(pings,0) * 100), 0),
	             min(ts), max(ts)
	        FROM samples WHERE target=$1 AND ts >= $2`
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

// maxRangeRounds defensively bounds a windowed series read. It's far above any
// real drill-down (30h at a 60s step = 1800 rounds); the true bound is the raw
// retention policy (30 days). If a window somehow holds more, the most recent
// maxRangeRounds are returned rather than an unbounded result set.
const maxRangeRounds = 20000

// HistorySince returns the target's rounds at or after cutoff, oldest->newest,
// unbounded by histCap — so a long raw range (e.g. the 30h drill-down) reads the
// whole window instead of just the last histCap rounds.
func (s *PGStore) HistorySince(ctx context.Context, target string, cutoff time.Time) ([]scheduler.Outcome, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples WHERE target=$1 AND ts >= $2 ORDER BY ts DESC LIMIT $3`, target, cutoff.UTC(), maxRangeRounds)
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
	// query is DESC; return oldest->newest to match History/MemStore.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// LatestAll returns every target's most recent outcome in one query (DISTINCT ON),
// so the live endpoints don't fan out to one Latest per target.
func (s *PGStore) LatestAll() map[string]scheduler.Outcome {
	rows, err := s.pool.Query(s.ctx,
		`SELECT DISTINCT ON (target) ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err, duration_ms
		   FROM samples ORDER BY target, ts DESC`)
	if err != nil {
		s.onErr(err)
		return nil
	}
	defer rows.Close()
	out := map[string]scheduler.Outcome{}
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return out
		}
		out[o.Target.Name] = o
	}
	return out
}

// AvailabilityAll aggregates every target over [cutoff, now) in one grouped query —
// the bulk form of Availability, so /api/sla is a single scan, not one per target.
func (s *PGStore) AvailabilityAll(ctx context.Context, cutoff time.Time, maxLossPct *float64) (map[string]store.AvailabilityStat, error) {
	upCond := "loss < pings"
	args := []any{cutoff.UTC()}
	if maxLossPct != nil {
		upCond = "loss * 100.0 <= $2 * pings"
		args = append(args, *maxLossPct)
	}
	q := `SELECT target, count(*),
	             count(*) FILTER (WHERE ` + upCond + `),
	             coalesce(sum(loss::float / NULLIF(pings,0) * 100), 0),
	             min(ts), max(ts)
	        FROM samples WHERE ts >= $1 GROUP BY target`
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

var _ interface {
	Add([]scheduler.Outcome)
	Keys() []string
	Latest(string) (scheduler.Outcome, bool)
	History(string) []scheduler.Outcome
} = (*PGStore)(nil)

var _ interface {
	store.Rollupper
	store.Availabler
	store.RangeHistorier
	store.LatestAller
	store.AvailabilityAller
} = (*PGStore)(nil)
