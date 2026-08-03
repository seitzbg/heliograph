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

func (f *fakeProbe) Name() string                        { return f.name }
func (f *fakeProbe) Describe() string                    { return f.name }
func (f *fakeProbe) Schema() map[string]probe.VarSpec    { return nil }
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
