package pingprobe

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
)

func TestPingRegisteredWithSchema(t *testing.T) {
	if !slices.Contains(probe.Registered(), "Ping") {
		t.Fatal("Ping not registered")
	}
	sc, _ := probe.SchemaOf("Ping")
	for _, k := range []string{"packetsize", "interval_ms", "mode"} {
		if _, ok := sc[k]; !ok {
			t.Errorf("missing schema var %q", k)
		}
	}
	if err := sc["mode"].ValidateValue("mode", "bogus"); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestPingLoopbackBestEffort(t *testing.T) {
	newForTest := newPingProbe
	p, err := newForTest(map[string]string{"mode": "auto", "interval_ms": "5"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := p.Measure(ctx, probe.Target{Host: "127.0.0.1"}, 3)
	if err != nil {
		// no socket in this environment (no sysctl, no CAP_NET_RAW) → skip, don't fail
		t.Skipf("cannot open ICMP socket here: %v", err)
	}
	if len(res.Samples) == 0 {
		t.Error("expected at least one loopback sample")
	}
}

func TestPingLossOnUnroutableReturnsPromptly(t *testing.T) {
	newForTest := newPingProbe
	p, err := newForTest(map[string]string{"interval_ms": "5"})
	if err != nil {
		t.Fatal(err)
	}
	// probe TEST-NET-1 (RFC5737, won't answer) with a short deadline
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := p.Measure(ctx, probe.Target{Host: "192.0.2.1"}, 3)
	if err != nil {
		t.Skipf("cannot open ICMP socket here: %v", err) // skip if no socket
	}
	if len(res.Samples) != 0 {
		t.Errorf("expected 0 samples for unroutable, got %d", len(res.Samples))
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Measure did not return promptly on loss")
	}
}

// TestPingCancelDuringRoundReturnsCanceled is a regression test: a genuine
// context cancellation (not the round's own deadline) must be surfaced as an
// error, not silently swallowed into a 0-sample "loss" result — otherwise a
// round cut short by e.g. process shutdown would be persisted as a bogus
// high-loss sample instead of being recognizable as abandoned.
//
// It cancels the round while it is genuinely in flight and blocked waiting
// for replies (an unroutable target, well after all sends have gone out —
// see the timing comments below), and asserts both that the error is
// context.Canceled and that Measure returns promptly rather than sitting
// blocked until the outer (generous) context deadline.
func TestPingCancelDuringRoundReturnsCanceled(t *testing.T) {
	p, err := newPingProbe(map[string]string{"interval_ms": "20"})
	if err != nil {
		t.Fatal(err)
	}
	// Outer deadline is generous (5s) so it never fires on its own — only the
	// explicit cancel() below should end the round.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type outcome struct {
		res probe.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		// 5 pings, unroutable TEST-NET-1 target so nothing ever replies:
		// sends finish around 4*20ms=80ms, then Measure sits blocked inside
		// conn.ReadFrom waiting out the receive window.
		res, err := p.Measure(ctx, probe.Target{Host: "192.0.2.1"}, 5)
		done <- outcome{res, err}
	}()

	// Give the round time to finish sending and be blocked in the receive
	// phase (genuinely in flight, not still sending) before cancelling.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatal("expected an error on cancellation, got nil")
		}
		if !errors.Is(o.err, context.Canceled) {
			// Not a cancellation-shaped error at all — most likely no ICMP
			// socket available in this environment, matching the other
			// tests' skip convention.
			t.Skipf("cannot open ICMP socket here (or unexpected error): %v", o.err)
		}
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Errorf("Measure did not return promptly on cancellation, took %v", elapsed)
		}
		if len(o.res.Samples) != 0 {
			t.Errorf("expected 0 samples (unroutable target never replies), got %d", len(o.res.Samples))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Measure did not return within 2s of cancellation")
	}
}

// TestPingLoopbackRTTMagnitude is a regression test for the send-then-receive
// bug: sending all `pings` echoes (with interval_ms sleeps between them)
// before starting to read replies measures the send schedule, not the
// network — replies buffer in the socket and all get read back-to-back once
// the last send finally goes out, so RTT ends up ≈ (pings-1-seq)*interval
// regardless of real latency (median ≈ 100ms for pings=5/interval_ms=50 on
// a live run, even against loopback, which is sub-millisecond in reality).
//
// With sender and receiver running concurrently, a loopback RTT must stay
// far below the ~200ms total send window. 20ms is a generous bound — real
// loopback RTT is sub-millisecond — that still comfortably fails on the old
// sequential code (whose samples cluster around 0/50/100/150/200ms) and
// passes after the fix.
func TestPingLoopbackRTTMagnitude(t *testing.T) {
	p, err := newPingProbe(map[string]string{"mode": "auto", "interval_ms": "50"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := p.Measure(ctx, probe.Target{Host: "127.0.0.1"}, 5)
	if err != nil {
		t.Skipf("cannot open ICMP socket here: %v", err)
	}
	if len(res.Samples) == 0 {
		t.Fatal("expected at least one loopback sample")
	}
	const bound = 20 * time.Millisecond
	for i, s := range res.Samples {
		if d := time.Duration(s * float64(time.Second)); d >= bound {
			t.Errorf("sample[%d] = %v, want < %v (loopback RTT is sub-millisecond; a sample this large "+
				"means Measure is timing the send schedule, not the network)", i, d, bound)
		}
	}
}
