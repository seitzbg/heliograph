package api

import (
	"testing"
	"time"
)

// A snapshot is one coherent read: total always reflects every observed round, while
// the latest-round fields come as a set from the most recent round by start time. An
// out-of-order (older) completion counts toward total but must not rewind those fields.
func TestRoundStatsSnapshot(t *testing.T) {
	var rs RoundStats
	if _, ok := rs.snapshot(); ok {
		t.Fatal("no round observed yet -> snapshot must be !ok")
	}

	t0 := time.Unix(1000, 0)
	rs.Observe(50*time.Millisecond, 8, 1, t0)
	snap, ok := rs.snapshot()
	if !ok {
		t.Fatal("snapshot should be ok after one Observe")
	}
	if snap.total != 1 || snap.targets != 8 || snap.errs != 1 || snap.duration != 50*time.Millisecond || snap.lastUnix != 1000 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// A later round replaces the latest-round fields as one coherent set.
	rs.Observe(70*time.Millisecond, 9, 0, t0.Add(time.Minute))
	snap, _ = rs.snapshot()
	if snap.total != 2 || snap.targets != 9 || snap.errs != 0 || snap.duration != 70*time.Millisecond || snap.lastUnix != t0.Add(time.Minute).Unix() {
		t.Fatalf("snapshot after 2nd = %+v", snap)
	}

	// An out-of-order (older) completion counts toward total but must NOT regress the
	// latest-round fields — a scrape must never see a timestamp/duration move backwards.
	rs.Observe(10*time.Millisecond, 3, 2, t0) // older start than the 2nd round
	snap, _ = rs.snapshot()
	if snap.total != 3 {
		t.Errorf("total = %d, want 3 (every round counts)", snap.total)
	}
	if snap.lastUnix != t0.Add(time.Minute).Unix() || snap.targets != 9 || snap.errs != 0 || snap.duration != 70*time.Millisecond {
		t.Errorf("older completion regressed the latest-round fields: %+v", snap)
	}
}

// A nil *RoundStats is a no-op (the pure-API path leaves Rounds nil).
func TestRoundStatsNil(t *testing.T) {
	var rs *RoundStats
	rs.Observe(time.Second, 1, 0, time.Unix(1, 0)) // must not panic
	if _, ok := rs.snapshot(); ok {
		t.Error("nil RoundStats snapshot must be !ok")
	}
}
