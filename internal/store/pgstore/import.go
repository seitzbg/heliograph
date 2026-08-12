package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/seitzbg/heliograph/internal/config"
)

// importBatchSize bounds how many rows a single pgx.Batch/SendBatch round-trip
// carries. The importer can hand ImportSamples years of RRD history at once
// (potentially hundreds of thousands of rows); one unbounded batch would hold a
// single connection and a huge in-memory command queue for the whole import.
const importBatchSize = 1000

// ImportRow is one historical measurement to backfill (vantage is always "local" —
// RRD history predates federation, so it is always the hub's own past data).
type ImportRow struct {
	TS                  time.Time
	Target, Probe, Host string
	Pings, Loss         int
	MedianSeconds       float64
}

// validateImportRow is ImportSamples' defense-in-depth backstop against a semantically
// invalid historical row reaching the samples table (Finding #5). The CLI importer
// (cmd/smoked's runHistory) is expected to have already validated every target's
// resolved pings and every extracted sample's loss before ever calling here — this
// exists so no caller, present or future, can slip an invalid row past ImportSamples.
// pings<1 would make raw LossFraction read as a false 0% and NULLIF(pings,0) blank out
// the aggregate loss (see the samples_hourly/samples_daily view definitions); pings
// beyond config.MaxPings risks overflowing the smallint column; loss<0 or loss>pings is
// nonsensical on its face.
func validateImportRow(r ImportRow) error {
	switch {
	case r.Pings < 1:
		return fmt.Errorf("pgstore: import row %s@%s: pings must be >= 1, got %d", r.Target, r.TS, r.Pings)
	case r.Pings > config.MaxPings:
		return fmt.Errorf("pgstore: import row %s@%s: pings must be <= %d, got %d", r.Target, r.TS, config.MaxPings, r.Pings)
	case r.Loss < 0:
		return fmt.Errorf("pgstore: import row %s@%s: loss must be >= 0, got %d", r.Target, r.TS, r.Loss)
	case r.Loss > r.Pings:
		return fmt.Errorf("pgstore: import row %s@%s: loss (%d) must be <= pings (%d)", r.Target, r.TS, r.Loss, r.Pings)
	}
	return nil
}

// ImportSamples bulk-inserts historical rows idempotently (ON CONFLICT
// (target,vantage,ts) DO NOTHING, matching the live-write shape), so re-running an
// import after a partial failure never duplicates rows. Imported rows carry no raw
// RTT distribution (rtts_seconds = '{}', NOT NULL) and no err/duration (NULL) — RRD
// history only has the consolidated median/loss, not the per-round samples. Rows are
// validated up front (validateImportRow) before any insert, then sent in bounded
// batches; the return value is the number of rows actually inserted (rows already
// present count 0, not an error).
func (s *PGStore) ImportSamples(ctx context.Context, rows []ImportRow) (int64, error) {
	// Validate the whole batch up front, before any INSERT: a bad row here should never
	// happen (the CLI importer filters first), so this backstop rejects the call outright
	// rather than silently dropping just the offending row — no partial/uncertain state.
	for _, r := range rows {
		if err := validateImportRow(r); err != nil {
			return 0, err
		}
	}
	var total int64
	for start := 0; start < len(rows); start += importBatchSize {
		end := start + importBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		n, err := s.importBatch(ctx, rows[start:end])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *PGStore) importBatch(ctx context.Context, rows []ImportRow) (int64, error) {
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(
			`INSERT INTO samples (ts,target,probe,host,vantage,pings,loss,median_seconds,rtts_seconds,err,duration_ms)
			 VALUES ($1,$2,$3,$4,'local',$5,$6,$7,'{}'::double precision[],NULL,NULL)
			 ON CONFLICT (target,vantage,ts) DO NOTHING`,
			r.TS.UTC(), r.Target, r.Probe, r.Host, r.Pings, r.Loss, nanToNil(r.MedianSeconds))
	}
	br := s.pool.SendBatch(ctx, batch)
	var total int64
	// drainBatch also checks the batch finalization error, so an unconfirmed/rolled-back import
	// batch isn't reported as inserted rows and then aggregate-refreshed (CODE_REVIEW: batch
	// finalization). On error the count is not trusted — the caller fails the import.
	if err := drainBatch(br, len(rows), func(_ int, ra int64) { total += ra }); err != nil {
		return 0, fmt.Errorf("pgstore: import insert: %w", err)
	}
	return total, nil
}

// AggregatesExist reports whether BOTH continuous aggregates the importer depends on —
// samples_hourly and samples_daily — are present, so `--history` refuses to write when
// either is missing rather than inserting raw rows and only then failing on the daily
// refresh. `--history` refreshes both views over the imported range, so both must exist
// before any insert; an interrupted downsampling init or a manually dropped daily view can
// leave hourly present and daily absent, which the earlier hourly-only check missed
// (CODE_REVIEW #6). The operator is told to run `smoked -downsample` first.
func (s *PGStore) AggregatesExist(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('samples_hourly') IS NOT NULL AND to_regclass('samples_daily') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("pgstore: check aggregates: %w", err)
	}
	return ok, nil
}

// RefreshAggregates materializes the hourly and daily continuous aggregates over
// [from, until] — the arbitrary-range generalization of backfillAggregates, which only
// ever covers the trailing policy windows (10d hourly / 30d daily) from now. The
// importer calls this after ImportSamples to materialize the just-inserted historical
// range, however old. until is capped at now()-1h: continuous aggregates only refresh
// closed, bucket-aligned ranges, and the last (still-open) hour bucket is left for the
// regular refresh policy, matching backfillAggregates' own end_offset. Must run outside
// an explicit transaction, like backfillAggregates (TimescaleDB requires CALL
// refresh_continuous_aggregate to not be inside one).
//
// Both bounds are widened out to whole UTC days before use: verified against a live
// TimescaleDB instance, `CALL refresh_continuous_aggregate(view, from, until)` can leave
// the bucket containing `until` entirely unmaterialized when `until` sits exactly at (or
// only seconds past) the newest raw sample it's meant to cover — the daily bucket comes
// back with zero rows, not a partial one. Rounding `from` down and `until` up to the
// next UTC-day boundary guarantees the daily aggregate's bucket (the coarser, binding
// one — see minRefreshSpan) at both ends is unambiguously covered, regardless of the
// exact half-open/closed semantics TimescaleDB applies internally. This only widens the
// requested range (never narrows it), so it's safe for every caller — including one that
// already asks for a generously-wide window. This is what review Finding #3's fix relies
// on to actually materialize old history into samples_daily, not just land it in raw
// `samples` (see cmd/smoked's DB-gated TestImportCmdHistoryMaterializesOldHistoryIntoDailyAggregate).
func (s *PGStore) RefreshAggregates(ctx context.Context, from, until time.Time) error {
	from = from.UTC().Truncate(24 * time.Hour)
	until = until.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	refreshCeiling := time.Now().Add(-time.Hour)
	if until.After(refreshCeiling) {
		until = refreshCeiling
	}
	if !until.After(from) {
		return nil // empty/inverted range — nothing to refresh
	}
	fromLit := from.UTC().Format(time.RFC3339)
	untilLit := until.UTC().Format(time.RFC3339)
	for _, view := range []string{"samples_hourly", "samples_daily"} {
		// CALL refresh_continuous_aggregate can't bind $-params via pgx's simple/extended
		// protocol in all paths (it is a procedure call, not a plain statement), so the
		// range is formatted as RFC3339 literals. Safe here: both timestamps are
		// computed above (time.Time -> time.RFC3339), never user-supplied text.
		q := fmt.Sprintf(`CALL refresh_continuous_aggregate('%s', '%s'::timestamptz, '%s'::timestamptz)`, view, fromLit, untilLit)
		if err := s.execWithRetry(ctx, q); err != nil {
			return fmt.Errorf("pgstore: refresh aggregates (%s): %w", view, err)
		}
	}
	return nil
}

// refreshRetryAttempts and refreshRetryDelay bound execWithRetry's backoff for
// SQLSTATE 55P03 (lock_not_available): a heliograph DB whose background
// continuous-aggregate refresh policy is active can hold the same cagg's refresh
// lock when the importer's own CALL runs at the same moment, and TimescaleDB
// aborts the loser rather than queuing it. This is expected contention on a live
// DB, not a real failure, so a few short retries let the importer's refresh land
// once the background job releases the lock.
const (
	refreshRetryAttempts = 4
	refreshRetryDelay    = 500 * time.Millisecond
)

// execWithRetry runs q, retrying up to refreshRetryAttempts times only when the
// error is TimescaleDB's 55P03 (lock_not_available) — a concurrent-refresh
// collision with the background refresh policy. Any other error (including a
// genuine 55P03 that never clears) returns immediately/after the last attempt.
func (s *PGStore) execWithRetry(ctx context.Context, q string) error {
	var lastErr error
	for attempt := 0; attempt < refreshRetryAttempts; attempt++ {
		_, err := s.pool.Exec(ctx, q)
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			return err
		}
		lastErr = err
		if attempt < refreshRetryAttempts-1 {
			time.Sleep(refreshRetryDelay)
		}
	}
	return lastErr
}
