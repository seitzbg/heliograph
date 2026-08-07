package api

import (
	"sync/atomic"
	"time"
)

// RoundStats holds operational counters for measurement rounds — the coarse
// "how is the collector itself doing" signal (round wall-clock, size, error
// count) that complements the per-target series. The collector loop calls
// Observe after each round; the /metrics handler renders the latest snapshot.
// The zero value is ready to use and safe for concurrent access.
//
// Reads are one coherent snapshot published behind a single atomic pointer, so a
// scrape never mixes fields from two different rounds. Overlapping batches can finish
// out of start order, so an older completion increments the total but does not rewind
// the latest-round fields (CODE_REVIEW round-completion/snapshot consistency).
type RoundStats struct {
	total atomic.Int64                  // rounds completed since start (a monotonic counter)
	last  atomic.Pointer[roundSnapshot] // the most recent round's fields, as one coherent set
}

// roundSnapshot is an immutable, coherent read of the counters for rendering.
type roundSnapshot struct {
	total     int64
	duration  time.Duration
	targets   int64
	errs      int64
	lastUnix  int64 // round start, whole seconds — exported as the metric
	lastNanos int64 // round start, nanoseconds — used only for ordering (seconds tie within a second)
}

// Observe records one completed round. The total always increments; the latest-round
// fields are replaced only when this round's start is at or after the currently
// published one, so a batch that finishes after a newer one (out-of-order completion)
// cannot rewind the reported round.
func (rs *RoundStats) Observe(d time.Duration, targets, errs int, when time.Time) {
	if rs == nil {
		return
	}
	total := rs.total.Add(1)
	whenNanos := when.UnixNano()
	next := &roundSnapshot{
		total:     total,
		duration:  d,
		targets:   int64(targets),
		errs:      int64(errs),
		lastUnix:  when.Unix(),
		lastNanos: whenNanos,
	}
	for {
		cur := rs.last.Load()
		if cur != nil && whenNanos < cur.lastNanos {
			return // older completion: counts toward total (already added), publishes nothing
		}
		if rs.last.CompareAndSwap(cur, next) {
			return
		}
		// lost the race with a concurrent Observe; reload cur and retry.
	}
}

func (rs *RoundStats) snapshot() (roundSnapshot, bool) {
	if rs == nil {
		return roundSnapshot{}, false
	}
	last := rs.last.Load()
	if last == nil {
		return roundSnapshot{}, false // no round has completed yet
	}
	snap := *last
	snap.total = rs.total.Load() // authoritative count, even if the latest publish was an older round
	return snap, true
}
