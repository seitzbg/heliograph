package store

import (
	"context"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
	"smokeping-modern/internal/scheduler"
)

// TestMemStoreAvailability covers the aggregate math: measured/up counts, the
// cutoff boundary, and the maxLossPct criterion.
func TestMemStoreAvailability(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	s := NewMem(100)
	mk := func(off time.Duration, pings int, rtts []float64) scheduler.Outcome {
		return scheduler.Outcome{Target: probe.Target{Name: "t"}, When: t0.Add(off), Computed: sample.Compute(pings, rtts)}
	}
	s.Add([]scheduler.Outcome{mk(0, 4, []float64{.01, .01, .01, .01})})   // 0% loss  -> up
	s.Add([]scheduler.Outcome{mk(1*time.Minute, 4, []float64{.01, .01})}) // 50% loss -> up (got a reply)
	s.Add([]scheduler.Outcome{mk(2*time.Minute, 4, nil)})                 // 100% loss -> down
	s.Add([]scheduler.Outcome{mk(3*time.Minute, 4, []float64{.01, .01, .01, .01})})
	s.Add([]scheduler.Outcome{mk(4*time.Minute, 4, []float64{.01, .01, .01, .01})})
	s.Add([]scheduler.Outcome{mk(5*time.Minute, 4, []float64{.01, .01, .01, .01})})

	// Default: up = at least one reply. 5 of 6 up (only the 100% round is down).
	st, err := s.Availability(context.Background(), "t", t0, nil)
	if err != nil {
		t.Fatalf("Availability: %v", err)
	}
	if st.Measured != 6 || st.Up != 5 {
		t.Errorf("measured/up = %d/%d, want 6/5", st.Measured, st.Up)
	}
	if !st.Oldest.Equal(t0) || !st.Latest.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("oldest/latest = %v/%v, want %v/%v", st.Oldest, st.Latest, t0, t0.Add(5*time.Minute))
	}
	// avg loss = (0 + 50 + 100 + 0 + 0 + 0)/6 = 25
	if avg := st.SumLossPct / float64(st.Measured); avg < 24.9 || avg > 25.1 {
		t.Errorf("avg loss = %g, want ~25", avg)
	}

	// cutoff at t0+2m excludes the first two rounds.
	st2, _ := s.Availability(context.Background(), "t", t0.Add(2*time.Minute), nil)
	if st2.Measured != 4 {
		t.Errorf("measured after cutoff = %d, want 4", st2.Measured)
	}

	// maxLossPct=10: the 50%-loss round now counts as down -> 4 up of 6.
	maxLoss := 10.0
	st3, _ := s.Availability(context.Background(), "t", t0, &maxLoss)
	if st3.Up != 4 {
		t.Errorf("up (maxloss=10) = %d, want 4", st3.Up)
	}
}

// TestMemStoreHistorySince covers the time-bounded read: rounds at or after the
// cutoff, oldest->newest, and an empty result when the cutoff is past everything.
func TestMemStoreHistorySince(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	s := NewMem(100)
	mk := func(off time.Duration) scheduler.Outcome {
		return scheduler.Outcome{Target: probe.Target{Name: "t"}, When: t0.Add(off), Computed: sample.Compute(2, []float64{.01, .02})}
	}
	for i := 0; i < 6; i++ {
		s.Add([]scheduler.Outcome{mk(time.Duration(i) * time.Minute)})
	}

	// cutoff at t0+3m -> the rounds at 3,4,5 minutes, oldest first.
	got, err := s.HistorySince(context.Background(), "t", t0.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("HistorySince: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if !got[0].When.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("oldest = %v, want t0+3m", got[0].When)
	}
	if !got[2].When.Equal(t0.Add(5 * time.Minute)) {
		t.Errorf("newest = %v, want t0+5m", got[2].When)
	}

	// cutoff after everything -> empty.
	empty, _ := s.HistorySince(context.Background(), "t", t0.Add(time.Hour))
	if len(empty) != 0 {
		t.Errorf("expected empty, got %d rounds", len(empty))
	}
}
