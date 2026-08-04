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
type RoundStats struct {
	total       atomic.Int64 // rounds completed since start (a counter)
	durationNs  atomic.Int64 // wall-clock of the most recent round
	targetsLast atomic.Int64 // jobs run in the most recent round
	errsLast    atomic.Int64 // jobs that errored in the most recent round
	lastUnixSec atomic.Int64 // start time of the most recent round (unix seconds)
}

// Observe records one completed round.
func (rs *RoundStats) Observe(d time.Duration, targets, errs int, when time.Time) {
	if rs == nil {
		return
	}
	rs.total.Add(1)
	rs.durationNs.Store(int64(d))
	rs.targetsLast.Store(int64(targets))
	rs.errsLast.Store(int64(errs))
	rs.lastUnixSec.Store(when.Unix())
}

// roundSnapshot is an immutable read of the counters for rendering.
type roundSnapshot struct {
	total    int64
	duration time.Duration
	targets  int64
	errs     int64
	lastUnix int64
}

func (rs *RoundStats) snapshot() (roundSnapshot, bool) {
	if rs == nil || rs.total.Load() == 0 {
		return roundSnapshot{}, false // no round has completed yet
	}
	return roundSnapshot{
		total:    rs.total.Load(),
		duration: time.Duration(rs.durationNs.Load()),
		targets:  rs.targetsLast.Load(),
		errs:     rs.errsLast.Load(),
		lastUnix: rs.lastUnixSec.Load(),
	}, true
}
