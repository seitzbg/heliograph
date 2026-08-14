package pgstore

import (
	"context"
	"testing"
)

// caggHasColumn reports whether a continuous aggregate / view exposes a column.
func caggHasColumn(t *testing.T, s *PGStore, view, col string) bool {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT bool_or(column_name=$2) FROM information_schema.columns WHERE table_name=$1`, view, col).Scan(&ok); err != nil {
		t.Fatalf("column check %s.%s: %v", view, col, err)
	}
	return ok
}

// CODE_REVIEW M7: migrateAggregates must rebuild samples_daily when ITS shape is stale even if
// samples_hourly is already current. The prior check validated only hourly, so a current-hourly /
// stale-daily database (an older or hand-created daily missing the vantage dimension) silently kept
// the wrong daily view and the 400-day graph blended vantages / reported mis-weighted medians while
// startup looked healthy. This drives the exact current-hourly + stale-daily case.
func TestMigrateAggregatesRebuildsStaleDaily(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Start from a clean aggregate state (a prior test run may have left views).
	for _, q := range []string{
		`DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE`,
		`DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("drop caggs: %v", err)
		}
	}
	// A CURRENT hourly — has the vantage dimension and median_rounds.
	if _, err := s.pool.Exec(ctx, `
		CREATE MATERIALIZED VIEW samples_hourly
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket('1 hour', ts) AS bucket, target, vantage,
		       avg(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		       max(median_seconds) AS median_max,
		       avg(loss::float / NULLIF(pings,0)) AS loss_frac,
		       count(*) AS rounds, count(median_seconds) AS median_rounds
		FROM samples GROUP BY bucket, target, vantage WITH NO DATA`); err != nil {
		t.Fatalf("create current hourly: %v", err)
	}
	// ... paired with a STALE daily: the pre-federation shape, GROUP BY without vantage.
	if _, err := s.pool.Exec(ctx, `
		CREATE MATERIALIZED VIEW samples_daily
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket('1 day', ts) AS bucket, target,
		       avg(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		       max(median_seconds) AS median_max,
		       avg(loss::float / NULLIF(pings,0)) AS loss_frac,
		       count(*) AS rounds, count(median_seconds) AS median_rounds
		FROM samples GROUP BY bucket, target WITH NO DATA`); err != nil {
		t.Fatalf("create stale daily: %v", err)
	}
	// Precondition: hourly current, daily stale.
	if !caggHasColumn(t, s, "samples_hourly", "vantage") {
		t.Fatal("precondition: hourly must have the vantage dimension")
	}
	if caggHasColumn(t, s, "samples_daily", "vantage") {
		t.Fatal("precondition: daily must be stale (no vantage)")
	}

	// EnableDownsampling must detect the stale daily and rebuild the pair to the current shape.
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	if !caggHasColumn(t, s, "samples_daily", "vantage") {
		t.Error("samples_daily still lacks the vantage dimension — migrateAggregates did not rebuild a stale daily when hourly was current (CODE_REVIEW M7)")
	}
	if !caggHasColumn(t, s, "samples_daily", "median_rounds") {
		t.Error("samples_daily lacks median_rounds after the rebuild")
	}
	// Hourly must remain current after the pair rebuild.
	if !caggHasColumn(t, s, "samples_hourly", "vantage") || !caggHasColumn(t, s, "samples_hourly", "median_rounds") {
		t.Error("samples_hourly lost its shape after the rebuild")
	}
}
