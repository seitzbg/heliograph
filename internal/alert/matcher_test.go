package alert

import (
	"math"
	"testing"
)

func TestCheckLossHysteresis(t *testing.T) {
	m := CheckLoss{L: 50, X: 3}
	if m.Test(Window{Loss: []float64{60, 60}}, false) { // < X samples worth of bad? only 2
		t.Errorf("should not raise with 2 bad samples when X=3")
	}
	if !m.Test(Window{Loss: []float64{60, 60, 60}}, false) {
		t.Errorf("should raise: 3 consecutive >= 50")
	}
	if !m.Test(Window{Loss: []float64{60, 60, 0}}, true) {
		t.Errorf("should hold firing: not all of last 3 are clear")
	}
	if m.Test(Window{Loss: []float64{0, 0, 0}}, true) {
		t.Errorf("should clear: 3 consecutive < 50")
	}
	if m.Test(Window{Loss: []float64{60, 0, 60}}, false) {
		t.Errorf("should not raise: not all bad")
	}
	if m.Test(Window{Loss: []float64{60}}, true) != true {
		t.Errorf("insufficient data should hold prev state")
	}
}

func TestCheckLatencyNaN(t *testing.T) {
	m := CheckLatency{L: 0.2, X: 2} // 200ms
	if !m.Test(Window{RTT: []float64{0.3, 0.3}}, false) {
		t.Errorf("should raise: 2 consecutive >= 200ms")
	}
	if m.Test(Window{RTT: []float64{math.NaN(), 0.3}}, false) {
		t.Errorf("NaN (lost round) must fail the threshold predicate")
	}
}

func TestCheckLatencyHoldsThroughHardDown(t *testing.T) {
	m := CheckLatency{L: 0.2, X: 2} // 200ms
	// Host goes hard down: both recent rounds are fully lost, so RTT is NaN
	// (no median). A lost round carries no latency opinion and must NOT clear a
	// firing latency alert — reporting the 100%-loss condition is the loss
	// alert's job, not a reason to declare latency "recovered".
	if !m.Test(Window{RTT: []float64{math.NaN(), math.NaN()}}, true) {
		t.Errorf("firing latency alert must hold through an all-lost window, not clear")
	}
	// A single lost round mixed into an otherwise-recovered window also holds,
	// rather than clearing on incomplete data.
	if !m.Test(Window{RTT: []float64{math.NaN(), 0.1}}, true) {
		t.Errorf("firing latency alert must hold when the window still contains a lost round")
	}
	// A genuine recovery (every measured RTT back under threshold) still clears.
	if m.Test(Window{RTT: []float64{0.1, 0.1}}, true) {
		t.Errorf("firing latency alert must clear once RTT recovers below threshold")
	}
}

func TestParseAndMatchPattern(t *testing.T) {
	p, err := ParsePattern("loss", ">50%,>50%")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Length() != 2 {
		t.Fatalf("length = %d, want 2", p.Length())
	}
	if !p.Test(Window{Loss: []float64{0, 60, 60}}, false) { // tail [60,60]
		t.Errorf("pattern should match tail [60,60]")
	}
	if p.Test(Window{Loss: []float64{0, 60, 40}}, false) {
		t.Errorf("pattern should not match tail [60,40]")
	}

	// rtt pattern: values are ms -> seconds internally
	pr, err := ParsePattern("rtt", ">200,>200")
	if err != nil {
		t.Fatalf("parse rtt: %v", err)
	}
	if !pr.Test(Window{RTT: []float64{0.3, 0.3}}, false) {
		t.Errorf("rtt pattern should match [0.3s,0.3s] > 200ms")
	}
	if pr.Test(Window{RTT: []float64{0.1, 0.3}}, false) {
		t.Errorf("rtt pattern should not match tail [0.1,0.3]")
	}
}

// nan is a readable NaN for building loss/rtt windows in tests.
func nan() float64 { return math.NaN() }

// The `*N*` skip must align the surrounding hard tokens correctly — the bug the
// Perl original shipped, where `>0%,*12*,>0%` silently never matched. The pattern
// is right-anchored: the newest sample must satisfy the final token, and a skip
// of 0..N samples sits between the hard comparisons.
func TestPatternSkipAlignment(t *testing.T) {
	p, err := ParsePattern("loss", ">0%,*12*,>0%")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Length() != 14 { // 1 + 12 + 1
		t.Fatalf("Length = %d, want 14", p.Length())
	}
	cases := []struct {
		name string
		loss []float64
		want bool
	}{
		{"adjacent loss (skip=0)", []float64{5, 5}, true},
		{"gap of 1 clean sample", []float64{5, 0, 5}, true},
		{"gap of exactly 12 clean samples", []float64{5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, true},
		{"gap of 13 clean samples (exceeds *12*)", []float64{5, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, false},
		{"newest is clean -> not right-anchored", []float64{5, 0, 5, 0}, false},
		{"only one loss sample", []float64{0, 0, 5}, false},
		{"no loss at all", []float64{0, 0, 0}, false},
	}
	for _, c := range cases {
		if got := p.Test(Window{Loss: c.loss}, false); got != c.want {
			t.Errorf("%s: Test(%v) = %v, want %v", c.name, c.loss, got, c.want)
		}
	}
}

// A pattern with many skips must stay bounded: without memoization, 8 hard slots separated by
// *20* skips explores ~21^7 skip combinations against an all-zero (never-matching) window and
// hangs; the (step,pos) memo bounds it to steps×window so this returns promptly, while a fully
// matching window still matches (memoization must not change the result) (CodeRabbit #2).
func TestPatternManySkipsBounded(t *testing.T) {
	src := ">0%"
	for i := 0; i < 7; i++ {
		src += ",*20*,>0%"
	}
	p, err := ParsePattern("loss", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	win := make([]float64, p.Length()) // all zeros -> no >0% slot ever matches
	if p.Test(Window{Loss: win}, false) {
		t.Fatal("all-zero window must not match a >0% pattern")
	}
	for i := range win {
		win[i] = 60 // every slot satisfies >0% -> pattern matches (skip 0 each time)
	}
	if !p.Test(Window{Loss: win}, false) {
		t.Fatal("all-loss window should satisfy the >0% pattern (memoization broke positive matching)")
	}
}

// The unknown value U must be matchable and distinct from 0% — the second Perl
// pitfall, where a lost round was recorded as 0% so `==U` could never fire and an
// outage read as clean history.
func TestPatternUnknownToken(t *testing.T) {
	// A high-latency round followed by a fully-lost (unknown-rtt) round.
	p, err := ParsePattern("rtt", ">200,==U")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Length() != 2 {
		t.Fatalf("Length = %d, want 2", p.Length())
	}
	if !p.Test(Window{RTT: []float64{0.3, nan()}}, false) {
		t.Errorf("==U should match a lost (NaN) newest round after a >200ms round")
	}
	if p.Test(Window{RTT: []float64{0.3, 0.3}}, false) {
		t.Errorf("==U must not match a known (0.3s) round — unknown is distinct from a value")
	}
	// A zero-latency round is a *known* value, so ==U (unknown) must be false,
	// and !=U (known) must be true — proving U is not conflated with 0.
	pne, _ := ParsePattern("rtt", "!=U")
	if !pne.Test(Window{RTT: []float64{0.0}}, false) {
		t.Errorf("!=U should match a known 0ms round")
	}
	if pne.Test(Window{RTT: []float64{nan()}}, false) {
		t.Errorf("!=U must not match an unknown round")
	}
}

// A bare `*` consumes exactly one arbitrary sample.
func TestPatternBareWildcard(t *testing.T) {
	p, err := ParsePattern("loss", ">50%,*,>50%")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Length() != 3 {
		t.Fatalf("Length = %d, want 3", p.Length())
	}
	if !p.Test(Window{Loss: []float64{60, 0, 60}}, false) {
		t.Errorf("should match: loss, one arbitrary sample, loss")
	}
	if p.Test(Window{Loss: []float64{60, 60}}, false) {
		t.Errorf("bare * is a mandatory slot: [60,60] has no middle sample to fill it")
	}
}

func TestParsePatternRejectsBadTokens(t *testing.T) {
	cases := []struct{ field, src, why string }{
		{"loss", "==S", "S startup sentinel is dropped"},
		{"rtt", "==S", "S startup sentinel is dropped"},
		{"loss", "==U", "U is not valid on the loss field"},
		{"rtt", ">U", "U requires == or !="},
		{"rtt", "<U", "U requires == or !="},
		{"loss", "*5*", "only skips -> matches anything"},
		{"loss", "*,*3*", "only wildcards/skips -> matches anything"},
		{"loss", "*x*", "non-integer skip count"},
		{"loss", "*-3*", "negative skip count"},
		{"loss", ">0%,*20000*,>0%", "skip count exceeds the max window"},
		{"loss", "50%", "missing comparison operator"},
		{"loss", "", "empty pattern"},
		// Threshold value grammar (CODE_REVIEW #7).
		{"loss", ">NaN%", "non-finite loss threshold"},
		{"loss", ">Inf%", "non-finite loss threshold"},
		{"rtt", ">NaN", "non-finite rtt threshold"},
		{"rtt", ">Inf", "non-finite rtt threshold"},
		{"loss", ">150%", "loss threshold above 100%"},
		{"loss", ">-5%", "negative loss threshold"},
		{"rtt", ">-5", "negative rtt threshold"},
	}
	for _, c := range cases {
		if _, err := ParsePattern(c.field, c.src); err == nil {
			t.Errorf("ParsePattern(%q, %q): expected error (%s), got nil", c.field, c.src, c.why)
		}
	}
	// Valid thresholds must still parse — including a skip at the max allowance.
	for _, c := range []struct{ field, src string }{{"loss", ">50%,<100%"}, {"rtt", ">200,<1000"}, {"loss", "==0%"}, {"loss", ">0%,*10000*,>0%"}} {
		if _, err := ParsePattern(c.field, c.src); err != nil {
			t.Errorf("ParsePattern(%q, %q): unexpected error %v", c.field, c.src, err)
		}
	}
}

// Each *N* skip is individually capped, but their sum sizes the per-target window — a
// pattern whose aggregate length exceeds the cap must be rejected, not silently huge.
func TestAggregatePatternLengthRejected(t *testing.T) {
	if _, err := ParsePattern("rtt", ">0,*10000*,*10000*"); err == nil {
		t.Fatal("expected an over-cap aggregate pattern length to be rejected")
	}
	if _, err := ParsePattern("rtt", ">0,*5*,>0"); err != nil {
		t.Fatalf("a modest pattern should still be accepted: %v", err)
	}
}
