package scheduler

import (
	"context"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
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
