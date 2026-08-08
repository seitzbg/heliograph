package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"smokeping-modern/internal/agentwire"
	"smokeping-modern/internal/scheduler"
)

// Options configures an Agent: which hub to talk to, which vantage it measures
// as, and the cadences/limits for polling, measuring, and buffering.
type Options struct {
	Hub      string
	Key      string
	Vantage  string
	Insecure bool

	Interval time.Duration // assignment poll interval
	Timeout  time.Duration // per-target probe timeout (and HTTP client timeout)

	Workers   int // max concurrent probes
	BufferCap int // bounded store-and-forward buffer capacity (rounds)
	FlushMax  int // max rounds per push
}

// Agent is the smoke-agent runtime: it polls a hub for its per-vantage
// assignment, measures the assigned targets on their own cadence via the
// shared scheduler, and pushes measured rounds back to the hub with a bounded
// buffer and retry/backoff.
type Agent struct {
	opts   Options
	client *Client
	buf    *buffer

	// jobs is the current runnable job set, built from the latest assignment.
	// It is written by the poll goroutine and read by the measure goroutine;
	// an atomic pointer avoids a lock on the hot measure-tick path.
	jobs atomic.Pointer[[]scheduler.Job]

	// measureDone is closed by measureLoop, exactly once, after it has fully
	// drained in-flight probes (disp.Wait()) on shutdown. flushLoop waits on
	// it (bounded) before its final flush, so a round completing during
	// shutdown is buffered before the last peek — otherwise it would land in
	// the buffer after finalFlush's one-time peek and be silently discarded.
	measureDone chan struct{}
}

// New builds an Agent from opts. It does not start any goroutines — call Run.
func New(opts Options) *Agent {
	a := &Agent{
		opts:        opts,
		client:      NewClient(opts.Hub, opts.Key, opts.Insecure, opts.Timeout),
		buf:         newBuffer(opts.BufferCap),
		measureDone: make(chan struct{}),
	}
	empty := []scheduler.Job{}
	a.jobs.Store(&empty)
	return a
}

// Run blocks until ctx is cancelled, running the poll, measure, and flush
// loops as goroutines coordinated by ctx and an internal WaitGroup. On
// shutdown it joins all three: the measure loop drains in-flight probes via
// Dispatcher.Wait and signals measureDone, the flush loop waits (bounded) for
// that signal and then makes one final best-effort push of whatever remains
// buffered — so a round completing during shutdown is captured, not dropped.
func (a *Agent) Run(ctx context.Context) error {
	slog.Info("smoke-agent starting", "hub", a.opts.Hub, "vantage", a.opts.Vantage, "interval", a.opts.Interval)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	go func() { defer wg.Done(); a.measureLoop(ctx) }()
	go func() { defer wg.Done(); a.flushLoop(ctx) }()
	wg.Wait()

	slog.Info("smoke-agent stopped", "dropped", a.buf.dropped())
	return nil
}

// pollLoop fetches the assignment immediately, then every Interval. A 304
// (changed=false) keeps the running job set; a transport/decode error is
// logged and the running job set is kept too — a hub blip must not blank out
// an agent's targets. On a real change, the assignment is validated into a
// fresh job set (invalid targets skipped + logged) and the ETag is advanced
// so the next poll can 304 again.
func (a *Agent) pollLoop(ctx context.Context) {
	var etag string
	t := time.NewTimer(0) // fire immediately
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			asg, changed, err := a.client.PullAssignment(ctx, etag)
			if err != nil {
				slog.Warn("assignment pull failed, keeping current jobs", "err", err)
			} else if changed {
				jobs, skipped := BuildJobs(asg.Targets, a.opts.Timeout)
				if len(skipped) > 0 {
					slog.Warn("assignment: skipped invalid targets", "skipped", skipped)
				}
				a.jobs.Store(&jobs)
				etag = asg.ConfigVersion
				slog.Info("assignment updated", "targets", len(jobs), "config_version", etag)
			}
			t.Reset(a.opts.Interval)
		}
	}
}

// measureLoop mirrors cmd/smoked's serve loop: a Planner tracks each target's
// per-target cadence, a Dispatcher runs due jobs without blocking the tick, and
// each outcome is buffered as it completes. maxSleep is capped at one second
// so a re-armed job set (from a fresh poll) is picked up promptly.
func (a *Agent) measureLoop(ctx context.Context) {
	disp := scheduler.NewDispatcher(a.opts.Workers)
	planner := scheduler.NewPlanner()
	const maxSleep = time.Second
	for {
		now := time.Now()
		jobs := *a.jobs.Load()
		due, _ := planner.Tick(jobs, now, maxSleep)
		if len(due) > 0 {
			disp.Go(ctx, due,
				func(o scheduler.Outcome) {
					a.buf.add(reportFromOutcome(o))
				},
				func(bs scheduler.BatchStat) {
					slog.Debug("measure batch complete", "targets", bs.Ran, "errors", bs.Errs,
						"duration_ms", float64(bs.Duration.Microseconds())/1000, "buffered", a.buf.len())
				})
		}
		sleep := planner.SleepToNext(time.Now(), maxSleep)
		select {
		case <-ctx.Done():
			disp.Wait()          // let in-flight probes finish and buffer their outcome
			close(a.measureDone) // signal flushLoop it's now safe to take the final peek
			return
		case <-time.After(sleep):
		}
	}
}

// flushLoop pushes buffered rounds to the hub in batches of up to FlushMax,
// with exponential backoff on failure (capped, and the batch is retained —
// never committed — so nothing is lost across a hub blip; the hub dedups on
// (target,vantage,ts) so a retried batch is safe to resend). On ctx.Done it
// waits (bounded) for the measure loop to finish draining in-flight probes —
// so a round that completes during shutdown is buffered before the final
// peek, not lost after it — then makes one final best-effort push with a
// short bounded context and returns.
func (a *Agent) flushLoop(ctx context.Context) {
	const (
		baseIdle      = 500 * time.Millisecond
		backoffStart  = time.Second
		backoffCap    = 30 * time.Second
		drainWait     = 5 * time.Second // bound on waiting for measureLoop to drain
		finalFlushTTL = 5 * time.Second
	)
	shutdown := func() {
		a.awaitMeasureDrain(drainWait)
		a.finalFlush(finalFlushTTL)
	}
	backoff := backoffStart
	for {
		select {
		case <-ctx.Done():
			shutdown()
			return
		default:
		}

		if a.buf.len() == 0 {
			select {
			case <-ctx.Done():
				shutdown()
				return
			case <-time.After(baseIdle):
			}
			continue
		}

		batch, upto := a.buf.peekBatch(a.opts.FlushMax)
		if _, err := a.client.PushResults(ctx, batch); err != nil {
			slog.Warn("push failed, will retry", "err", err, "batch", len(batch), "backoff", backoff)
			select {
			case <-ctx.Done():
				shutdown()
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > backoffCap {
				backoff = backoffCap
			}
			continue
		}
		a.buf.commit(upto)
		backoff = backoffStart
		slog.Debug("pushed results", "batch", len(batch), "remaining", a.buf.len(), "dropped", a.buf.dropped())
	}
}

// awaitMeasureDrain blocks until measureLoop has signaled it fully drained
// in-flight probes (every completed round is buffered), or until wait
// elapses — whichever comes first. The bound keeps shutdown from hanging
// if a probe ignores context cancellation; measureDone is already closed by
// the time this runs in the common case (disp.Wait() only needs to unwind
// well-behaved probes, which inherit ctx's cancellation), so the wait is a
// safety net, not the expected path.
func (a *Agent) awaitMeasureDrain(wait time.Duration) {
	select {
	case <-a.measureDone:
	case <-time.After(wait):
		slog.Warn("measure loop did not drain within bound, final flush may miss in-flight rounds", "wait", wait)
	}
}

// finalFlush makes one best-effort push of whatever remains buffered, bounded
// by a short fresh context (the parent ctx is already cancelled by the time
// this runs). It does not retry — this is shutdown, not the steady-state loop.
func (a *Agent) finalFlush(ttl time.Duration) {
	if a.buf.len() == 0 {
		return
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), ttl)
	defer cancel()
	batch, upto := a.buf.peekBatch(a.opts.FlushMax)
	if len(batch) == 0 {
		return
	}
	if _, err := a.client.PushResults(shutCtx, batch); err != nil {
		slog.Warn("final flush failed, rounds left unsent", "err", err, "batch", len(batch), "dropped", a.buf.dropped())
		return
	}
	a.buf.commit(upto)
	slog.Info("final flush complete", "batch", len(batch), "dropped", a.buf.dropped())
}

// reportFromOutcome converts a scheduler.Outcome (the shared probe/scheduler
// pipeline's result) into the wire RoundReport sent to the hub. The hub
// re-derives loss/median/centered via sample.Compute from RTTs, so only the
// raw received samples (Computed.Sorted) travel over the wire, not the
// agent's own derived stats.
func reportFromOutcome(o scheduler.Outcome) agentwire.RoundReport {
	errStr := ""
	if o.Err != nil {
		errStr = o.Err.Error()
	}
	return agentwire.RoundReport{
		Target:     o.Target.Name,
		TS:         o.When.UTC().Format(time.RFC3339Nano),
		Pings:      o.Computed.Pings,
		RTTs:       o.Computed.Sorted,
		Err:        errStr,
		DurationMs: float64(o.Duration.Microseconds()) / 1000,
	}
}
