// Package scheduler runs measurement rounds fast and in parallel with strict
// per-target isolation: a bounded worker pool executes probes concurrently, each
// under its own timeout, so one slow or hung target never blocks the others.
//
// This replaces both SmokePing concurrency layers at once — the process-per-probe
// supervisor and basefork's per-round fork pool (see codemap 01 §2) — with a
// single goroutine pool.
package scheduler

import (
	"context"
	"sync"
	"time"

	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/sample"
)

// Job is one probe/target measurement to run this round.
type Job struct {
	Probe   probe.Probe
	Target  probe.Target
	Pings   int
	Timeout time.Duration
}

// Outcome is the derived result of a Job.
type Outcome struct {
	Target    probe.Target
	ProbeName string
	Computed  sample.Computed
	Err       error
	When      time.Time
	Duration  time.Duration
}

// RunRound executes all jobs concurrently, capped at `workers` in flight, each
// under its own Timeout derived from the parent ctx. It returns one Outcome per
// job (index-aligned). It waits for every job to finish or time out.
func RunRound(ctx context.Context, jobs []Job, workers int) []Outcome {
	if workers <= 0 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	out := make([]Outcome, len(jobs))
	var wg sync.WaitGroup

	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			j := jobs[i]
			jctx, cancel := context.WithTimeout(ctx, j.Timeout)
			defer cancel()

			start := time.Now()
			res, err := j.Probe.Measure(jctx, j.Target, j.Pings)
			out[i] = Outcome{
				Target:    j.Target,
				ProbeName: j.Probe.Name(),
				Computed:  sample.Compute(j.Pings, res.Samples),
				Err:       err,
				When:      start,
				Duration:  time.Since(start),
			}
		}(i)
	}
	wg.Wait()
	return out
}

// NextDelay returns how long to sleep so the next round lands on a fixed
// wall-clock grid of period `step`, shifted by `offset`. This reproduces
// SmokePing's phase-aligned scheduler: sleeptime = step - ((now-offset) mod step)
// (see codemap 01 §2), which keeps samples on aligned timestamps regardless of
// how long a round takes, and lets per-probe offsets stagger network bursts.
func NextDelay(now time.Time, step, offset time.Duration) time.Duration {
	if step <= 0 {
		return 0
	}
	n := now.UnixNano() - int64(offset)
	m := n % int64(step)
	if m < 0 {
		m += int64(step)
	}
	return time.Duration(int64(step) - m)
}
