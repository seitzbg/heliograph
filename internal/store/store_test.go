package store

import (
	"context"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
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
	st, err := s.Availability(context.Background(), "t", "", t0, nil)
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
	st2, _ := s.Availability(context.Background(), "t", "", t0.Add(2*time.Minute), nil)
	if st2.Measured != 4 {
		t.Errorf("measured after cutoff = %d, want 4", st2.Measured)
	}

	// maxLossPct=10: the 50%-loss round now counts as down -> 4 up of 6.
	maxLoss := 10.0
	st3, _ := s.Availability(context.Background(), "t", "", t0, &maxLoss)
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
	got, err := s.HistorySince(context.Background(), "t", "", t0.Add(3*time.Minute))
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
	empty, _ := s.HistorySince(context.Background(), "t", "", t0.Add(time.Hour))
	if len(empty) != 0 {
		t.Errorf("expected empty, got %d rounds", len(empty))
	}
}

// LatestAll returns every target's most recent outcome in one call (the bulk form
// the API uses instead of one Latest per target).
func TestMemStoreLatestAll(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	s := NewMem(100)
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "a"}, When: t0, Computed: sample.Compute(2, []float64{.01, .02})}})
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "a"}, When: t0.Add(time.Minute), Computed: sample.Compute(2, []float64{.03, .04})}})
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "b"}, When: t0, Computed: sample.Compute(2, nil)}})

	all, err := s.LatestAll("")
	if err != nil {
		t.Fatalf("LatestAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("LatestAll len = %d, want 2", len(all))
	}
	if !all["a"].When.Equal(t0.Add(time.Minute)) {
		t.Errorf("a latest = %v, want the newest round t0+1m", all["a"].When)
	}
	if _, ok := all["b"]; !ok {
		t.Errorf("LatestAll missing b")
	}
}

// AvailabilityAll aggregates every target over the window in one call, matching
// the per-target Availability results.
func TestMemStoreAvailabilityAll(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	s := NewMem(100)
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "a"}, When: t0, Computed: sample.Compute(4, []float64{.01, .01, .01, .01})}})
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "a"}, When: t0.Add(time.Minute), Computed: sample.Compute(4, []float64{.01, .01})}}) // 50% loss, still up
	s.Add([]scheduler.Outcome{{Target: probe.Target{Name: "b"}, When: t0, Computed: sample.Compute(4, nil)}})                                  // down

	all, err := s.AvailabilityAll(context.Background(), "", t0, nil)
	if err != nil {
		t.Fatalf("AvailabilityAll: %v", err)
	}
	if a := all["a"]; a.Measured != 2 || a.Up != 2 {
		t.Errorf("a measured/up = %d/%d, want 2/2", a.Measured, a.Up)
	}
	if b := all["b"]; b.Measured != 1 || b.Up != 0 {
		t.Errorf("b measured/up = %d/%d, want 1/0", b.Measured, b.Up)
	}
	// matches the per-target Availability
	a1, _ := s.Availability(context.Background(), "a", "", t0, nil)
	if a1.Measured != all["a"].Measured || a1.Up != all["a"].Up {
		t.Errorf("AvailabilityAll[a] != Availability(a)")
	}
}

// TestMemStoreSeriesAll covers the bulk incremental read: every target's rounds
// strictly after the cutoff, grouped by target, oldest->newest; a round exactly at
// the cutoff is excluded (so an incremental fetch never re-sends its watermark round),
// and a target with no rounds after the cutoff is omitted entirely.
func TestMemStoreSeriesAll(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	s := NewMem(100)
	catalog := []string{"a", "b"}
	add := func(name string, off time.Duration) {
		s.Add([]scheduler.Outcome{{Target: probe.Target{Name: name}, When: t0.Add(off), Computed: sample.Compute(2, []float64{.01, .02})}})
	}
	add("a", 0)
	add("a", 1*time.Minute)
	add("a", 2*time.Minute)
	add("b", 1*time.Minute)
	add("b", 3*time.Minute)

	// cutoff at t0+1m: strictly-after keeps a@2m and b@3m only.
	got, _, err := s.SeriesAll(context.Background(), "", catalog, t0.Add(1*time.Minute), 0) // 0 = unbounded
	if err != nil {
		t.Fatalf("SeriesAll: %v", err)
	}
	if len(got["a"]) != 1 || !got["a"][0].When.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("a rounds len=%d, want [a@2m]", len(got["a"]))
	}
	if len(got["b"]) != 1 || !got["b"][0].When.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("b rounds len=%d, want [b@3m]", len(got["b"]))
	}

	// zero cutoff -> everything, oldest->newest per target.
	all, _, _ := s.SeriesAll(context.Background(), "", catalog, time.Time{}, 0)
	if len(all["a"]) != 3 {
		t.Fatalf("a full len = %d, want 3", len(all["a"]))
	}
	for i := 1; i < len(all["a"]); i++ {
		if all["a"][i].When.Before(all["a"][i-1].When) {
			t.Errorf("a not oldest->newest")
		}
	}

	// cutoff past everything -> no targets in the map.
	none, _, _ := s.SeriesAll(context.Background(), "", catalog, t0.Add(1*time.Hour), 0)
	if len(none) != 0 {
		t.Errorf("past-cutoff map len = %d, want 0", len(none))
	}

	// A supplied application catalog excludes historical rows for removed targets.
	configured, _, err := s.SeriesAll(context.Background(), "", []string{"a"}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("SeriesAll configured catalog: %v", err)
	}
	if len(configured["a"]) != 3 {
		t.Errorf("configured a rounds = %d, want 3", len(configured["a"]))
	}
	if _, ok := configured["b"]; ok {
		t.Error("configured catalog returned removed target b")
	}

	// An absent/empty live catalog means no configured targets, not "discover everything from
	// history". Both store implementations follow this contract so removed rows cannot leak back.
	for name, empty := range map[string][]string{"nil": nil, "empty": {}} {
		emptyResult, _, err := s.SeriesAll(context.Background(), "", empty, time.Time{}, 0)
		if err != nil {
			t.Fatalf("SeriesAll %s catalog: %v", name, err)
		}
		if len(emptyResult) != 0 {
			t.Errorf("SeriesAll %s catalog returned %d historical target(s), want 0", name, len(emptyResult))
		}
	}
}

// CODE_REVIEW M5 (store-query bound): SeriesAll bounds the TOTAL rounds across all targets by
// maxTotal, keeping each target's newest rounds so every target stays represented, and reports
// truncation — so a bulk read over many targets can't materialize an unbounded result.
func TestMemStoreSeriesAllGlobalBound(t *testing.T) {
	s := NewMem(1000)
	base := time.Unix(1_700_000_000, 0).UTC()
	catalog := []string{"a", "b", "c"}
	for _, tgt := range catalog {
		for i := 0; i < 100; i++ {
			s.Add([]scheduler.Outcome{{Target: probe.Target{Name: tgt, Host: "h"}, When: base.Add(time.Duration(i) * time.Second)}})
		}
	}
	// maxTotal=30 across 3 targets -> each keeps its 10 newest; truncated.
	got, truncated, err := s.SeriesAll(context.Background(), "", catalog, base.Add(-time.Second), 30)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("want truncated=true when the total exceeds maxTotal")
	}
	total := 0
	for name, h := range got {
		total += len(h)
		if len(h) == 0 {
			t.Fatalf("target %q dropped entirely; every target must stay represented", name)
		}
		if !h[len(h)-1].When.Equal(base.Add(99 * time.Second)) {
			t.Fatalf("target %q must keep its NEWEST rounds, last=%v", name, h[len(h)-1].When)
		}
	}
	if total > 30 {
		t.Fatalf("bounded total = %d, want <= 30", total)
	}
	if _, tr, _ := s.SeriesAll(context.Background(), "", catalog, base.Add(-time.Second), 0); tr {
		t.Fatal("maxTotal=0 (unbounded) must not truncate")
	}
}

func TestMemStoreVantageDefaulting(t *testing.T) {
	s := NewMem(10)
	when := time.Unix(1_700_000_000, 0).UTC()
	s.Add([]scheduler.Outcome{
		{Target: probe.Target{Name: "a", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(1, []float64{0.01}), When: when}, // Vantage empty -> local
		{Target: probe.Target{Name: "b", Host: "h"}, ProbeName: "FPing",
			Computed: sample.Compute(1, []float64{0.01}), When: when, Vantage: "nyc"},
	})

	if o, ok := s.Latest("a"); !ok || o.Vantage != "local" {
		t.Errorf("Latest(a).Vantage = %q (ok=%v), want \"local\"", o.Vantage, ok)
	}
	// Latest is pinned to the local vantage: b has no local round, so it must not
	// be found there — only via LatestAll("nyc") (the #2 isolation this task adds).
	if _, ok := s.Latest("b"); ok {
		t.Errorf("Latest(b) found a round, want none (b is nyc-only, Latest is local-only)")
	}
	if la, err := s.LatestAll("nyc"); err != nil || la["b"].Vantage != "nyc" {
		t.Errorf("LatestAll(nyc)[b].Vantage = %q (err %v), want \"nyc\"", la["b"].Vantage, err)
	}
	if h, err := s.History("a"); err != nil || len(h) != 1 || h[0].Vantage != "local" {
		t.Errorf("History(a) vantage = %v (err %v), want [local]", h, err)
	}
	if got := VantageOf(scheduler.Outcome{}); got != "local" {
		t.Errorf("VantageOf(zero) = %q, want \"local\"", got)
	}
}

// The MemStore replay dedup index must stay bounded to the retained history, not grow with every
// unique round ever ingested — else a long-lived in-memory ResultIngester leaks (CODE_REVIEW:
// MemStore replay index). With a small history cap, ingesting many unique rounds for one target
// keeps the index at (roughly) the cap.
func TestMemStoreReplayIndexBounded(t *testing.T) {
	s := NewMem(3) // history cap 3 per (vantage,target)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 100; i++ {
		o := scheduler.Outcome{
			Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
			When: base.Add(time.Duration(i) * time.Minute), Computed: sample.Compute(1, []float64{0.01}),
		}
		if _, err := s.AddResults(context.Background(), []scheduler.Outcome{o}); err != nil {
			t.Fatalf("AddResults %d: %v", i, err)
		}
	}
	if n := len(s.ingested); n > 3 {
		t.Fatalf("replay index grew to %d after 100 unique ingests, want <= cap (3)", n)
	}
	// And a replay of a still-retained round is still deduplicated (stored/returned once).
	last := scheduler.Outcome{
		Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
		When: base.Add(99 * time.Minute), Computed: sample.Compute(1, []float64{0.01}),
	}
	if ins, _ := s.AddResults(context.Background(), []scheduler.Outcome{last}); len(ins) != 0 {
		t.Fatalf("replay of a retained round must report 0 newly-inserted, got %d", len(ins))
	}
}
