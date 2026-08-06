package fping

import "testing"

type errStub struct{}

func (errStub) Error() string { return "boom" }

// fping exit 1 is ordinary loss (return the samples); any other non-zero exit is a
// real error even with partial samples — it must not be reported as success.
func TestInterpretExit(t *testing.T) {
	partial := []float64{0.01, 0.02}

	if r, err := interpretExit(1, errStub{}, partial, ""); err != nil || len(r.Samples) != 2 {
		t.Errorf("exit 1 with samples: got samples=%d err=%v, want 2 samples, nil err", len(r.Samples), err)
	}
	if r, err := interpretExit(1, errStub{}, nil, ""); err != nil || len(r.Samples) != 0 {
		t.Errorf("exit 1 empty: got err=%v, want total-loss (nil err, 0 samples)", err)
	}
	// Non-loss exit codes (and a start failure = -1) are errors regardless of samples.
	for _, code := range []int{2, 3, 4, -1} {
		if _, err := interpretExit(code, errStub{}, partial, "err text"); err == nil {
			t.Errorf("exit %d with partial samples: got success, want an error", code)
		}
	}
}
