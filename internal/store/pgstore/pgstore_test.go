package pgstore

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
)

// fakeBatch is a minimal pgx.BatchResults for exercising drainBatch's finalization handling
// without a database: it succeeds on every Exec (reporting rowsAffected) except an optional
// execAt-th call, and Close returns closeErr. Query/QueryRow are unused by drainBatch.
type fakeBatch struct {
	rowsAffected int64
	execAt       int // 1-based Exec call that fails; 0 = never
	execErr      error
	closeErr     error
	calls        int
	closed       bool
}

func (f *fakeBatch) Exec() (pgconn.CommandTag, error) {
	f.calls++
	if f.execAt > 0 && f.calls == f.execAt {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("INSERT 0 " + strconv.FormatInt(f.rowsAffected, 10)), nil
}
func (f *fakeBatch) Query() (pgx.Rows, error) { return nil, nil }
func (f *fakeBatch) QueryRow() pgx.Row        { return nil }
func (f *fakeBatch) Close() error             { f.closed = true; return f.closeErr }

// drainBatch must surface a batch finalization (Close) error even when every per-row Exec
// succeeded, and must always Close the batch — the store-before-alert guarantee depends on not
// reporting an unconfirmed batch as durable (CODE_REVIEW: batch finalization).
func TestDrainBatchSurfacesFinalizationError(t *testing.T) {
	fb := &fakeBatch{rowsAffected: 1, closeErr: errors.New("commit failed")}
	inserted := 0
	err := drainBatch(fb, 3, func(_ int, ra int64) {
		if ra == 1 {
			inserted++
		}
	})
	if err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("drainBatch must return the Close error, got %v", err)
	}
	if !fb.closed {
		t.Error("drainBatch must always Close the batch")
	}

	// A per-row Exec error takes precedence over Close, and Close still runs.
	fb2 := &fakeBatch{rowsAffected: 1, execAt: 2, execErr: errors.New("exec boom"), closeErr: errors.New("commit failed")}
	if err := drainBatch(fb2, 3, nil); err == nil || !strings.Contains(err.Error(), "exec boom") {
		t.Fatalf("loop Exec error should take precedence, got %v", err)
	}
	if !fb2.closed {
		t.Error("drainBatch must Close even after an Exec error")
	}

	// The happy path: all Execs succeed and Close succeeds -> no error.
	fb3 := &fakeBatch{rowsAffected: 1}
	if err := drainBatch(fb3, 2, nil); err != nil {
		t.Fatalf("clean batch should not error, got %v", err)
	}
}

// refreshAgg manually refreshes a continuous aggregate, retrying on TimescaleDB's
// "concurrent refresh" error (SQLSTATE 55P03): a background refresh-policy job can overlap
// a test's manual refresh. That is a test-timing race — not a real failure — so retry
// briefly rather than let it flake the suite (which must be reliable enough to gate CI).
func refreshAgg(ctx context.Context, t *testing.T, s *PGStore, view string) {
	t.Helper()
	var lastErr error
	for i := 0; i < 40; i++ {
		if _, err := s.pool.Exec(ctx, "CALL refresh_continuous_aggregate('"+view+"', NULL, NULL)"); err == nil {
			return
		} else {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				lastErr = err
				time.Sleep(50 * time.Millisecond)
				continue
			}
			t.Fatalf("refresh %s: %v", view, err)
		}
	}
	t.Fatalf("refresh %s: still blocked by a concurrent refresh after retries: %v", view, lastErr)
}

// testStore returns a PGStore connected to SMOKE_TEST_DSN with a freshly
// truncated samples table, skipping the test if the DSN is not set. For tests
// that don't need a specific histCap or a custom onErr.
func testStore(t *testing.T) *PGStore {
	t.Helper()
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// Run with a live TimescaleDB:
//
//	SMOKE_TEST_DSN='postgres://smoke:smoke@127.0.0.1:5433/smoke?sslmode=disable' go test ./internal/store/pgstore
func TestPGStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// A round with loss so the centered array carries NaN gaps (stored as NULL).
	comp := sample.Compute(5, []float64{0.3, 0.1, 0.2}) // loss 2, median 0.2, centered [NaN,.1,.2,.3,NaN]
	when := time.Unix(1_700_000_000, 0).UTC()
	out := scheduler.Outcome{
		Target: probe.Target{Name: "t1", Host: "h1"}, ProbeName: "FPing",
		Computed: comp, When: when, Duration: 12 * time.Millisecond,
	}
	// A second, later round, total loss.
	out2 := scheduler.Outcome{
		Target: probe.Target{Name: "t1", Host: "h1"}, ProbeName: "FPing",
		Computed: sample.Compute(5, nil), When: when.Add(time.Minute),
	}
	s.Add([]scheduler.Outcome{out, out2})

	if keys, err := s.Keys(); err != nil || len(keys) != 1 || keys[0] != "t1" {
		t.Fatalf("Keys = %v (err %v), want [t1]", keys, err)
	}

	// Latest is the total-loss round.
	latest, ok := s.Latest("t1")
	if !ok {
		t.Fatal("Latest not found")
	}
	if latest.Computed.Loss != 5 || !math.IsNaN(latest.Computed.Median) {
		t.Errorf("latest loss=%d median=%v, want loss=5 median=NaN", latest.Computed.Loss, latest.Computed.Median)
	}

	// History oldest->newest, NaN gaps preserved.
	hist, err := s.History("t1")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("History len = %d, want 2", len(hist))
	}
	h0 := hist[0].Computed
	if h0.Loss != 2 || h0.Median != 0.2 {
		t.Errorf("round0 loss=%d median=%v, want 2/0.2", h0.Loss, h0.Median)
	}
	if len(h0.Centered) != 5 || !math.IsNaN(h0.Centered[0]) || h0.Centered[1] != 0.1 ||
		h0.Centered[2] != 0.2 || h0.Centered[3] != 0.3 || !math.IsNaN(h0.Centered[4]) {
		t.Errorf("round0 centered = %v, want [NaN,0.1,0.2,0.3,NaN]", h0.Centered)
	}
	if hist[0].When.After(hist[1].When) {
		t.Errorf("history not oldest->newest")
	}
	if hist[0].Err != nil {
		t.Errorf("round0 unexpected err: %v", hist[0].Err)
	}
	if hist[0].Duration != 12*time.Millisecond {
		t.Errorf("round0 duration = %v, want 12ms (must survive the DB round-trip)", hist[0].Duration)
	}
}

// The whole point of #8: availability is computed over the full requested window,
// not just the last histCap stored rounds. Here histCap=10 but the window holds
// 60 rounds — History truncates to 10, Availability must see all 60.
func TestPGStoreAvailabilityIgnoresHistoryCap(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 10, func(e error) { t.Errorf("store error: %v", e) }) // deliberately small cap
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	base := time.Unix(1_700_100_000, 0).UTC()
	const N = 60
	var outs []scheduler.Outcome
	for i := 0; i < N; i++ {
		var rtts []float64
		if i%10 == 9 {
			rtts = nil // every 10th round fully lost -> down
		} else {
			rtts = []float64{0.01, 0.02, 0.03, 0.04}
		}
		outs = append(outs, scheduler.Outcome{
			Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(4, rtts),
		})
	}
	s.Add(outs)

	// The History-based path the old SLA used is capped — the truncation being fixed.
	if h, err := s.History("t"); err != nil || len(h) != 10 {
		t.Fatalf("History len = %d, want 10 (the cap that truncated SLA)", len(h))
	}

	// Availability spans the whole window regardless of the cap.
	st, err := s.Availability(ctx, "t", "local", base, nil)
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if st.Measured != N {
		t.Errorf("measured = %d, want %d (must ignore the history cap of 10)", st.Measured, N)
	}
	down := N / 10
	if st.Up != N-down {
		t.Errorf("up = %d, want %d", st.Up, N-down)
	}
	if !st.Oldest.Equal(base) || !st.Latest.Equal(base.Add(time.Duration(N-1)*time.Minute)) {
		t.Errorf("oldest/latest = %v / %v", st.Oldest, st.Latest)
	}

	// cutoff filters to the in-window subset.
	st2, err := s.Availability(ctx, "t", "local", base.Add(30*time.Minute), nil)
	if err != nil {
		t.Fatalf("Availability (cutoff): %v", err)
	}
	if st2.Measured != N-30 {
		t.Errorf("measured after cutoff = %d, want %d", st2.Measured, N-30)
	}

	// maxLossPct=10: the fully-lost rounds are down; the healthy rounds (0% loss) stay up.
	maxLoss := 10.0
	st3, err := s.Availability(ctx, "t", "local", base, &maxLoss)
	if err != nil {
		t.Fatalf("Availability (maxloss): %v", err)
	}
	if st3.Up != N-down {
		t.Errorf("up (maxloss=10) = %d, want %d", st3.Up, N-down)
	}
}

// The 30h raw drill-down needs the full window, not just the last histCap rounds.
// Here histCap=10 but the window holds 60 rounds — History truncates to 10,
// HistorySince must return all 60 (oldest->newest), and honour the cutoff.
func TestPGStoreHistorySinceIgnoresHistoryCap(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 10, func(e error) { t.Errorf("store error: %v", e) }) // deliberately small cap
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	base := time.Unix(1_700_200_000, 0).UTC()
	const N = 60
	var outs []scheduler.Outcome
	for i := 0; i < N; i++ {
		outs = append(outs, scheduler.Outcome{
			Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04}),
		})
	}
	s.Add(outs)

	// History is capped — the truncation being fixed.
	if h, err := s.History("t"); err != nil || len(h) != 10 {
		t.Fatalf("History len = %d, want 10 (the cap that truncated the 30h view)", len(h))
	}

	// HistorySince spans the whole window regardless of the cap, oldest->newest.
	got, err := s.HistorySince(ctx, "t", "local", base)
	if err != nil {
		t.Fatalf("HistorySince: %v", err)
	}
	if len(got) != N {
		t.Fatalf("HistorySince len = %d, want %d (must ignore the history cap of 10)", len(got), N)
	}
	if !got[0].When.Equal(base) || !got[N-1].When.Equal(base.Add(time.Duration(N-1)*time.Minute)) {
		t.Errorf("oldest/newest = %v / %v, want %v / %v", got[0].When, got[N-1].When, base, base.Add(time.Duration(N-1)*time.Minute))
	}

	// cutoff filters to the in-window subset.
	got2, err := s.HistorySince(ctx, "t", "local", base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("HistorySince (cutoff): %v", err)
	}
	if len(got2) != N-30 {
		t.Errorf("HistorySince after cutoff = %d, want %d", len(got2), N-30)
	}
}

// #5: a failed sample-batch write is counted and exposed on /metrics, so a database
// rejecting or timing out writes is visible (writes are fire-and-forget from the round's
// point of view). Inducing a failure by writing to a closed pool.
func TestPGStoreWriteFailureMetric(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, nil) // nil onErr -> failures are counted, not surfaced as fatal
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Close() // close the pool so the next write fails

	s.Add([]scheduler.Outcome{{
		Target: probe.Target{Name: "x", Host: "h"}, ProbeName: "FPing",
		When: time.Unix(1_700_600_000, 0).UTC(), Computed: sample.Compute(1, []float64{0.01}),
	}})
	if got := s.writeFails.Load(); got != 1 {
		t.Fatalf("writeFails = %d, want 1 after a write to a closed pool", got)
	}
	var b strings.Builder
	s.WriteMetrics(&b)
	if !strings.Contains(b.String(), "smokeping_store_write_failures_total 1") {
		t.Errorf("metrics missing the failure counter:\n%s", b.String())
	}
}

func TestMsToDuration(t *testing.T) {
	cases := []struct {
		ms   float64
		want time.Duration
	}{
		{12, 12 * time.Millisecond},
		{1.5, 1500 * time.Microsecond},
		{0, 0},
	}
	for _, c := range cases {
		if got := msToDuration(c.ms); got != c.want {
			t.Errorf("msToDuration(%g) = %v, want %v", c.ms, got, c.want)
		}
	}
}

func TestEnableDownsampling(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE"); err != nil {
		t.Fatalf("drop cagg: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	s.Add([]scheduler.Outcome{{
		Target: probe.Target{Name: "d", Host: "h"}, ProbeName: "FPing",
		Computed: sample.Compute(5, []float64{0.01, 0.02, 0.03, 0.04, 0.05}),
		When:     time.Unix(1_700_000_000, 0).UTC(),
	}})

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	// second call must be a no-op (idempotent)
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling (2nd): %v", err)
	}

	refreshAgg(ctx, t, s, "samples_hourly")
	var rounds int
	var medAvg float64
	if err := s.pool.QueryRow(ctx,
		"SELECT rounds, median_avg FROM samples_hourly WHERE target='d'").Scan(&rounds, &medAvg); err != nil {
		t.Fatalf("query aggregate: %v", err)
	}
	if rounds != 1 {
		t.Errorf("aggregate rounds = %d, want 1", rounds)
	}
	if medAvg != 0.03 { // median of the 5 samples
		t.Errorf("aggregate median_avg = %v, want 0.03", medAvg)
	}
}

// CODE_REVIEW L1: if samples_hourly is present but samples_daily is missing (a partial/interrupted
// schema), EnableDownsampling must still run its one-time backfill so the recreated daily view is
// populated from existing raw history — not left empty until trailing refreshes catch up. The
// backfill decision now keys off BOTH aggregates existing, not just hourly.
func TestEnableDownsamplingBackfillsDailyWhenOnlyDailyMissing(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	// Clean slate with both aggregates present.
	if _, err := s.pool.Exec(ctx, "DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE"); err != nil {
		t.Fatalf("drop hourly: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	// A raw sample ~48h ago: a completed past daily bucket, inside the daily backfill window
	// (now-30d .. now-1h) and within raw retention.
	when := time.Now().Add(-48 * time.Hour).UTC()
	s.Add([]scheduler.Outcome{{
		Target: probe.Target{Name: "d", Host: "h"}, ProbeName: "FPing",
		Computed: sample.Compute(3, []float64{0.01, 0.02, 0.03}), When: when,
	}})
	// Simulate the partial schema: drop ONLY daily, keep hourly.
	if _, err := s.pool.Exec(ctx, "DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE"); err != nil {
		t.Fatalf("drop daily: %v", err)
	}
	// Re-enable: hourly present but daily missing must still trigger the backfill.
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling (daily missing): %v", err)
	}
	var rounds int
	if err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(sum(rounds),0)::int FROM samples_daily WHERE target='d'").Scan(&rounds); err != nil {
		t.Fatalf("query samples_daily: %v", err)
	}
	if rounds < 1 {
		t.Fatalf("samples_daily not backfilled after re-enable with only daily missing: rounds=%d (want >=1)", rounds)
	}
}

// AggregatesExist (the --history preflight) must require BOTH samples_hourly and
// samples_daily, so an interrupted/partial schema (hourly present, daily dropped) can't pass
// the fail-before-write check and then fail mid-import on the daily refresh (CODE_REVIEW #6).
func TestAggregatesExistRequiresBothViews(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	// Both views present -> true.
	if ok, err := s.AggregatesExist(ctx); err != nil || !ok {
		t.Fatalf("AggregatesExist with both views = (%v,%v), want (true,nil)", ok, err)
	}
	// Drop only the daily view: the preflight must now report false, not "hourly is enough".
	if _, err := s.pool.Exec(ctx, "DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE"); err != nil {
		t.Fatalf("drop daily cagg: %v", err)
	}
	if ok, err := s.AggregatesExist(ctx); err != nil || ok {
		t.Fatalf("AggregatesExist with daily absent = (%v,%v), want (false,nil)", ok, err)
	}
	// Restore for any subsequent test sharing this database.
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling (restore): %v", err)
	}
}

// An existing continuous aggregate created before median_rounds existed must be rebuilt by
// EnableDownsampling — the migrateAggregates drop+recreate — so the new column appears
// (CODE_REVIEW #6 / P2-6). Without the rebuild, Rollup's SELECT of median_rounds would fail.
func TestMigrateAggregatesAddsMedianRounds(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Build the PRE-median_rounds aggregate shape (has vantage, but count(*) AS rounds only).
	for _, q := range []string{
		"DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE",
		"DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE",
		"TRUNCATE samples",
		`CREATE MATERIALIZED VIEW samples_hourly
		 WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
		 SELECT time_bucket('1 hour', ts) AS bucket, target, vantage,
		        avg(median_seconds) AS median_avg, min(median_seconds) AS median_min,
		        max(median_seconds) AS median_max,
		        avg(loss::float / NULLIF(pings, 0)) AS loss_frac, count(*) AS rounds
		 FROM samples GROUP BY bucket, target, vantage WITH NO DATA`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	// Precondition: the old shape lacks median_rounds.
	var has bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name='samples_hourly' AND column_name='median_rounds')`).Scan(&has); err != nil {
		t.Fatalf("precheck: %v", err)
	}
	if has {
		t.Fatal("precondition failed: old-shape aggregate already has median_rounds")
	}

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling (migration): %v", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name='samples_hourly' AND column_name='median_rounds')`).Scan(&has); err != nil {
		t.Fatalf("postcheck: %v", err)
	}
	if !has {
		t.Error("EnableDownsampling did not rebuild the aggregate to add median_rounds")
	}
}

// A bucket containing fully-lost rounds must report median_rounds (rounds that produced a
// median) distinct from rounds (all rounds), and its median_avg must exclude the lost
// rounds. This is what lets the client weight the median by surviving rounds rather than
// total rounds during an outage (CODE_REVIEW #6 / P2-6).
func TestRollupMedianRoundsExcludesLostRounds(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	for _, q := range []string{
		"DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE",
		"DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE",
		"TRUNCATE samples",
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// One successful round (median 0.02) and three fully-lost rounds, all in one hour bucket.
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	outs := []scheduler.Outcome{
		{Target: probe.Target{Name: "mr", Host: "h"}, ProbeName: "FPing", When: base, Computed: sample.Compute(3, []float64{0.01, 0.02, 0.03})},
	}
	for i := 1; i <= 3; i++ {
		outs = append(outs, scheduler.Outcome{
			Target: probe.Target{Name: "mr", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(3, nil), // total loss -> NULL median
		})
	}
	s.Add(outs)

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	refreshAgg(ctx, t, s, "samples_hourly")

	pts, err := s.Rollup(ctx, "mr", "local", "1h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d buckets, want 1: %+v", len(pts), pts)
	}
	p := pts[0]
	if p.Rounds != 4 {
		t.Errorf("Rounds = %d, want 4 (all rounds)", p.Rounds)
	}
	if p.MedianRounds != 1 {
		t.Errorf("MedianRounds = %d, want 1 (only the surviving round produced a median)", p.MedianRounds)
	}
	if p.MedianAvg < 0.019 || p.MedianAvg > 0.021 {
		t.Errorf("MedianAvg = %v, want ~0.02 (lost rounds excluded)", p.MedianAvg)
	}
}

// The 400d drill-down reads a daily tier. EnableDownsampling must also create
// samples_daily, and Rollup(..., "1d") must return day buckets: rounds per day,
// and the median avg/min/max consolidated across each day's rounds.
func TestDailyRollup(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	for _, q := range []string{
		"DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE",
		"DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE",
		"TRUNCATE samples",
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	day1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "dr", Host: "h"}, ProbeName: "FPing", When: day1, Computed: sample.Compute(3, []float64{0.01, 0.02, 0.03})},                // median .02
		{Target: probe.Target{Name: "dr", Host: "h"}, ProbeName: "FPing", When: day1.Add(time.Hour), Computed: sample.Compute(3, []float64{0.03, 0.04, 0.05})}, // median .04
		{Target: probe.Target{Name: "dr", Host: "h"}, ProbeName: "FPing", When: day2, Computed: sample.Compute(3, []float64{0.09, 0.10, 0.11})},                // median .10
	})

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	refreshAgg(ctx, t, s, "samples_daily")

	pts, err := s.Rollup(ctx, "dr", "local", "1d", time.Time{}, time.Time{}) // zero since/until -> full history
	if err != nil {
		t.Fatalf("Rollup 1d: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("daily buckets = %d, want 2", len(pts))
	}
	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	// Day 1: two rounds, medians .02 and .04 -> avg .03, min .02, max .04.
	d1 := pts[0]
	if d1.Rounds != 2 {
		t.Errorf("day1 rounds = %d, want 2", d1.Rounds)
	}
	if !approx(d1.MedianAvg, 0.03) || !approx(d1.MedianMin, 0.02) || !approx(d1.MedianMax, 0.04) {
		t.Errorf("day1 med avg/min/max = %v/%v/%v, want .03/.02/.04", d1.MedianAvg, d1.MedianMin, d1.MedianMax)
	}
	// Day 2: one round, median .10.
	if d2 := pts[1]; d2.Rounds != 1 || !approx(d2.MedianAvg, 0.10) {
		t.Errorf("day2 rounds/avg = %d/%v, want 1/.10", d2.Rounds, d2.MedianAvg)
	}

	// A `since` at the day-2 bucket start bounds the result to day 2 only (daily
	// buckets are labeled at midnight, so the cutoff is the bucket boundary).
	day2Start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	pts2, err := s.Rollup(ctx, "dr", "local", "1d", day2Start, time.Time{})
	if err != nil {
		t.Fatalf("Rollup 1d since day2: %v", err)
	}
	if len(pts2) != 1 {
		t.Fatalf("windowed daily = %d buckets, want 1 (day 2 only)", len(pts2))
	}
	if !approx(pts2[0].MedianAvg, 0.10) {
		t.Errorf("windowed daily avg = %v, want .10 (day 2)", pts2[0].MedianAvg)
	}

	// An `until` at the day-1 boundary (exclusive of day 2) bounds to day 1 only — the
	// drag-zoom [from,to] path. `since` zero means "from the start".
	pts3, err := s.Rollup(ctx, "dr", "local", "1d", time.Time{}, day2Start.Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("Rollup 1d until day1: %v", err)
	}
	if len(pts3) != 1 {
		t.Fatalf("bounded daily [.., day1] = %d buckets, want 1 (day 1 only)", len(pts3))
	}
	if !approx(pts3[0].MedianAvg, 0.03) {
		t.Fatalf("bounded daily [.., day1] avg = %v, want .03 (day 1 only)", pts3[0].MedianAvg)
	}
}

// TestRollupIsolatesByVantage covers the read side of the vantage dimension: the
// continuous aggregates GROUP BY (bucket, target, vantage), so two vantages probing
// the same target in the same hour must never blend into one bucket. samples_hourly
// is created WITH (timescaledb.materialized_only = false), so the just-written rows
// are already visible via real-time aggregation without a manual refresh — this test
// refreshes anyway so the assertions don't depend on that continuing to hold. Because
// real-time aggregation makes exact medians available here, this asserts the precise
// per-vantage median (not just bucket presence): local's bucket must show only its
// own 0.01s round, nyc's only its own 0.09s round, with nothing crossing over.
func TestRollupIsolatesByVantage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	hour := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "riv", Host: "h"}, ProbeName: "FPing", When: hour,
			Computed: sample.Compute(1, []float64{0.01})}, // empty Vantage -> "local"
		{Target: probe.Target{Name: "riv", Host: "h"}, ProbeName: "FPing", When: hour.Add(10 * time.Minute),
			Computed: sample.Compute(1, []float64{0.09}), Vantage: "nyc"},
	})

	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	refreshAgg(ctx, t, s, "samples_hourly")

	local, err := s.Rollup(ctx, "riv", "local", "1h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Rollup local: %v", err)
	}
	nyc, err := s.Rollup(ctx, "riv", "nyc", "1h", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Rollup nyc: %v", err)
	}

	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if len(local) != 1 || local[0].Rounds != 1 {
		t.Fatalf("local buckets = %+v, want exactly 1 bucket with 1 round", local)
	}
	if !approx(local[0].MedianAvg, 0.01) {
		t.Errorf("local median_avg = %v, want 0.01 (must not include nyc's 0.09 round)", local[0].MedianAvg)
	}
	if len(nyc) != 1 || nyc[0].Rounds != 1 {
		t.Fatalf("nyc buckets = %+v, want exactly 1 bucket with 1 round", nyc)
	}
	if !approx(nyc[0].MedianAvg, 0.09) {
		t.Errorf("nyc median_avg = %v, want 0.09 (must not include local's 0.01 round)", nyc[0].MedianAvg)
	}
}

// LatestAll and AvailabilityAll return every target in one query — the bulk forms
// the API uses to avoid a per-target fan-out.
func TestPGStoreBulkReads(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	base := time.Unix(1_700_300_000, 0).UTC()
	// a: two rounds, both up; b: one round, fully lost (down).
	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing", When: base, Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04})},
		{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing", When: base.Add(time.Minute), Computed: sample.Compute(4, []float64{0.05, 0.06, 0.07, 0.08})},
		{Target: probe.Target{Name: "b", Host: "h"}, ProbeName: "TCPConnect", When: base, Computed: sample.Compute(4, nil)},
	})

	all, err := s.LatestAll("local")
	if err != nil {
		t.Fatalf("LatestAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LatestAll len = %d, want 2", len(all))
	}
	if !all["a"].When.Equal(base.Add(time.Minute)) {
		t.Errorf("a latest = %v, want the newer round", all["a"].When)
	}
	if all["b"].ProbeName != "TCPConnect" {
		t.Errorf("b probe = %q, want TCPConnect", all["b"].ProbeName)
	}

	av, err := s.AvailabilityAll(ctx, "local", base, nil)
	if err != nil {
		t.Fatalf("AvailabilityAll: %v", err)
	}
	if a := av["a"]; a.Measured != 2 || a.Up != 2 {
		t.Errorf("a measured/up = %d/%d, want 2/2", a.Measured, a.Up)
	}
	if b := av["b"]; b.Measured != 1 || b.Up != 0 {
		t.Errorf("b measured/up = %d/%d, want 1/0", b.Measured, b.Up)
	}
	// matches the per-target Availability
	a1, _ := s.Availability(ctx, "a", "local", base, nil)
	if a1.Measured != av["a"].Measured || a1.Up != av["a"].Up {
		t.Errorf("AvailabilityAll[a] != Availability(a)")
	}
}

// #3: a windowed single-target read is capped at maxRangeRounds, keeping the NEWEST
// rounds (still returned oldest->newest). The cap is far above the documented 30h view
// at the 1s min step; here it is lowered so a handful of rows exercises the truncation.
func TestPGStoreHistorySinceCap(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	old := maxRangeRounds
	maxRangeRounds = 5
	defer func() { maxRangeRounds = old }()

	base := time.Unix(1_700_400_000, 0).UTC()
	const N = 12
	var outs []scheduler.Outcome
	for i := 0; i < N; i++ {
		outs = append(outs, scheduler.Outcome{
			Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04}),
		})
	}
	s.Add(outs)

	got, err := s.HistorySince(ctx, "t", "local", base)
	if err != nil {
		t.Fatalf("HistorySince: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("HistorySince len = %d, want 5 (capped at maxRangeRounds)", len(got))
	}
	// the newest 5 rounds (7..11), still oldest->newest
	if !got[0].When.Equal(base.Add(7*time.Minute)) || !got[4].When.Equal(base.Add(11*time.Minute)) {
		t.Errorf("cap kept %v..%v, want rounds 7..11 (the newest 5)", got[0].When, got[4].When)
	}
}

// drag-zoom: HistoryBetween returns a target's rounds in [from,to], oldest->newest.
func TestPGStoreHistoryBetween(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	base := time.Unix(1_700_700_000, 0).UTC()
	var outs []scheduler.Outcome
	for i := 0; i < 6; i++ { // rounds at minutes 0..5
		outs = append(outs, scheduler.Outcome{
			Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04}),
		})
	}
	s.Add(outs)

	got, err := s.HistoryBetween(ctx, "t", "local", base.Add(time.Minute), base.Add(4*time.Minute)) // [min1, min4]
	if err != nil {
		t.Fatalf("HistoryBetween: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d rounds, want 4 (minutes 1..4)", len(got))
	}
	if !got[0].When.Equal(base.Add(time.Minute)) || !got[3].When.Equal(base.Add(4*time.Minute)) {
		t.Errorf("range = %v..%v, want min1..min4 oldest->newest", got[0].When, got[3].When)
	}
}

// #8: the bulk grid read caps each target to its newest maxSeriesAllPerTarget rounds,
// so no target is ever dropped whole (as a bare LIMIT would); rounds stay strictly after
// the cutoff and oldest->newest.
func TestPGStoreSeriesAllPerTargetCap(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	old := maxSeriesAllPerTarget
	maxSeriesAllPerTarget = 3
	defer func() { maxSeriesAllPerTarget = old }()

	base := time.Unix(1_700_500_000, 0).UTC()
	mk := func(name string, i int) scheduler.Outcome {
		return scheduler.Outcome{
			Target: probe.Target{Name: name, Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(4, []float64{0.01, 0.02, 0.03, 0.04}),
		}
	}
	var outs []scheduler.Outcome
	for i := 0; i < 5; i++ { // target a: 5 rounds -> capped to 3
		outs = append(outs, mk("a", i))
	}
	for i := 0; i < 2; i++ { // target b: 2 rounds -> under the cap, all kept
		outs = append(outs, mk("b", i))
	}
	s.Add(outs)

	all, _, err := s.SeriesAll(ctx, "local", base.Add(-time.Second), 0) // 0 = no global bound; everything is after this
	if err != nil {
		t.Fatalf("SeriesAll: %v", err)
	}
	if len(all["a"]) != 3 {
		t.Fatalf("a rounds = %d, want 3 (capped)", len(all["a"]))
	}
	if len(all["b"]) != 2 {
		t.Fatalf("b rounds = %d, want 2 (under cap, kept)", len(all["b"]))
	}
	// a kept the newest 3 (rounds 2,3,4), oldest->newest
	if !all["a"][0].When.Equal(base.Add(2*time.Minute)) || !all["a"][2].When.Equal(base.Add(4*time.Minute)) {
		t.Errorf("a cap kept %v..%v, want rounds 2..4 (newest 3)", all["a"][0].When, all["a"][2].When)
	}
	// strictly-after cutoff: a cutoff at round 2's ts drops a's 0..2 and all of b.
	after, _, err := s.SeriesAll(ctx, "local", base.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatalf("SeriesAll (cutoff): %v", err)
	}
	if len(after["a"]) != 2 {
		t.Errorf("a after cutoff = %d, want 2 (rounds 3,4)", len(after["a"]))
	}
	if _, ok := after["b"]; ok {
		t.Errorf("b should be absent after the cutoff (its newest round is at the cutoff)")
	}
}

// CODE_REVIEW M5 (store-query bound): SeriesAll caps EACH target at min(perTarget, maxTotal/targets)
// in the query itself, so the total materialized stays under the global budget, every target stays
// represented, and it reports truncation — bounding the query, not just the response.
func TestPGStoreSeriesAllGlobalBound(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	base := time.Unix(1_700_600_000, 0).UTC()
	mk := func(name string, i int) scheduler.Outcome {
		return scheduler.Outcome{
			Target: probe.Target{Name: name, Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(1, []float64{0.01}),
		}
	}
	var outs []scheduler.Outcome
	for _, tgt := range []string{"a", "b", "c"} {
		for i := 0; i < 4; i++ { // 3 targets x 4 rounds = 12 total
			outs = append(outs, mk(tgt, i))
		}
	}
	s.Add(outs)
	// maxTotal=6 over 3 targets -> each capped to its 2 newest; truncated.
	all, truncated, err := s.SeriesAll(ctx, "local", base.Add(-time.Second), 6)
	if err != nil {
		t.Fatalf("SeriesAll: %v", err)
	}
	if !truncated {
		t.Fatal("want truncated=true when the global budget bites")
	}
	total := 0
	for name, h := range all {
		total += len(h)
		if len(h) != 2 {
			t.Fatalf("target %q kept %d rounds, want 2 (fair share of the 6-round budget over 3 targets)", name, len(h))
		}
		if !h[len(h)-1].When.Equal(base.Add(3 * time.Minute)) {
			t.Fatalf("target %q must keep its NEWEST rounds, last=%v", name, h[len(h)-1].When)
		}
	}
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
}

func TestPGStoreWritesVantage(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	when := time.Unix(1_700_000_000, 0).UTC()
	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "hub", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(1, []float64{0.01}), When: when}, // empty -> local
		{Target: probe.Target{Name: "remote", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(1, []float64{0.01}), When: when, Vantage: "nyc"},
	})

	want := map[string]string{"hub": "local", "remote": "nyc"}
	for target, wv := range want {
		var got string
		if err := s.pool.QueryRow(ctx,
			`SELECT vantage FROM samples WHERE target=$1 LIMIT 1`, target).Scan(&got); err != nil {
			t.Fatalf("select vantage for %s: %v", target, err)
		}
		if got != wv {
			t.Errorf("target %s vantage = %q, want %q", target, got, wv)
		}
	}
}

func TestPGStoreRenamesLegacyMasterVantage(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Simulate a pre-upgrade DB: the column default is still 'master' and a hub row
	// was written under the old name.
	for _, q := range []string{
		"TRUNCATE samples",
		"ALTER TABLE samples ALTER COLUMN vantage SET DEFAULT 'master'",
		`INSERT INTO samples (ts,target,probe,host,vantage,pings,loss,rtts_seconds)
		   VALUES (now(),'legacy','FPing','h','master',1,0,'{0.01}')`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var got string
	if err := s.pool.QueryRow(ctx, "SELECT vantage FROM samples WHERE target='legacy'").Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != "local" {
		t.Errorf("legacy row vantage = %q, want \"local\" (rename did not run)", got)
	}
	// And the rename is idempotent: a second migrate is a no-op and doesn't error.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// A database created before the vantage column existed (a 0.1-era install) must
// upgrade cleanly: migrate has to ADD the column before it builds the
// (target, vantage, ts) index that references it, because CREATE TABLE
// IF NOT EXISTS never alters a table that already exists. Reproduces CODE_REVIEW #1
// (P1-1) — without the ADD COLUMN step the index build fails with
// "column vantage does not exist" and smoked cannot start.
func TestPGStoreMigratesPreVantageSchema(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Rebuild the exact pre-federation schema: no vantage column, the old
	// (target, ts) unique index, and one row written under it.
	for _, q := range []string{
		"DROP TABLE IF EXISTS samples CASCADE",
		`CREATE TABLE samples (
			ts             timestamptz        NOT NULL,
			target         text               NOT NULL,
			probe          text               NOT NULL,
			host           text               NOT NULL,
			pings          smallint           NOT NULL,
			loss           smallint           NOT NULL,
			median_seconds double precision,
			rtts_seconds   double precision[] NOT NULL,
			err            text,
			duration_ms    double precision
		)`,
		"SELECT create_hypertable('samples','ts', if_not_exists => TRUE)",
		"CREATE UNIQUE INDEX samples_target_ts ON samples (target, ts)",
		`INSERT INTO samples (ts,target,probe,host,pings,loss,rtts_seconds)
		   VALUES (now(),'legacy','FPing','h',1,0,'{0.01}')`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}

	// The upgrade must succeed and backfill the pre-existing row to 'local'
	// (every pre-federation row was a hub-local measurement).
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate pre-vantage schema: %v", err)
	}
	var got string
	if err := s.pool.QueryRow(ctx, "SELECT vantage FROM samples WHERE target='legacy'").Scan(&got); err != nil {
		t.Fatalf("select vantage: %v", err)
	}
	if got != "local" {
		t.Errorf("legacy row vantage = %q, want \"local\"", got)
	}
	// The composite unique index must now exist and the legacy (target, ts) index
	// must be gone; a second migrate is a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var oldIdx int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_indexes WHERE tablename='samples' AND indexname='samples_target_ts'").Scan(&oldIdx); err != nil {
		t.Fatalf("index check: %v", err)
	}
	if oldIdx != 0 {
		t.Errorf("legacy index samples_target_ts still present after migrate")
	}
	// Leave a clean current-schema table for later tests in this serial package.
	if _, err := s.pool.Exec(ctx, "TRUNCATE samples"); err != nil {
		t.Fatalf("cleanup truncate: %v", err)
	}
}

// At the shutdown boundary the process context is cancelled and then the dispatcher
// drains in-flight probes, whose completed outcomes reach Add. Add must still persist
// them: its write deadline is a fresh bounded context, not the (now-cancelled) process
// context, so the final local rounds are not lost on a graceful shutdown (CODE_REVIEW #10 /
// P2-10).
func TestAddPersistsAcrossContextCancel(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var storeErr error
	s, err := New(ctx, dsn, 100, func(e error) { storeErr = e })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	cancel() // process shutting down
	s.Add([]scheduler.Outcome{{
		Target: probe.Target{Name: "drain", Host: "h"}, ProbeName: "FPing",
		Computed: sample.Compute(1, []float64{0.01}), When: time.Unix(1_700_000_100, 0).UTC(),
	}})

	if storeErr != nil {
		t.Fatalf("Add after context cancel reported a write error (round lost on shutdown): %v", storeErr)
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM samples WHERE target='drain'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (Add must drain the write past process-context cancel)", n)
	}
}

// A replayed batch (same target/vantage/ts) must not duplicate rows — the
// idempotency an agent retrying an ingest POST after a dropped response relies on.
func TestAddResultsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	tgt := probe.Target{Name: "idem", Host: "10.0.0.1"}
	when := time.Date(2026, 8, 7, 12, 0, 0, 123_000_000, time.UTC)
	mk := func() []scheduler.Outcome {
		return []scheduler.Outcome{{
			Target: tgt, ProbeName: "FPing", When: when, Vantage: "nyc",
			Computed: sample.Compute(3, []float64{0.010, 0.011, 0.012}),
		}}
	}
	ins1, err := s.AddResults(ctx, mk())
	if err != nil {
		t.Fatalf("AddResults #1: %v", err)
	}
	if len(ins1) != 1 {
		t.Fatalf("first AddResults should report 1 newly-inserted round, got %d", len(ins1))
	}
	ins2, err := s.AddResults(ctx, mk()) // replay
	if err != nil {
		t.Fatalf("AddResults #2 (replay): %v", err)
	}
	if len(ins2) != 0 {
		t.Fatalf("replay must report 0 newly-inserted rounds (so alerts aren't re-evaluated), got %d", len(ins2))
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM samples WHERE target=$1 AND vantage=$2 AND ts=$3`,
		tgt.Name, "nyc", when).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed batch must be idempotent: got %d rows, want 1", n)
	}
}

// The #2 conflation regression: two vantages writing to the same target must never
// be mixed on read. LatestAll and HistorySince must each see only their own vantage.
func TestVantageReadIsolation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	tgt := probe.Target{Name: "iso", Host: "10.0.0.9"}
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	write := func(vant string, i int, rtt float64) {
		o := scheduler.Outcome{
			Target: tgt, ProbeName: "FPing", When: base.Add(time.Duration(i) * time.Minute),
			Vantage: vant, Computed: sample.Compute(2, []float64{rtt, rtt}),
		}
		if _, err := s.AddResults(ctx, []scheduler.Outcome{o}); err != nil {
			t.Fatalf("write %s/%d: %v", vant, i, err)
		}
	}
	for i := 0; i < 5; i++ {
		write("local", i, 0.010) // hub rows
		write("nyc", i, 0.050)   // remote rows, distinct latency
	}

	// LatestAll is per-vantage: local sees ~10ms, nyc sees ~50ms — never mixed.
	for _, tc := range []struct {
		vant string
		want float64 // median seconds
	}{{"local", 0.010}, {"nyc", 0.050}} {
		la, err := s.LatestAll(tc.vant)
		if err != nil {
			t.Fatalf("LatestAll(%s): %v", tc.vant, err)
		}
		o, ok := la[tgt.Name]
		if !ok {
			t.Fatalf("LatestAll(%s): missing target", tc.vant)
		}
		if o.Vantage != tc.vant {
			t.Errorf("LatestAll(%s): got vantage %q", tc.vant, o.Vantage)
		}
		if math.Abs(o.Computed.Median-tc.want) > 1e-9 {
			t.Errorf("LatestAll(%s): median %.4f, want %.4f", tc.vant, o.Computed.Median, tc.want)
		}
	}

	// HistorySince isolates too.
	h, err := s.HistorySince(ctx, tgt.Name, "nyc", base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("HistorySince(nyc): %v", err)
	}
	if len(h) != 5 {
		t.Fatalf("HistorySince(nyc): got %d rounds, want 5", len(h))
	}
	for _, o := range h {
		if o.Vantage != "nyc" {
			t.Fatalf("HistorySince(nyc): leaked vantage %q", o.Vantage)
		}
	}
}

// CODE_REVIEW #4: EnableDownsampling must backfill still-present raw history into the
// aggregates on first create/recreate — the trailing refresh policies (3d hourly) alone never
// materialize older data. Seed rounds 5 days ago (beyond the 3d window) and assert they appear
// in samples_hourly WITHOUT a manual refresh.
func TestEnableDownsamplingBackfillsHistory(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 100, func(e error) { t.Errorf("store error: %v", e) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	for _, q := range []string{
		"DROP MATERIALIZED VIEW IF EXISTS samples_daily CASCADE",
		"DROP MATERIALIZED VIEW IF EXISTS samples_hourly CASCADE",
		"DELETE FROM samples WHERE target='bf-old'",
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	var rounds []scheduler.Outcome
	for i := 0; i < 3; i++ {
		rounds = append(rounds, scheduler.Outcome{
			Target: probe.Target{Name: "bf-old", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(5, []float64{0.01, 0.02, 0.03, 0.04, 0.05}),
			When:     old.Add(time.Duration(i) * time.Minute),
		})
	}
	if _, err := s.AddResults(ctx, rounds); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}
	var got int
	if err := s.pool.QueryRow(ctx, "SELECT coalesce(sum(rounds),0)::int FROM samples_hourly WHERE target='bf-old'").Scan(&got); err != nil {
		t.Fatalf("query aggregate: %v", err)
	}
	if got < 3 {
		t.Fatalf("initial backfill: expected the 3 five-day-old rounds materialized, got sum(rounds)=%d", got)
	}
}
