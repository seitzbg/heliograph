package pgstore

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
)

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
	st, err := s.Availability(ctx, "t", base, nil)
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
	st2, err := s.Availability(ctx, "t", base.Add(30*time.Minute), nil)
	if err != nil {
		t.Fatalf("Availability (cutoff): %v", err)
	}
	if st2.Measured != N-30 {
		t.Errorf("measured after cutoff = %d, want %d", st2.Measured, N-30)
	}

	// maxLossPct=10: the fully-lost rounds are down; the healthy rounds (0% loss) stay up.
	maxLoss := 10.0
	st3, err := s.Availability(ctx, "t", base, &maxLoss)
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
	got, err := s.HistorySince(ctx, "t", base)
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
	got2, err := s.HistorySince(ctx, "t", base.Add(30*time.Minute))
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

	if _, err := s.pool.Exec(ctx, "CALL refresh_continuous_aggregate('samples_hourly', NULL, NULL)"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
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
	if _, err := s.pool.Exec(ctx, "CALL refresh_continuous_aggregate('samples_daily', NULL, NULL)"); err != nil {
		t.Fatalf("refresh daily: %v", err)
	}

	pts, err := s.Rollup(ctx, "dr", "1d", time.Time{}, time.Time{}) // zero since/until -> full history
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
	pts2, err := s.Rollup(ctx, "dr", "1d", day2Start, time.Time{})
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
	pts3, err := s.Rollup(ctx, "dr", "1d", time.Time{}, day2Start.Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("Rollup 1d until day1: %v", err)
	}
	if len(pts3) != 1 || !approx(pts3[0].MedianAvg, 0.03) {
		t.Fatalf("bounded daily [.., day1] = %d buckets avg %v, want 1 / .03 (day 1 only)", len(pts3), pts3[0].MedianAvg)
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

	all, err := s.LatestAll()
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

	av, err := s.AvailabilityAll(ctx, base, nil)
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
	a1, _ := s.Availability(ctx, "a", base, nil)
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

	got, err := s.HistorySince(ctx, "t", base)
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

	got, err := s.HistoryBetween(ctx, "t", base.Add(time.Minute), base.Add(4*time.Minute)) // [min1, min4]
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

	all, err := s.SeriesAll(ctx, base.Add(-time.Second)) // everything is after this
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
	after, err := s.SeriesAll(ctx, base.Add(2*time.Minute))
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
	if err := s.AddResults(ctx, mk()); err != nil {
		t.Fatalf("AddResults #1: %v", err)
	}
	if err := s.AddResults(ctx, mk()); err != nil { // replay
		t.Fatalf("AddResults #2 (replay): %v", err)
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
