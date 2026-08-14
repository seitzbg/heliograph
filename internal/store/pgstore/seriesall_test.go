package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
)

// seedRounds writes n rounds for target, one minute apart starting at base (oldest first).
func seedRounds(s *PGStore, target string, base time.Time, n int) {
	outs := make([]scheduler.Outcome, n)
	for i := 0; i < n; i++ {
		outs[i] = scheduler.Outcome{
			Target:    probe.Target{Name: target, Host: "h"},
			ProbeName: "FPing",
			Computed:  sample.Compute(3, []float64{0.01, 0.02, 0.03}),
			When:      base.Add(time.Duration(i) * time.Minute),
		}
	}
	s.Add(outs)
}

// CODE_REVIEW M5/L3: SeriesAll returns each target's NEWEST perTarget rounds via the indexed
// per-target LATERAL top-N, and flags truncation ONLY when a target actually exceeded the cap.
// maxSeriesAllPerTarget is overridden small so the cap boundary is cheap to seed; maxTotal=0 keeps
// the fair-share step out of the way so perTarget == the (overridden) ceiling.
func TestSeriesAllTruncationBoundary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer func(orig int) { maxSeriesAllPerTarget = orig }(maxSeriesAllPerTarget)
	maxSeriesAllPerTarget = 3

	base := time.Unix(1_700_000_000, 0).UTC()
	seedRounds(s, "under", base, 2) // below cap
	seedRounds(s, "exact", base, 3) // exactly at cap -> must NOT be truncated (L3)
	seedRounds(s, "over", base, 4)  // above cap -> truncated, newest 3 kept (M5)

	out, truncated, err := s.SeriesAll(ctx, "", base.Add(-time.Second), 0)
	if err != nil {
		t.Fatalf("SeriesAll: %v", err)
	}
	if len(out["under"]) != 2 || len(out["exact"]) != 3 || len(out["over"]) != 3 {
		t.Fatalf("per-target lengths = under:%d exact:%d over:%d, want 2/3/3",
			len(out["under"]), len(out["exact"]), len(out["over"]))
	}
	if !truncated {
		t.Errorf("truncated = false, want true (\"over\" exceeded the cap)")
	}
	// "over" keeps the NEWEST 3 (rounds at +1m,+2m,+3m), oldest->newest — the +0m round was dropped.
	over := out["over"]
	if !over[0].When.Equal(base.Add(time.Minute)) || !over[2].When.Equal(base.Add(3*time.Minute)) {
		t.Errorf("\"over\" kept the wrong rounds: first=%v last=%v (want +1m .. +3m)", over[0].When, over[2].When)
	}
	// Every target's rounds must be oldest->newest.
	for name, hist := range out {
		for i := 1; i < len(hist); i++ {
			if hist[i-1].When.After(hist[i].When) {
				t.Errorf("%s: rounds not oldest->newest", name)
			}
		}
	}
}

// L3 exact-cap regression in isolation: with NO target over the cap (the largest holds EXACTLY
// perTarget rounds), truncated MUST be false. Pre-fix, len>=perTarget tripped a false "truncated"
// warning at the boundary even though nothing was dropped.
func TestSeriesAllExactCapNotTruncated(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer func(orig int) { maxSeriesAllPerTarget = orig }(maxSeriesAllPerTarget)
	maxSeriesAllPerTarget = 3

	base := time.Unix(1_700_000_000, 0).UTC()
	seedRounds(s, "cap", base, 3) // exactly perTarget

	out, truncated, err := s.SeriesAll(ctx, "", base.Add(-time.Second), 0)
	if err != nil {
		t.Fatalf("SeriesAll: %v", err)
	}
	if len(out["cap"]) != 3 {
		t.Fatalf("cap target len = %d, want 3 (all rounds returned)", len(out["cap"]))
	}
	if truncated {
		t.Errorf("truncated = true at the exact-cap boundary; want false (nothing was dropped)")
	}
}
