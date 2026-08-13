package fping

import (
	"strconv"
	"testing"
	"time"
)

type errStub struct{}

func (errStub) Error() string { return "boom" }

// flagVal returns the argument following flag in args, or "".
func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestFpingArgsSpreadsPings: the N pings must be SPREAD OUT (a per-send period) and
// each reply given its own timeout (-t), not crammed into a ~1s burst. A 50ms burst
// measured ~4x the real loss on a marginal link vs SmokePing's spread-out pings.
func TestFpingArgsSpreadsPings(t *testing.T) {
	args := fpingArgs("h", 20, 60*time.Second, "56", 0, 0)
	if flagVal(args, "-C") != "20" {
		t.Errorf("-C = %q, want 20", flagVal(args, "-C"))
	}
	p, err := strconv.Atoi(flagVal(args, "-p"))
	if err != nil || p < 200 {
		t.Errorf("-p = %q ms, want spread out (>=200ms), not a ~50ms burst", flagVal(args, "-p"))
	}
	if flagVal(args, "-t") == "" {
		t.Error("-t (per-reply timeout) is missing; a lost/slow reply needs a bounded wait")
	}
	if flagVal(args, "-b") != "56" {
		t.Errorf("-b = %q, want 56", flagVal(args, "-b"))
	}
	if len(args) == 0 || args[len(args)-1] != "h" {
		t.Errorf("host should be the last arg, got %v", args)
	}
}

// TestFpingArgsFitBudget: (pings-1)*period + timeout must fit the round budget, so a
// short step never gets the fping call truncated (which would itself cause loss).
func TestFpingArgsFitBudget(t *testing.T) {
	for _, budget := range []time.Duration{2 * time.Second, 10 * time.Second, 60 * time.Second} {
		args := fpingArgs("h", 20, budget, "", 0, 0)
		p, _ := strconv.Atoi(flagVal(args, "-p"))
		to, _ := strconv.Atoi(flagVal(args, "-t"))
		run := time.Duration(19*p+to) * time.Millisecond
		if run > budget {
			t.Errorf("budget %v: fping run ~%v exceeds it (-p=%dms -t=%dms)", budget, run, p, to)
		}
	}
}

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
