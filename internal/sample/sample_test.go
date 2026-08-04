package sample

import (
	"math"
	"testing"
)

func nan(v float64) bool { return math.IsNaN(v) }

func TestComputeBasic(t *testing.T) {
	// 5 expected, 3 received (2 lost). Unsorted input.
	c := Compute(5, []float64{0.3, 0.1, 0.2})

	if c.Loss != 2 {
		t.Errorf("loss = %d, want 2", c.Loss)
	}
	if c.Median != 0.2 { // sorted[3/2] = sorted[1] = 0.2
		t.Errorf("median = %v, want 0.2", c.Median)
	}
	wantSorted := []float64{0.1, 0.2, 0.3}
	for i, v := range wantSorted {
		if c.Sorted[i] != v {
			t.Errorf("sorted[%d] = %v, want %v", i, c.Sorted[i], v)
		}
	}
	// centering: floor(2/2)=1 leading NaN, then samples, then trailing NaN
	// -> [NaN, 0.1, 0.2, 0.3, NaN]
	if len(c.Centered) != 5 {
		t.Fatalf("centered len = %d, want 5", len(c.Centered))
	}
	if !nan(c.Centered[0]) || !nan(c.Centered[4]) {
		t.Errorf("centered ends should be NaN, got %v", c.Centered)
	}
	if c.Centered[1] != 0.1 || c.Centered[2] != 0.2 || c.Centered[3] != 0.3 {
		t.Errorf("centered middle = %v, want [_,0.1,0.2,0.3,_]", c.Centered)
	}
	if c.LossFraction() != 0.4 {
		t.Errorf("loss fraction = %v, want 0.4", c.LossFraction())
	}
}

func TestComputeNoLoss(t *testing.T) {
	c := Compute(4, []float64{0.04, 0.01, 0.03, 0.02})
	if c.Loss != 0 {
		t.Errorf("loss = %d, want 0", c.Loss)
	}
	if c.Median != 0.03 { // sorted[4/2] = sorted[2] = 0.03
		t.Errorf("median = %v, want 0.03", c.Median)
	}
	for i, v := range c.Centered {
		if nan(v) {
			t.Errorf("centered[%d] unexpectedly NaN with no loss", i)
		}
	}
}

func TestComputeTotalLoss(t *testing.T) {
	c := Compute(3, nil)
	if c.Loss != 3 {
		t.Errorf("loss = %d, want 3", c.Loss)
	}
	// A fully-lost round is 100% loss, never silently 0% (SmokePing's "U"->0%
	// trap): the alert engine must see this as a down target, not clean history.
	if c.LossFraction() != 1.0 {
		t.Errorf("loss fraction = %v, want 1.0", c.LossFraction())
	}
	if !nan(c.Median) {
		t.Errorf("median = %v, want NaN", c.Median)
	}
	if len(c.Centered) != 3 {
		t.Fatalf("centered len = %d, want 3", len(c.Centered))
	}
	for i, v := range c.Centered {
		if !nan(v) {
			t.Errorf("centered[%d] = %v, want NaN", i, v)
		}
	}
}

func TestComputeOddLoss(t *testing.T) {
	// 6 expected, 3 received (3 lost, odd). floor(3/2)=1 leading NaN,
	// 3 samples, then 6-1-3=2 trailing NaN.
	c := Compute(6, []float64{0.2, 0.1, 0.15})
	if c.Loss != 3 {
		t.Fatalf("loss = %d, want 3", c.Loss)
	}
	if !nan(c.Centered[0]) {
		t.Errorf("centered[0] should be NaN")
	}
	if c.Centered[1] != 0.1 || c.Centered[2] != 0.15 || c.Centered[3] != 0.2 {
		t.Errorf("centered samples misplaced: %v", c.Centered)
	}
	if !nan(c.Centered[4]) || !nan(c.Centered[5]) {
		t.Errorf("centered tail should be NaN: %v", c.Centered)
	}
}
