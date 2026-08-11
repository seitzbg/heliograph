package agent

import (
	"context"
	"errors"
	"fmt"
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

// Validate rejects an Options that would make an unusable or dangerous agent — the
// package-boundary guard so a non-CLI caller can't construct one. In particular a
// non-positive FlushMax panics peekBatch's make([], FlushMax) as soon as data is buffered,
// and a non-positive Interval hot-loops the poller (CODE_REVIEW #9 / P2-9). Run calls this
// before starting any goroutine.
func (o Options) Validate() error {
	switch {
	case o.Hub == "":
		return errors.New("agent: hub is required")
	case o.Key == "":
		return errors.New("agent: key is required")
	case o.Interval <= 0:
		return fmt.Errorf("agent: interval must be positive, got %s", o.Interval)
	case o.Timeout <= 0:
		return fmt.Errorf("agent: timeout must be positive, got %s", o.Timeout)
	case o.Workers < 1:
		return fmt.Errorf("agent: workers must be at least 1, got %d", o.Workers)
	case o.BufferCap < 1:
		return fmt.Errorf("agent: buffer capacity must be at least 1, got %d", o.BufferCap)
	case o.FlushMax < 1:
		return fmt.Errorf("agent: flush max must be at least 1, got %d", o.FlushMax)
	case o.FlushMax > agentwire.MaxResultsRounds:
		// A larger batch is always rejected by the hub (its per-request cap), which would wedge
		// the flush loop retrying a never-acceptable batch forever (CODE_REVIEW #2).
		return fmt.Errorf("agent: flush max must be at most %d (the hub's per-request limit), got %d", agentwire.MaxResultsRounds, o.FlushMax)
	}
	return nil
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
	if err := a.opts.Validate(); err != nil {
		return err
	}
	slog.Info("smoke-agent starting", "hub", a.opts.Hub, "vantage", a.opts.Vantage, "interval", a.opts.Interval)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.pollLoop(ctx) }()
	go func() { defer wg.Done(); a.measureLoop(ctx) }()
	go func() { defer wg.Done(); a.flushLoop(ctx) }()
	wg.Wait()

	slog.Info("smoke-agent stopped", "dropped", a.buf.dropped(), "rejected", a.buf.rejected())
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
	var vantageChecked bool // warn at most once if the configured vantage label disagrees with the hub's
	t := time.NewTimer(0)   // fire immediately
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
				// The hub derives the vantage from the API key and returns it here; the
				// configured `vantage` is only a label. Warn once if the operator's label
				// disagrees (e.g. a key minted for a different vantage), so the logs can't
				// quietly attribute this agent's rounds to the wrong vantage.
				if !vantageChecked && a.opts.Vantage != "" && a.opts.Vantage != asg.Vantage {
					slog.Warn("configured vantage does not match the hub-assigned vantage; the hub's key-derived value wins",
						"configured", a.opts.Vantage, "assigned", asg.Vantage)
				}
				vantageChecked = true
				a.jobs.Store(&jobs)
				etag = asg.ConfigVersion
				slog.Info("assignment updated", "vantage", asg.Vantage, "targets", len(jobs), "config_version", etag)
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

// sendBatch pushes rounds to the hub, splitting on a size (413) rejection so that a
// single oversized round doesn't sink the rest of an otherwise fully-sendable batch — a
// 413 is a recoverable SIZE condition, not a verdict that every round in the batch is
// unsendable (CODE_REVIEW round-9 #1). It returns sent (rounds successfully POSTed) and
// dropped (rounds that are individually unsendable and must be counted rejected), and has
// NO side effects on a.buf — the caller applies (commit + reject) both together, and only
// once sendBatch returns err == nil for the top-level call.
//
// That "only when err == nil" rule matters because buf.commit is all-or-nothing for a
// peeked batch (it has no notion of partially handling one): if a TRANSIENT error is hit
// anywhere during the recursive split, sendBatch propagates it immediately and the caller
// must not commit or reject anything from this peek — the whole original batch is left
// buffered to retry as a whole next cycle, exactly the pre-existing transient-retry
// contract. Any sub-batches that were already successfully POSTed before the transient
// hit will be resent on that retry, which is safe because the hub dedups incoming rounds
// on (target, vantage, ts).
//
// Splitting only happens on a size rejection of a batch with more than one round — a
// non-size permanent rejection (400 malformed) drops the WHOLE input, unsplit, exactly as
// before (a malformed-batch verdict from the hub's decoder isn't fixed by trying smaller
// pieces of the same shape). A size rejection of a single round means that round alone
// exceeds the hub's byte cap and can never be sent at any batch size: it is dropped and
// counted, not retried.
//
// Termination: a split of a batch of length n>1 yields two halves of length in [1, n-1]
// (integer-division midpoint), each strictly smaller than n, and a batch of length 1 never
// recurses further (it is sent, dropped, or its error is propagated) — so recursion depth
// is bounded by ceil(log2(len(rounds))) and always terminates.
func (a *Agent) sendBatch(ctx context.Context, rounds []agentwire.RoundReport) (sent, dropped int, err error) {
	if len(rounds) == 0 {
		return 0, 0, nil
	}
	_, pushErr := a.client.PushResults(ctx, rounds)
	if pushErr == nil {
		return len(rounds), 0, nil
	}
	var pe *pushError
	if !errors.As(pushErr, &pe) || !pe.permanent() {
		return 0, 0, pushErr // transient (or non-pushError, e.g. transport/ctx): caller retries the whole batch later
	}
	if pe.oversize() && len(rounds) > 1 {
		mid := len(rounds) / 2
		s1, d1, err1 := a.sendBatch(ctx, rounds[:mid])
		if err1 != nil {
			return 0, 0, err1
		}
		s2, d2, err2 := a.sendBatch(ctx, rounds[mid:])
		if err2 != nil {
			return 0, 0, err2
		}
		return s1 + s2, d1 + d2, nil
	}
	// A malformed (400) batch, or a single round that alone exceeds the byte cap: unsendable,
	// drop it (counted by the caller).
	return 0, len(rounds), nil
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

		batch, upto := a.buf.peekBatch(a.opts.FlushMax, flushByteBudget)
		sent, dropped, err := a.sendBatch(ctx, batch)
		if err != nil {
			// A transient failure somewhere in the (possibly split) push: nothing in this
			// peek is committed, so the whole batch — including any sub-batches sendBatch
			// already delivered — is retried as a whole next cycle (safe: the hub dedups on
			// (target,vantage,ts)).
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
		if dropped > 0 {
			// A permanently-unsendable subset (oversized round(s), or a malformed batch) was
			// identified within this peek. Retrying it forever would wedge the queue behind
			// it and block every newer round — so drop it (loud + counted) and keep draining
			// (CODE_REVIEW #2 / round-9 #1: the REST of the batch, if any, was already sent
			// above by sendBatch's split, not dropped with it).
			a.buf.reject(dropped)
			slog.Warn("dropping unsendable results (oversized round(s) or malformed batch)",
				"sent", sent, "dropped", dropped, "rejected_total", a.buf.rejected())
		}
		backoff = backoffStart
		slog.Debug("pushed results", "batch", len(batch), "sent", sent, "dropped", dropped,
			"remaining", a.buf.len(), "dropped_overflow", a.buf.dropped())
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

// finalFlush makes a best-effort push of EVERYTHING still buffered at shutdown,
// looping over FlushMax-sized batches until the buffer is empty, the fresh shutdown
// context expires, or a push fails (the parent ctx is already cancelled by the time
// this runs). It does not retry a transient failure — this is shutdown, not the
// steady-state loop — but it no longer stops after a single batch, which used to strand
// up to (buffer - FlushMax) rounds on a controlled shutdown after a hub outage
// (CODE_REVIEW #8), and a size-rejected batch is split (via sendBatch) rather than
// dropped wholesale (CODE_REVIEW round-9 #1).
func (a *Agent) finalFlush(ttl time.Duration) {
	if a.buf.len() == 0 {
		return
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), ttl)
	defer cancel()
	sent, rejected := 0, 0
	for {
		if err := shutCtx.Err(); err != nil {
			slog.Warn("final flush deadline exceeded, rounds left unsent",
				"sent", sent, "remaining", a.buf.len(), "dropped", a.buf.dropped())
			return
		}
		batch, upto := a.buf.peekBatch(a.opts.FlushMax, flushByteBudget)
		if len(batch) == 0 {
			break // buffer drained
		}
		s, dropped, err := a.sendBatch(shutCtx, batch)
		if err != nil {
			// Transient: this is shutdown, not the steady-state loop, so it is not retried —
			// nothing in this peek is committed, and whatever sendBatch already delivered
			// before the transient hit will simply not be resent (best-effort, matches the
			// pre-existing "final flush failed, rounds left unsent" contract).
			slog.Warn("final flush failed, rounds left unsent",
				"err", err, "sent", sent, "remaining", a.buf.len(), "dropped", a.buf.dropped())
			return
		}
		a.buf.commit(upto)
		sent += s
		if dropped > 0 {
			// A permanently-unsendable subset (oversized round(s), or a malformed batch) must
			// not strand the sendable rounds behind it: drop it (counted) and keep draining,
			// mirroring flushLoop (CODE_REVIEW #2 / round-9 #1).
			a.buf.reject(dropped)
			rejected += dropped
			slog.Warn("final flush dropping unsendable results (oversized round(s) or malformed batch)",
				"dropped", dropped, "rejected_total", a.buf.rejected())
		}
	}
	slog.Info("final flush complete", "sent", sent, "rejected", rejected, "dropped", a.buf.dropped())
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
		Target:      o.Target.Name,
		TS:          o.When.UTC().Format(time.RFC3339Nano),
		Pings:       o.Computed.Pings,
		RTTs:        o.Computed.Sorted,
		Err:         errStr,
		Fingerprint: o.Fingerprint,
		DurationMs:  float64(o.Duration.Microseconds()) / 1000,
	}
}
