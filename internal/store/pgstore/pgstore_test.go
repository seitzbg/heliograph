package pgstore

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
)

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

	if keys := s.Keys(); len(keys) != 1 || keys[0] != "t1" {
		t.Fatalf("Keys = %v, want [t1]", keys)
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
	hist := s.History("t1")
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
	if h := s.History("t"); len(h) != 10 {
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
	if h := s.History("t"); len(h) != 10 {
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
