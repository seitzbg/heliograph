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
