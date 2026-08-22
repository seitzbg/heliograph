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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
)

// Job is one probe/target measurement to run this round.
type Job struct {
	Probe   probe.Probe
	Target  probe.Target
	Pings   int
	Timeout time.Duration
	Step    time.Duration // per-target polling interval (drives the Planner)
	// Fingerprint is an opaque measurement-identity tag from the hub's assignment
	// (agent path only; empty for the hub's own local jobs). It flows through to the
	// Outcome so a buffered remote round can be attributed to the exact assignment
	// that produced it. See federation.Fingerprint / agentwire.
	Fingerprint string
}

// Outcome is the derived result of a Job.
type Outcome struct {
	Target      probe.Target
	ProbeName   string
	Computed    sample.Computed
	Err         error
	When        time.Time
	Duration    time.Duration
	Vantage     string // where this round was measured; "" means the hub (store.DefaultVantage)
	Fingerprint string // opaque tag copied from the Job (empty for local hub jobs)
	// Metric names what Computed's values mean: probe.MetricRTT (round-trip time; every probe but
	// NTP-offset) or probe.MetricOffset (a signed clock offset). Stored per row so history isn't
	// merged across a metric change and RTT-only consumers don't misread an offset as latency.
	Metric string
}

// roundBudget sizes a target's round timeout. A probe measures Pings samples
// sequentially, so the budget is Timeout*Pings — each ping gets ~Timeout — rather
// than a flat per-round Timeout, which would guillotine a slow-but-responding
// endpoint's later pings into false loss. It is capped by Step so a round can never
// overrun its own polling interval (and pile up). -timeout is therefore a per-ping
// budget. Pings==1 (or Step<=0) preserves the old behavior.
//
// This sizes the whole-round deadline; the sequential probes (TCPConnect/HTTP/DNS/SSH)
// further subdivide it per ping via probe.AttemptContext so one hung attempt can't
// consume the entire round.
func roundBudget(j Job) time.Duration {
	pings := j.Pings
	if pings < 1 {
		pings = 1
	}
	budget := j.Timeout * time.Duration(pings)
	if j.Step > 0 && j.Step < budget {
		budget = j.Step
	}
	return budget
}

// runOne measures a single job under its own timeout derived from ctx and derives
// its Outcome. Shared by RunRound and the Dispatcher.
func runOne(ctx context.Context, j Job) Outcome {
	jctx, cancel := context.WithTimeout(ctx, roundBudget(j))
	defer cancel()
	start := time.Now()
	res, err := safeMeasure(jctx, j)
	return Outcome{
		Target:      j.Target,
		ProbeName:   j.Probe.Name(),
		Computed:    sample.Compute(j.Pings, res.Samples),
		Err:         err,
		When:        start,
		Duration:    time.Since(start),
		Fingerprint: j.Fingerprint,
		Metric:      probe.NormalizeMetric(res.Kind),
	}
}

// safeMeasure calls j.Probe.Measure and recovers a panic, turning it into an
// ordinary error result instead of a process crash. The per-probe goroutines
// started by RunRound and Dispatcher.Go are not otherwise supervised, so a
// misbehaving probe (e.g. a config value that slips past validation and
// drives an oversized allocation) would otherwise take down the whole
// daemon instead of just failing its own round. The recovered value and the
// target name are both included so the failure is diagnosable from the
// Outcome's Err alone.
func safeMeasure(ctx context.Context, j Job) (res probe.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("probe %s target %q panicked: %v", j.Probe.Name(), j.Target.Name, r)
		}
	}()
	return j.Probe.Measure(ctx, j.Target, j.Pings)
}

// RunRound executes all jobs concurrently, capped at `workers` in flight, each
// under its own Timeout derived from the parent ctx. It returns one Outcome per
// job (index-aligned). It waits for every job to finish or time out — used for the
// initial synchronous rounds; the serving loop uses a Dispatcher instead.
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
			out[i] = runOne(ctx, jobs[i])
		}(i)
	}
	wg.Wait()
	return out
}

// BatchStat summarizes one Go call's jobs after they all finish.
type BatchStat struct {
	Ran      int           // jobs actually run (excludes ones skipped as in-flight)
	Errs     int           // how many errored
	Duration time.Duration // wall-clock until the last of them finished
}

// Dispatcher runs due jobs on a shared bounded worker pool WITHOUT blocking the
// caller — so the serving loop keeps ticking (and firing fast targets) while a slow
// target burns down its timeout in the background. Each job is released from the
// in-flight set the instant IT finishes (not when its batch does), so a fast target
// sharing a tick with a slow one is not held back, and a target still in flight is
// skipped rather than overlapping itself (review item #3a). The worker cap is shared
// across all in-flight jobs, so total concurrency stays bounded.
type Dispatcher struct {
	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

// NewDispatcher returns a Dispatcher whose in-flight probes are capped at workers.
func NewDispatcher(workers int) *Dispatcher {
	if workers <= 0 {
		workers = 1
	}
	return &Dispatcher{sem: make(chan struct{}, workers), inflight: map[string]bool{}}
}

// Go runs each job not already in flight on the shared pool. onEach is called with
// each outcome the moment it completes — so a target's samples are stored in order
// and a fast target isn't held up by a slow one sharing the tick. onBatch, if
// non-nil, is called once after all of THIS call's jobs finish, with the summary.
// Both callbacks run on background goroutines, so Go returns immediately. If every
// job was skipped as in-flight, neither callback fires. Either callback may be nil.
func (d *Dispatcher) Go(ctx context.Context, jobs []Job, onEach func(Outcome), onBatch func(BatchStat)) {
	var run []Job
	d.mu.Lock()
	for _, j := range jobs {
		if d.inflight[j.Target.Name] {
			continue
		}
		d.inflight[j.Target.Name] = true
		run = append(run, j)
	}
	d.mu.Unlock()
	if len(run) == 0 {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		start := time.Now()
		var errs atomic.Int32
		var jwg sync.WaitGroup
		for _, j := range run {
			jwg.Add(1)
			go func(j Job) {
				defer jwg.Done()
				d.sem <- struct{}{}
				o := runOne(ctx, j)
				<-d.sem // release the worker slot before the (store-write) callback
				if o.Err != nil {
					errs.Add(1)
				}
				// Keep the target in flight THROUGH onEach (store write + alert eval),
				// then release it. This serializes a target's result processing end to
				// end, so a slow store write can't let its next round's callback overtake
				// and reorder its alert history, and blocked callbacks stay bounded to one
				// per target (the in-flight guard) rather than piling up (CODE_REVIEW #1).
				if onEach != nil {
					onEach(o)
				}
				d.mu.Lock()
				delete(d.inflight, j.Target.Name)
				d.mu.Unlock()
			}(j)
		}
		jwg.Wait()
		if onBatch != nil {
			onBatch(BatchStat{Ran: len(run), Errs: int(errs.Load()), Duration: time.Since(start)})
		}
	}()
}

// Wait blocks until all in-flight jobs have finished — for graceful shutdown.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Planner tracks each target's next-fire time so targets can poll on their own
// per-target `Step` cadence rather than one global interval. A single caller
// drives it: on each wake it calls Tick to learn which targets are due now and
// how long to sleep before the next one is. Times are passed in, so it is pure
// and deterministic to test. Not safe for concurrent use (one driver goroutine).
type Planner struct {
	next map[string]time.Time     // target name -> next scheduled fire (phase-aligned)
	step map[string]time.Duration // last-seen Step; a change re-arms the target
}

func NewPlanner() *Planner {
	return &Planner{next: map[string]time.Time{}, step: map[string]time.Duration{}}
}

// Tick reconciles the planner against the current job set and returns the jobs
// due at `now` (advancing each to its next phase-aligned grid point) plus how
// long to sleep before the next job is due, capped at maxSleep so the caller
// wakes often enough to notice a config reload. A target new to the planner
// fires immediately; a target no longer present is forgotten. A job with a
// non-positive Step is treated as due every tick (defensive; config rejects it).
//
// When a target's Step changes (a SIGHUP reload retuned its cadence) its old
// deadline is discarded so the new cadence takes effect immediately, rather than
// leaving it deferred until the deadline computed under the previous step.
func (p *Planner) Tick(jobs []Job, now time.Time, maxSleep time.Duration) (due []Job, sleep time.Duration) {
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		name := j.Target.Name
		seen[name] = true
		if s, known := p.step[name]; known && s != j.Step {
			delete(p.next, name) // re-arm on a changed cadence
		}
		p.step[name] = j.Step
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
	for name := range p.step {
		if !seen[name] {
			delete(p.step, name)
		}
	}
	sleep = p.SleepToNext(now, maxSleep)
	return due, sleep
}

// SleepToNext returns how long until the earliest scheduled fire, capped at
// maxSleep. It does not mutate the planner, so the serving loop can call it after
// a round's probes complete: the sleep then reflects the real current time and
// keeps samples on their phase-aligned grid, instead of double-counting the probe
// duration by using a value computed before the round ran.
func (p *Planner) SleepToNext(now time.Time, maxSleep time.Duration) time.Duration {
	sleep := maxSleep
	for _, nt := range p.next {
		if d := nt.Sub(now); d < sleep {
			sleep = d
		}
	}
	if sleep < 0 {
		sleep = 0
	}
	return sleep
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
