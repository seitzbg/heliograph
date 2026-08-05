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
	Step    time.Duration // per-target polling interval (drives the Planner)
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

// Planner tracks each target's next-fire time so targets can poll on their own
// per-target `Step` cadence rather than one global interval. A single caller
// drives it: on each wake it calls Tick to learn which targets are due now and
// how long to sleep before the next one is. Times are passed in, so it is pure
// and deterministic to test. Not safe for concurrent use (one driver goroutine).
type Planner struct {
	next map[string]time.Time // target name -> next scheduled fire (phase-aligned)
}

func NewPlanner() *Planner { return &Planner{next: map[string]time.Time{}} }

// Tick reconciles the planner against the current job set and returns the jobs
// due at `now` (advancing each to its next phase-aligned grid point) plus how
// long to sleep before the next job is due, capped at maxSleep so the caller
// wakes often enough to notice a config reload. A target new to the planner
// fires immediately; a target no longer present is forgotten. A job with a
// non-positive Step is treated as due every tick (defensive; config rejects it).
func (p *Planner) Tick(jobs []Job, now time.Time, maxSleep time.Duration) (due []Job, sleep time.Duration) {
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		name := j.Target.Name
		seen[name] = true
		nt, known := p.next[name]
		if !known || !nt.After(now) {
			due = append(due, j)
			p.next[name] = now.Add(NextDelay(now, j.Step, 0))
		}
	}
	for name := range p.next {
		if !seen[name] {
			delete(p.next, name)
		}
	}
	sleep = maxSleep
	for _, j := range jobs {
		if d := p.next[j.Target.Name].Sub(now); d < sleep {
			sleep = d
		}
	}
	if sleep < 0 {
		sleep = 0
	}
	return due, sleep
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
