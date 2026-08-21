package pgstore

import (
	"context"
	"strings"
	"testing"
)

// caggHasColumn reports whether a continuous aggregate / view exposes a column.
func caggHasColumn(t *testing.T, s *PGStore, view, col string) bool {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT bool_or(column_name=$2) FROM information_schema.columns
		  WHERE table_schema=current_schema() AND table_name=$1`, view, col).Scan(&ok); err != nil {
		t.Fatalf("column check %s.%s: %v", view, col, err)
	}
	return ok
}

func readAggregateOIDs(t *testing.T, s *PGStore) map[string]uint32 {
	t.Helper()
	ctx := context.Background()
	oids := make(map[string]uint32, len(aggregateSpecs))
	for _, spec := range aggregateSpecs {
		var oid uint32
		if err := s.pool.QueryRow(ctx,
			`SELECT to_regclass(format('%I.%I', current_schema(), $1::text))::oid`, spec.name).
			Scan(&oid); err != nil {
			t.Fatalf("read %s OID: %v", spec.name, err)
		}
		oids[spec.name] = oid
	}
	return oids
}

func TestAggregateDefinitionCurrent(t *testing.T) {
	hourly := `
		SELECT time_bucket('01:00:00'::interval, samples.ts) AS bucket,
		       samples.target, samples.vantage, samples.metric,
		       avg(samples.median_seconds) AS median_avg,
		       min(samples.median_seconds) AS median_min,
		       max(samples.median_seconds) AS median_max,
		       avg((samples.loss)::double precision /
		           (NULLIF(samples.pings, 0))::double precision) AS loss_frac,
		       count(*) AS rounds,
		       count(samples.median_seconds) AS median_rounds
		FROM samples
		GROUP BY (time_bucket('01:00:00'::interval, samples.ts)),
		         samples.target, samples.vantage, samples.metric;`
	if !aggregateDefinitionCurrent(hourly, "hour") {
		t.Fatal("current Timescale-style hourly definition was rejected")
	}
	qualified := strings.ReplaceAll(hourly, "samples.", `"monitoring"."samples".`)
	if !aggregateDefinitionCurrent(qualified, "hour") {
		t.Fatal("schema/table-qualified hourly definition was rejected")
	}

	daily := `
		SELECT time_bucket('1 day', ts) AS bucket, target, vantage, metric,
		       avg(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		       max(median_seconds) AS median_max,
		       avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
		       count(*) AS rounds, count(median_seconds) AS median_rounds
		FROM samples GROUP BY time_bucket('1 day', ts), target, vantage, metric`
	if !aggregateDefinitionCurrent(daily, "day") {
		t.Fatal("current source-style daily definition was rejected")
	}

	for name, definition := range map[string]string{
		"wrong bucket":         strings.Replace(hourly, "01:00:00", "02:00:00", 2),
		"wrong median average": strings.Replace(hourly, "avg(samples.median_seconds) AS median_avg", "max(samples.median_seconds) AS median_avg", 1),
		"wrong loss function":  strings.Replace(hourly, "avg((samples.loss)", "sum((samples.loss)", 1),
		"wrong median count":   strings.Replace(hourly, "count(samples.median_seconds) AS median_rounds", "count(*) AS median_rounds", 1),
		"filtered rows":        strings.Replace(hourly, "FROM samples", "FROM samples WHERE samples.vantage = 'local'", 1),
		"missing vantage group": strings.Replace(hourly,
			"samples.target, samples.vantage, samples.metric;", "samples.target, samples.metric;", 1),
		"missing metric group": strings.Replace(hourly,
			"samples.target, samples.vantage, samples.metric;", "samples.target, samples.vantage;", 1),
		"target expression group": strings.Replace(hourly,
			"samples.target, samples.vantage, samples.metric;", "lower(samples.target), samples.vantage, samples.metric;", 1),
		"extra grouping dimension": strings.Replace(hourly,
			"samples.target, samples.vantage, samples.metric;", "samples.probe, samples.target, samples.vantage, samples.metric;", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if aggregateDefinitionCurrent(definition, "hour") {
				t.Error("semantically stale definition was accepted")
			}
		})
	}
}

// A filtered aggregate can expose the exact expected columns, buckets, grouping, and aggregate
// expressions while silently omitting source rows. An unversioned/restored filtered definition
// must therefore be rebuilt rather than adopted and marked authoritative.
func TestMigrateAggregatesRebuildsFilteredDefinition(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, q := range []string{
		`DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE`,
		`DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("drop caggs: %v", err)
		}
	}
	if _, err := s.pool.Exec(ctx, downsampleStmts[0]); err != nil {
		t.Fatalf("create current hourly: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		CREATE MATERIALIZED VIEW samples_daily
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket('1 day', ts) AS bucket, target, vantage,
		       avg(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		       max(median_seconds) AS median_max,
		       avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
		       count(*) AS rounds, count(median_seconds) AS median_rounds
		FROM samples WHERE vantage = 'local'
		GROUP BY bucket, target, vantage WITH NO DATA`); err != nil {
		t.Fatalf("create filtered daily: %v", err)
	}
	if _, err := s.pool.Exec(ctx, aggregateVersionSchema); err != nil {
		t.Fatalf("create version catalog: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM heliograph_aggregate_versions WHERE aggregate_name = ANY($1::text[])`,
		[]string{"samples_hourly", "samples_daily"}); err != nil {
		t.Fatalf("clear version markers: %v", err)
	}

	before := readAggregateOIDs(t, s)
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	after := readAggregateOIDs(t, s)
	if after["samples_daily"] == before["samples_daily"] {
		t.Errorf("filtered samples_daily was adopted instead of rebuilt: OID stayed %d", after["samples_daily"])
	}
}

// A current pair created by an older Heliograph has no application version marker. Startup must
// validate and adopt those actual Timescale catalog definitions without dropping the relations —
// a false negative here would unnecessarily destroy rollup history older than raw retention.
func TestMigrateAggregatesAdoptsCurrentUnversionedViews(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, q := range []string{
		`DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE`,
		`DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("drop caggs: %v", err)
		}
	}
	for _, i := range []int{0, 2} { // the hourly and daily CREATE statements
		if _, err := s.pool.Exec(ctx, downsampleStmts[i]); err != nil {
			t.Fatalf("create current aggregate: %v", err)
		}
	}
	if _, err := s.pool.Exec(ctx, aggregateVersionSchema); err != nil {
		t.Fatalf("create version catalog: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM heliograph_aggregate_versions WHERE aggregate_name = ANY($1::text[])`,
		[]string{"samples_hourly", "samples_daily"}); err != nil {
		t.Fatalf("clear version markers: %v", err)
	}

	before := readAggregateOIDs(t, s)
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	after := readAggregateOIDs(t, s)
	for _, spec := range aggregateSpecs {
		if after[spec.name] != before[spec.name] {
			t.Errorf("%s was rebuilt despite a current definition: OID %d -> %d", spec.name, before[spec.name], after[spec.name])
		}
		current, versioned, err := s.aggregateCurrent(ctx, spec)
		if err != nil {
			t.Fatalf("aggregateCurrent(%s): %v", spec.name, err)
		}
		if !current || !versioned {
			t.Errorf("%s after adoption = current:%v versioned:%v, want true/true", spec.name, current, versioned)
		}
	}
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

// CODE_REVIEW L6: exposing all expected column names is not enough. A hand-created/future view
// can retain that surface while changing bucket width or aggregate expressions. The application
// version is tied to the relation OID, so this unversioned replacement must be inspected and the
// pair rebuilt before startup trusts it.
func TestMigrateAggregatesRebuildsSemanticDrift(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, q := range []string{
		`DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE`,
		`DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("drop caggs: %v", err)
		}
	}
	if _, err := s.pool.Exec(ctx, downsampleStmts[0]); err != nil {
		t.Fatalf("create current hourly: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		CREATE MATERIALIZED VIEW samples_daily
		WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		SELECT time_bucket('2 days', ts) AS bucket, target, vantage,
		       max(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		       max(median_seconds) AS median_max,
		       avg(loss::float / NULLIF(pings, 0)) AS loss_frac,
		       count(*) AS rounds, count(*) AS median_rounds
		FROM samples GROUP BY bucket, target, vantage WITH NO DATA`); err != nil {
		t.Fatalf("create semantic-drift daily: %v", err)
	}
	if _, err := s.pool.Exec(ctx, aggregateVersionSchema); err != nil {
		t.Fatalf("create version catalog: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM heliograph_aggregate_versions WHERE aggregate_name = ANY($1::text[])`,
		[]string{"samples_hourly", "samples_daily"}); err != nil {
		t.Fatalf("clear version markers: %v", err)
	}
	for _, col := range []string{"vantage", "median_rounds"} {
		if !caggHasColumn(t, s, "samples_daily", col) {
			t.Fatalf("precondition: drifted daily must still expose %s", col)
		}
	}
	before := readAggregateOIDs(t, s)

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	after := readAggregateOIDs(t, s)
	for _, spec := range aggregateSpecs {
		if after[spec.name] == before[spec.name] {
			t.Errorf("%s was marked without being rebuilt: OID stayed %d", spec.name, after[spec.name])
		}
		var definition string
		if err := s.pool.QueryRow(ctx, `
			SELECT view_definition
			  FROM timescaledb_information.continuous_aggregates
			 WHERE view_schema = current_schema() AND view_name = $1`, spec.name).
			Scan(&definition); err != nil {
			t.Fatalf("read rebuilt %s definition: %v", spec.name, err)
		}
		if !aggregateDefinitionCurrent(definition, spec.bucket) {
			t.Errorf("rebuilt %s catalog definition is not current: %s", spec.name, definition)
		}
		current, versioned, err := s.aggregateCurrent(ctx, spec)
		if err != nil {
			t.Fatalf("aggregateCurrent(%s): %v", spec.name, err)
		}
		if !current || !versioned {
			t.Errorf("%s after rebuild = current:%v versioned:%v, want true/true", spec.name, current, versioned)
		}
	}
}
