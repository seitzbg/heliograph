package scheduler

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// fakeProbe sleeps `delay` per ping, then reports `rtt` seconds. It honors ctx
// cancellation between pings, so a long delay under a short timeout yields loss.
type fakeProbe struct {
	name  string
	delay time.Duration
	rtt   float64
}

func (f *fakeProbe) Name() string                     { return f.name }
func (f *fakeProbe) Describe() string                 { return f.name }
func (f *fakeProbe) Schema() map[string]probe.VarSpec { return nil }
func (f *fakeProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	var samples []float64
	for i := 0; i < pings; i++ {
		select {
		case <-ctx.Done():
			return probe.Result{Samples: samples}, ctx.Err()
		case <-time.After(f.delay):
		}
		samples = append(samples, f.rtt)
	}
	return probe.Result{Samples: samples}, nil
}

// deadlineProbe records the ctx budget (time until deadline) it was handed, then
// returns immediately — for asserting how the scheduler sizes the round timeout.
type deadlineProbe struct {
	budget time.Duration
	ok     bool
}

func (d *deadlineProbe) Name() string { return "deadline" }
func (d *deadlineProbe) Measure(ctx context.Context, _ probe.Target, _ int) (probe.Result, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.budget = time.Until(dl)
		d.ok = true
	}
	return probe.Result{Samples: []float64{0.01}}, nil
}

// TestRoundBudgetScalesWithPings: a probe measures N pings sequentially, so the
// round budget must be Timeout*Pings (each ping gets ~Timeout) rather than a flat
// per-round Timeout — otherwise a slow-but-responding endpoint has its later pings
// guillotined into false loss. Step caps it so a round never overruns its own
// polling interval.
func TestRoundBudgetScalesWithPings(t *testing.T) {
	// 100ms/ping x 5 pings = ~500ms; Step (10s) does not cap.
	p := &deadlineProbe{}
	RunRound(context.Background(), []Job{{Probe: p, Target: probe.Target{Name: "x"}, Pings: 5, Timeout: 100 * time.Millisecond, Step: 10 * time.Second}}, 1)
	if !p.ok {
		t.Fatal("probe got no ctx deadline")
	}
	if p.budget < 400*time.Millisecond || p.budget > 600*time.Millisecond {
		t.Fatalf("round budget = %v, want ~500ms (100ms x 5 pings)", p.budget)
	}

	// Step caps it: 100ms x 100 pings = 10s, but Step=1s caps to ~1s.
	c := &deadlineProbe{}
	RunRound(context.Background(), []Job{{Probe: c, Target: probe.Target{Name: "y"}, Pings: 100, Timeout: 100 * time.Millisecond, Step: time.Second}}, 1)
	if !c.ok {
		t.Fatal("capped probe got no ctx deadline")
	}
	if c.budget < 800*time.Millisecond || c.budget > 1100*time.Millisecond {
		t.Fatalf("capped round budget = %v, want ~1s (Step cap)", c.budget)
	}
}

// TestParallelism: 8 jobs that each take ~200ms complete in well under the
// 8*200ms=1.6s a serial run would need, when 8 workers run concurrently.
func TestParallelism(t *testing.T) {
	const n = 8
	jobs := make([]Job, n)
	for i := range jobs {
		jobs[i] = Job{
			Probe:   &fakeProbe{name: "fake", delay: 200 * time.Millisecond, rtt: 0.01},
			Target:  probe.Target{Name: "t", Host: "h"},
			Pings:   1,
			Timeout: 2 * time.Second,
		}
	}
	start := time.Now()
	out := RunRound(context.Background(), jobs, n)
	elapsed := time.Since(start)

	if elapsed > 800*time.Millisecond {
		t.Errorf("8 parallel 200ms jobs took %v; expected ~200ms (not serialized)", elapsed)
	}
	for i, o := range out {
		if o.Err != nil {
			t.Errorf("job %d errored: %v", i, o.Err)
		}
		if o.Computed.Loss != 0 {
			t.Errorf("job %d loss = %d, want 0", i, o.Computed.Loss)
		}
	}
}

// TestFingerprintPropagates: a Job's opaque Fingerprint tag must reach its Outcome
// unchanged, on both the success and the timeout/error path — that's what lets a
// buffered remote round be attributed to the exact assignment that produced it.
// runOne is shared by RunRound and Dispatcher.Go, so covering RunRound covers both.
func TestFingerprintPropagates(t *testing.T) {
	jobs := []Job{
		{Probe: &fakeProbe{name: "ok", delay: time.Millisecond, rtt: 0.01}, Target: probe.Target{Name: "a"}, Pings: 1, Timeout: time.Second, Fingerprint: "sha256:aaa"},
		{Probe: &fakeProbe{name: "hung", delay: time.Second, rtt: 0.5}, Target: probe.Target{Name: "b"}, Pings: 1, Timeout: 20 * time.Millisecond, Fingerprint: "sha256:bbb"},
		{Probe: &fakeProbe{name: "none", delay: time.Millisecond, rtt: 0.01}, Target: probe.Target{Name: "c"}, Pings: 1, Timeout: time.Second}, // empty tag stays empty
	}
	out := RunRound(context.Background(), jobs, 4)
	want := []string{"sha256:aaa", "sha256:bbb", ""}
	for i, o := range out {
		if o.Fingerprint != want[i] {
			t.Errorf("job %d Outcome.Fingerprint = %q, want %q", i, o.Fingerprint, want[i])
		}
	}
}

// TestIsolation: one target hangs (5s) while three are fast (5ms). With a 300ms
// per-job timeout, the round finishes promptly, the fast targets succeed, and
// only the hung one records loss/error — proving one slow target can't block
// the others.
func TestIsolation(t *testing.T) {
	jobs := []Job{
		{Probe: &fakeProbe{name: "hung", delay: 5 * time.Second, rtt: 0.5}, Target: probe.Target{Name: "hung"}, Pings: 3, Timeout: 300 * time.Millisecond},
		{Probe: &fakeProbe{name: "fast", delay: 5 * time.Millisecond, rtt: 0.02}, Target: probe.Target{Name: "a"}, Pings: 3, Timeout: 300 * time.Millisecond},
		{Probe: &fakeProbe{name: "fast", delay: 5 * time.Millisecond, rtt: 0.02}, Target: probe.Target{Name: "b"}, Pings: 3, Timeout: 300 * time.Millisecond},
		{Probe: &fakeProbe{name: "fast", delay: 5 * time.Millisecond, rtt: 0.02}, Target: probe.Target{Name: "c"}, Pings: 3, Timeout: 300 * time.Millisecond},
	}
	start := time.Now()
	out := RunRound(context.Background(), jobs, 4)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("round took %v; hung target should have capped it near 300ms", elapsed)
	}
	// index 0 = hung
	if out[0].Err == nil {
		t.Errorf("hung target should have errored (timeout)")
	}
	if out[0].Computed.Loss != 3 {
		t.Errorf("hung target loss = %d, want 3", out[0].Computed.Loss)
	}
	for i := 1; i < 4; i++ {
		if out[i].Err != nil {
			t.Errorf("fast target %d errored: %v", i, out[i].Err)
		}
		if out[i].Computed.Loss != 0 {
			t.Errorf("fast target %d loss = %d, want 0", i, out[i].Computed.Loss)
		}
	}
}

// gateProbe blocks in Measure until gate is closed, tracking how many times it was
// called and the peak concurrency observed — for testing the Dispatcher.
type gateProbe struct {
	gate    chan struct{}
	calls   atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
}

func (g *gateProbe) Name() string                     { return "gate" }
func (g *gateProbe) Describe() string                 { return "gate" }
func (g *gateProbe) Schema() map[string]probe.VarSpec { return nil }
func (g *gateProbe) Measure(ctx context.Context, _ probe.Target, _ int) (probe.Result, error) {
	g.calls.Add(1)
	r := g.running.Add(1)
	for {
		p := g.peak.Load()
		if r <= p || g.peak.CompareAndSwap(p, r) {
			break
		}
	}
	defer g.running.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return probe.Result{}, ctx.Err()
	}
	return probe.Result{Samples: []float64{0.01}}, nil
}

func gateJob(name string, g *gateProbe) Job {
	return Job{Probe: g, Target: probe.Target{Name: name}, Pings: 1, Timeout: 2 * time.Second}
}

// A slow job in flight must not block a later dispatch of a fast job — the whole
// point of #3a (a slow target no longer stalls faster ones across ticks).
func TestDispatcherNonBlocking(t *testing.T) {
	d := NewDispatcher(4)
	slow := &gateProbe{gate: make(chan struct{})}
	fast := &gateProbe{gate: make(chan struct{})}
	close(fast.gate) // fast returns immediately

	d.Go(context.Background(), []Job{gateJob("slow", slow)}, func(Outcome) {}, nil)

	done := make(chan Outcome, 1)
	d.Go(context.Background(), []Job{gateJob("fast", fast)}, func(o Outcome) { done <- o }, nil)

	select {
	case o := <-done:
		if o.Computed.Loss != 0 {
			t.Errorf("fast outcome = %+v, want no loss", o)
		}
	case <-time.After(time.Second):
		t.Fatal("fast job did not complete while a slow job was in flight (dispatcher blocked)")
	}
	close(slow.gate)
	d.Wait()
}

// The key fix: a fast job sharing a batch with a slow one is released the instant
// IT finishes, so it can be dispatched again before the slow job completes — it is
// not held in-flight for the batch's (slow) duration.
func TestDispatcherPerJobRelease(t *testing.T) {
	d := NewDispatcher(4)
	slow := &gateProbe{gate: make(chan struct{})}
	fast := &gateProbe{gate: make(chan struct{})}
	close(fast.gate)

	d.Go(context.Background(), []Job{gateJob("fast", fast), gateJob("slow", slow)}, func(Outcome) {}, nil)

	// While the slow job is still blocked, the fast target must become re-dispatchable
	// (it's released per-job, not held for the batch's slow member). Poll: a dispatch
	// while fast is briefly still in flight is a no-op, so retry until it runs.
	ran := make(chan struct{}, 1)
	ok := false
	for i := 0; i < 200 && !ok; i++ {
		d.Go(context.Background(), []Job{gateJob("fast", fast)}, func(Outcome) {
			select {
			case ran <- struct{}{}:
			default:
			}
		}, nil)
		select {
		case <-ran:
			ok = true
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !ok {
		t.Fatal("fast target never became re-dispatchable while the slow job was in flight — per-job release broken")
	}
	close(slow.gate)
	d.Wait()
}

// A target stays in flight until its onEach callback (store write + alert eval)
// has finished, so a slow store write can't let a second callback for the same
// target overtake and reorder its alert history (CODE_REVIEW #1).
func TestDispatcherHoldsInFlightThroughCallback(t *testing.T) {
	d := NewDispatcher(4)
	g := &gateProbe{gate: make(chan struct{})}
	close(g.gate) // measurement returns immediately; the callback is what blocks

	entered := make(chan struct{}, 1)
	proceed := make(chan struct{})
	d.Go(context.Background(), []Job{gateJob("x", g)}, func(Outcome) {
		entered <- struct{}{}
		<-proceed // simulate a slow store write inside onEach
	}, nil)
	<-entered // x measured; its onEach is now blocking

	ran := make(chan struct{}, 1)
	d.Go(context.Background(), []Job{gateJob("x", g)}, func(Outcome) { ran <- struct{}{} }, nil)
	select {
	case <-ran:
		t.Fatal("second callback for x ran while the first was still processing — overlap/reorder")
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	d.Wait()
	if n := g.calls.Load(); n != 1 {
		t.Errorf("gate probe called %d times, want 1 (the overlap was skipped)", n)
	}
}

// A target already in flight is skipped when dispatched again, so a slow target
// never overlaps itself.
func TestDispatcherInFlightGuard(t *testing.T) {
	d := NewDispatcher(4)
	g := &gateProbe{gate: make(chan struct{})}
	d.Go(context.Background(), []Job{gateJob("x", g)}, func(Outcome) {}, nil)
	// give the first dispatch a moment to mark x in flight
	for i := 0; i < 100 && g.running.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	called := make(chan struct{}, 1)
	d.Go(context.Background(), []Job{gateJob("x", g)}, func(Outcome) { called <- struct{}{} }, nil)
	select {
	case <-called:
		t.Fatal("second dispatch of an in-flight target should be skipped, not run")
	case <-time.After(100 * time.Millisecond):
	}
	close(g.gate)
	d.Wait()
	if n := g.calls.Load(); n != 1 {
		t.Errorf("gate probe called %d times, want 1 (the overlap was skipped)", n)
	}
}

// Concurrency is capped by the worker count across all in-flight jobs, and every
// job's outcome is delivered.
func TestDispatcherConcurrencyCap(t *testing.T) {
	d := NewDispatcher(2)
	g := &gateProbe{gate: make(chan struct{})}
	jobs := []Job{gateJob("a", g), gateJob("b", g), gateJob("c", g), gateJob("d", g), gateJob("e", g)}
	var got atomic.Int32
	var batches atomic.Int32
	d.Go(context.Background(), jobs, func(Outcome) { got.Add(1) }, func(BatchStat) { batches.Add(1) })
	// wait until the pool is saturated
	for i := 0; i < 200 && g.running.Load() < 2; i++ {
		time.Sleep(time.Millisecond)
	}
	if p := g.peak.Load(); p > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", p)
	}
	close(g.gate)
	d.Wait()
	if g.peak.Load() != 2 {
		t.Errorf("peak concurrency = %d, want exactly 2", g.peak.Load())
	}
	if got.Load() != 5 {
		t.Errorf("delivered %d outcomes, want 5", got.Load())
	}
	if batches.Load() != 1 {
		t.Errorf("onBatch called %d times, want 1", batches.Load())
	}
}

// panicProbe always panics inside Measure, simulating a buggy/misconfigured
// probe (e.g. an out-of-range allocation) — for testing that the scheduler
// contains a panic to a single Outcome instead of crashing the daemon.
type panicProbe struct{}

func (p *panicProbe) Name() string                     { return "panic" }
func (p *panicProbe) Describe() string                 { return "panic" }
func (p *panicProbe) Schema() map[string]probe.VarSpec { return nil }
func (p *panicProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	panic("boom: simulated probe panic")
}

// TestRunOneRecoversPanic is the regression test for the finding that
// scheduler probe goroutines did not recover panics: a panicking probe used
// to terminate the whole daemon. It must instead surface as a failed Outcome
// (Err set, mentioning the target and the recovered value), and — critically
// — must not stop other targets in the same round from completing.
func TestRunOneRecoversPanic(t *testing.T) {
	jobs := []Job{
		{Probe: &panicProbe{}, Target: probe.Target{Name: "panicky"}, Pings: 1, Timeout: time.Second},
		{Probe: &fakeProbe{name: "fine", delay: time.Millisecond, rtt: 0.01}, Target: probe.Target{Name: "ok"}, Pings: 1, Timeout: time.Second},
	}
	out := RunRound(context.Background(), jobs, 2)

	if out[0].Err == nil {
		t.Fatal("expected an error Outcome for the panicking probe, got nil Err")
	}
	if !strings.Contains(out[0].Err.Error(), "panicky") {
		t.Errorf("recovered error should mention the target name, got: %v", out[0].Err)
	}
	if !strings.Contains(out[0].Err.Error(), "boom") {
		t.Errorf("recovered error should mention the recovered panic value, got: %v", out[0].Err)
	}
	if out[0].Computed.Loss != 1 {
		t.Errorf("panicking target loss = %d, want 1 (full loss)", out[0].Computed.Loss)
	}

	// The other target in the same round must still complete normally — a
	// panic in one probe goroutine must not take down its siblings.
	if out[1].Err != nil {
		t.Errorf("sibling target errored: %v, want nil", out[1].Err)
	}
	if out[1].Computed.Loss != 0 {
		t.Errorf("sibling target loss = %d, want 0", out[1].Computed.Loss)
	}
}

func TestNextDelay(t *testing.T) {
	step := 10 * time.Second
	// now exactly on a grid boundary (offset 0) -> next delay == full step.
	now := time.Unix(1_000_000, 0) // 1000000 % 10 == 0
	if d := NextDelay(now, step, 0); d != step {
		t.Errorf("on-grid delay = %v, want %v", d, step)
	}
	// 3s past a boundary -> 7s until next.
	now2 := time.Unix(1_000_003, 0)
	if d := NextDelay(now2, step, 0); d != 7*time.Second {
		t.Errorf("delay = %v, want 7s", d)
	}
}

// pj builds a Planner test job with only a name and step (Probe unused by Tick).
func pj(name string, step time.Duration) Job {
	return Job{Target: probe.Target{Name: name}, Step: step}
}

func names(jobs []Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Target.Name
	}
	return out
}

func has(jobs []Job, name string) bool {
	for _, j := range jobs {
		if j.Target.Name == name {
			return true
		}
	}
	return false
}

func TestPlannerPerTargetCadence(t *testing.T) {
	// t0 is divisible by both 2s and 5s, so the phase-aligned grid is predictable:
	// next(A)=t0+2s, next(B)=t0+5s.
	t0 := time.Unix(1_700_000_000, 0)
	jobs := []Job{pj("A", 2*time.Second), pj("B", 5*time.Second)}
	p := NewPlanner()

	// First tick: both targets are new -> both fire immediately.
	due, sleep := p.Tick(jobs, t0, time.Minute)
	if len(due) != 2 {
		t.Fatalf("first tick due = %v, want both A and B", names(due))
	}
	if sleep != 2*time.Second { // earliest next is A at t0+2s
		t.Errorf("sleep = %v, want 2s", sleep)
	}

	// Same instant again: nothing is due yet.
	due, _ = p.Tick(jobs, t0, time.Minute)
	if len(due) != 0 {
		t.Fatalf("re-tick at same instant due = %v, want none", names(due))
	}

	// At t0+2s only A (2s cadence) is due; B (5s) is not.
	due, sleep = p.Tick(jobs, t0.Add(2*time.Second), time.Minute)
	if !has(due, "A") || has(due, "B") || len(due) != 1 {
		t.Fatalf("t0+2s due = %v, want [A] only", names(due))
	}
	if sleep != 2*time.Second { // A re-armed to t0+4s (2s away); B at t0+5s (3s away)
		t.Errorf("sleep = %v, want 2s", sleep)
	}

	// At t0+5s both are due (A's t0+4s has passed, B's t0+5s is now).
	due, _ = p.Tick(jobs, t0.Add(5*time.Second), time.Minute)
	if !has(due, "A") || !has(due, "B") {
		t.Fatalf("t0+5s due = %v, want both", names(due))
	}
}

// A SIGHUP reload that changes a target's Step must take effect promptly: the old
// deadline (computed under the previous step) is discarded so the new cadence
// applies, rather than leaving the target deferred until the stale deadline.
func TestPlannerReArmsOnStepChange(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	p := NewPlanner()
	// A on a 1h cadence fires now; next fire is ~t0+1h.
	p.Tick([]Job{pj("A", time.Hour)}, t0, time.Minute)
	if due, _ := p.Tick([]Job{pj("A", time.Hour)}, t0.Add(time.Second), time.Minute); has(due, "A") {
		t.Fatalf("A should not be due 1s into a 1h cadence")
	}
	// Reload retunes A to a 1s cadence: it must re-arm and fire promptly, not wait
	// out the old 1h deadline.
	due, _ := p.Tick([]Job{pj("A", time.Second)}, t0.Add(2*time.Second), time.Minute)
	if !has(due, "A") {
		t.Fatalf("after step change 1h->1s, A should fire promptly, got %v", names(due))
	}
}

// SleepToNext recomputes the wait from the current time, so a slow round doesn't
// skew the phase-aligned grid (the serving loop calls it after probes complete).
func TestPlannerSleepToNext(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	p := NewPlanner()
	p.Tick([]Job{pj("A", 10*time.Second)}, t0, time.Minute) // next A = t0+10s
	if d := p.SleepToNext(t0.Add(3*time.Second), time.Minute); d != 7*time.Second {
		t.Errorf("SleepToNext after 3s of work = %v, want 7s", d)
	}
	// A round that overran its step yields a zero sleep (fire immediately), not a
	// negative one.
	if d := p.SleepToNext(t0.Add(12*time.Second), time.Minute); d != 0 {
		t.Errorf("SleepToNext past the deadline = %v, want 0", d)
	}
}

func TestPlannerCapsSleepAndForgetsRemoved(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	p := NewPlanner()
	// A long-step target: sleep must be capped at maxSleep so reloads are noticed.
	p.Tick([]Job{pj("A", time.Hour)}, t0, time.Minute) // fires now, next = t0+1h
	_, sleep := p.Tick([]Job{pj("A", time.Hour)}, t0, time.Minute)
	if sleep != time.Minute {
		t.Errorf("sleep = %v, want capped at 1m", sleep)
	}
	// Dropping A from the job set should forget it (next tick with a new B only).
	due, _ := p.Tick([]Job{pj("B", time.Second)}, t0.Add(time.Second), time.Minute)
	if !has(due, "B") || has(due, "A") {
		t.Errorf("after removing A, due = %v, want [B]", names(due))
	}
	if _, ok := p.next["A"]; ok {
		t.Errorf("A should have been forgotten from the planner")
	}
}
