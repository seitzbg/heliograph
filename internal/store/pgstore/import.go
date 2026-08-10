package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// ImportSamples bulk-inserts historical rows idempotently (ON CONFLICT
// (target,vantage,ts) DO NOTHING, matching the live-write shape), so re-running an
// import after a partial failure never duplicates rows. Imported rows carry no raw
// RTT distribution (rtts_seconds = '{}', NOT NULL) and no err/duration (NULL) — RRD
// history only has the consolidated median/loss, not the per-round samples. Rows are
// sent in bounded batches; the return value is the number of rows actually inserted
// (rows already present count 0, not an error).
func (s *PGStore) ImportSamples(ctx context.Context, rows []ImportRow) (int64, error) {
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
	defer br.Close()
	var total int64
	for range rows {
		tag, err := br.Exec()
		if err != nil {
			return total, fmt.Errorf("pgstore: import insert: %w", err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// AggregatesExist reports whether the hourly (and, by construction, daily) continuous
// aggregates are present — exposing the existing aggregateExists check so the
// importer CLI can tell the operator to run `smoked -downsample` first rather than
// silently skipping the RefreshAggregates step.
func (s *PGStore) AggregatesExist(ctx context.Context) (bool, error) {
	return s.aggregateExists(ctx)
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
func (s *PGStore) RefreshAggregates(ctx context.Context, from, until time.Time) error {
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
// SQLSTATE 55P03 (lock_not_available): a smokeping-modern DB whose background
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
