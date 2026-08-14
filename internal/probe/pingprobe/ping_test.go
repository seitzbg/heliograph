package pingprobe

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

func TestPingRegisteredWithSchema(t *testing.T) {
	if !slices.Contains(probe.Registered(), "Ping") {
		t.Fatal("Ping not registered")
	}
	sc, _ := probe.SchemaOf("Ping")
	for _, k := range []string{"packetsize", "interval_ms", "mode", "timeout_ms"} {
		if _, ok := sc[k]; !ok {
			t.Errorf("missing schema var %q", k)
		}
	}
	if err := sc["mode"].ValidateValue("mode", "bogus"); err == nil {
		t.Error("bad mode accepted")
	}
	// timeout_ms publishes its 1s default so schema clients get the full contract, and is
	// a probe-level positive-integer var (the fping -t analog).
	tm := sc["timeout_ms"]
	if tm.Default != "1000" {
		t.Errorf("timeout_ms default = %q, want \"1000\"", tm.Default)
	}
	if tm.Scope != probe.ProbeVar {
		t.Errorf("timeout_ms scope = %v, want ProbeVar", tm.Scope)
	}
	if err := tm.ValidateValue("timeout_ms", "0"); err == nil {
		t.Error("timeout_ms=0 should be rejected (positive integer)")
	}
}

// TestPingPacketSizeSchemaBounds is the regression test for the finding that
// packetsize had no upper bound: a schema-valid value like 1073741824 would
// reach buildEcho's make([]byte, packetsize) and OOM or panic-allocate. The
// schema must reject anything over maxPacketSize while still accepting the
// boundary value itself, and must still reject non-integers/negatives.
func TestPingPacketSizeSchemaBounds(t *testing.T) {
	sc, _ := probe.SchemaOf("Ping")
	spec := sc["packetsize"]

	if err := spec.ValidateValue("packetsize", strconv.Itoa(maxPacketSize)); err != nil {
		t.Errorf("packetsize at the max (%d) should be accepted, got: %v", maxPacketSize, err)
	}
	if err := spec.ValidateValue("packetsize", strconv.Itoa(maxPacketSize+1)); err == nil {
		t.Error("packetsize one over the max should be rejected")
	}
	if err := spec.ValidateValue("packetsize", "1073741824"); err == nil {
		t.Error("huge packetsize (1073741824) should be rejected")
	}
	if err := spec.ValidateValue("packetsize", "-1"); err == nil {
		t.Error("negative packetsize should still be rejected")
	}
	if err := spec.ValidateValue("packetsize", "abc"); err == nil {
		t.Error("non-integer packetsize should still be rejected")
	}
	if err := spec.ValidateValue("packetsize", "56"); err != nil {
		t.Errorf("ordinary packetsize 56 should be accepted, got: %v", err)
	}
}

// TestNewPingProbeRejectsOversizedDefaultPacketsize: a per-target override
// bypasses the schema check that runs at config-load time (target params
// aren't necessarily re-validated the same way), but the probe-level default
// itself must still be rejected by the factory — otherwise a bad probe-level
// config would only surface as a crash on the first Measure call.
func TestNewPingProbeRejectsOversizedDefaultPacketsize(t *testing.T) {
	_, err := newPingProbe(map[string]string{"packetsize": strconv.Itoa(maxPacketSize + 1)})
	if err == nil {
		t.Fatal("expected newPingProbe to reject an out-of-range default packetsize")
	}
	// The boundary value itself must still be accepted.
	if _, err := newPingProbe(map[string]string{"packetsize": strconv.Itoa(maxPacketSize)}); err != nil {
		t.Errorf("newPingProbe should accept packetsize at the max (%d), got: %v", maxPacketSize, err)
	}
}

// TestMeasureRejectsOversizedPerTargetPacketsize: a per-target packetsize
// override can bypass probe-level validation entirely, so Measure itself
// must guard against it — returning a clear error instead of reaching
// buildEcho's make([]byte, packetsize) with an enormous size.
func TestMeasureRejectsOversizedPerTargetPacketsize(t *testing.T) {
	p, err := newPingProbe(map[string]string{"interval_ms": "5"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	target := probe.Target{Host: "127.0.0.1", Params: map[string]string{"packetsize": "1073741824"}}
	res, err := p.Measure(ctx, target, 1)
	if err == nil {
		t.Fatal("expected Measure to reject an out-of-range per-target packetsize")
	}
	if len(res.Samples) != 0 {
		t.Errorf("expected no samples on a rejected packetsize, got %d", len(res.Samples))
	}
}

// TestNewPingProbeTimeoutMs: the timeout_ms knob (fping -t analog) must parse into
// replyTimeout and reject non-positive/invalid values, like interval_ms.
func TestNewPingProbeTimeoutMs(t *testing.T) {
	p, err := newPingProbe(map[string]string{"timeout_ms": "250"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*pingProbe).replyTimeout; got != 250*time.Millisecond {
		t.Errorf("timeout_ms=250 -> replyTimeout %v, want 250ms", got)
	}
	if _, err := newPingProbe(map[string]string{"timeout_ms": "0"}); err == nil {
		t.Error("timeout_ms=0 should be rejected")
	}
	if _, err := newPingProbe(map[string]string{"timeout_ms": "abc"}); err == nil {
		t.Error("non-integer timeout_ms should be rejected")
	}
	// Unset leaves replyTimeout at 0 (receiveDeadline falls back to the default).
	def, _ := newPingProbe(nil)
	if got := def.(*pingProbe).replyTimeout; got != 0 {
		t.Errorf("unset timeout_ms -> replyTimeout %v, want 0 (default applied later)", got)
	}
}

// TestPingLossReturnsBeforeLargeRoundBudget is the end-to-end regression for the freeze
// bug through the real socket path: against an unroutable target with a LARGE round
// budget (like production's 60s step), a lost ping must end the round shortly after the
// sends finish (bounded by the per-reply timeout), not block for the whole budget. The
// old code set the read deadline to ctx.Deadline() and blocked the full budget.
func TestPingLossReturnsBeforeLargeRoundBudget(t *testing.T) {
	p, err := newPingProbe(map[string]string{"interval_ms": "5", "timeout_ms": "500"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	res, err := p.Measure(ctx, probe.Target{Host: "192.0.2.1"}, 3)
	if err != nil {
		t.Skipf("cannot open ICMP socket here: %v", err)
	}
	if len(res.Samples) != 0 {
		t.Errorf("expected 0 samples for unroutable, got %d", len(res.Samples))
	}
	// lastSend ~10ms + 500ms reply window ~= 0.5s; must be far below the 20s budget.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("lossy round took %v with a 20s budget — it waited out the round budget "+
			"instead of the per-reply timeout", elapsed)
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

// TestReplyDeadlineIsOneWindowFromActualSend is the regression test for the "lossy round
// freezes for the whole round budget" bug: after the sends finish, the read deadline is
// one reply window from the ACTUAL last send (the time passed in), not the full round
// budget and not a nominal start+(pings-1)*interval. Live docker5 data: every native
// round with any loss took exactly 60s, vs FPing's ~10s (fping bounds each reply with
// -t). Measuring from the real last-send time is what lets send jitter not close the
// window before the last echo is out.
func TestReplyDeadlineIsOneWindowFromActualSend(t *testing.T) {
	const budget = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	// A reply window measured from `lastSend` (well past `start`) — not from `start`,
	// and not the whole budget.
	lastSend := time.Now().Add(9500 * time.Millisecond) // where the sends actually ended
	const reply = time.Second
	dl := replyDeadline(ctx, lastSend, reply)
	if wait := dl.Sub(lastSend); wait < reply-50*time.Millisecond || wait > reply+50*time.Millisecond {
		t.Errorf("reply window = %v from the actual last send, want ~%v", wait, reply)
	}
}

// TestReplyDeadlineNeverExceedsRoundBudget: the reply window is clamped to the round
// budget, so a short step still caps the round.
func TestReplyDeadlineNeverExceedsRoundBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	budgetDL, _ := ctx.Deadline()
	dl := replyDeadline(ctx, time.Now(), time.Second) // 1s window would overrun the 200ms budget
	if dl.After(budgetDL.Add(20 * time.Millisecond)) {
		t.Errorf("reply deadline %v exceeds the round budget deadline %v", dl, budgetDL)
	}
}

// TestSendPhaseDeadlineUsesRoundBudget: while echoes are still going out the read is
// bounded by the round budget (ctx's deadline), not a tight nominal window — so a slow
// send can't fire the deadline before a later echo is even out.
func TestSendPhaseDeadlineUsesRoundBudget(t *testing.T) {
	const budget = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	budgetDL, _ := ctx.Deadline()
	if dl := sendPhaseDeadline(ctx, time.Now(), 20, 500*time.Millisecond, time.Second); !dl.Equal(budgetDL) {
		t.Errorf("send-phase deadline = %v, want the round budget deadline %v", dl, budgetDL)
	}
	// With no ctx deadline (uncommon), it falls back to a bounded estimate (span + reply).
	start := time.Now()
	dl := sendPhaseDeadline(context.Background(), start, 20, 100*time.Millisecond, time.Second)
	span := 19 * 100 * time.Millisecond
	if got := dl.Sub(start); got < span || got > span+2*time.Second {
		t.Errorf("no-deadline fallback = %v, want ~span+reply (~%v)", got, span+time.Second)
	}
}

// TestSpreadInterval: the N echo sends must be SPREAD across the round budget (the
// scheduler's ctx deadline), not crammed into a ~50ms burst — a burst inflates loss on
// a marginal link and spikes the instantaneous send rate to a single destination.
func TestSpreadInterval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if iv := spreadInterval(ctx, 20, 0, pingReplyTimeout); iv < 200*time.Millisecond {
		t.Errorf("interval = %v, want spread out (>=200ms), not a ~50ms burst", iv)
	}
	// An explicit interval_ms override is honored (still capped to fit the budget).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if iv := spreadInterval(ctx2, 20, 100*time.Millisecond, pingReplyTimeout); iv != 100*time.Millisecond {
		t.Errorf("override interval = %v, want 100ms honored", iv)
	}
	// A short budget shrinks the interval so the whole send span fits inside it.
	ctx3, cancel3 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel3()
	iv3 := spreadInterval(ctx3, 20, 0, pingReplyTimeout)
	if span := time.Duration(19) * iv3; span >= 2*time.Second {
		t.Errorf("send span %v exceeds the 2s budget (interval=%v)", span, iv3)
	}
}

// TestSpreadIntervalReservesConfiguredReplyTimeout is the regression test for the finding
// that spreadInterval reserved only a hardcoded 1s tail even when timeout_ms > 1s. The
// send schedule could then push the last echo so late that less than the configured reply
// window remained before the round deadline, so replyDeadline clamped it to the deadline
// and the last echo got a shorter timeout than requested — avoidable loss on a
// high-latency target. With a reply timeout > 1s (and room in the budget), the nominal
// last send plus the full configured reply window must still fit within the round budget.
func TestSpreadIntervalReservesConfiguredReplyTimeout(t *testing.T) {
	const budget = 6 * time.Second
	const reply = 2 * time.Second // timeout_ms=2000, larger than the old hardcoded 1s cap
	const pings = 10
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	iv := spreadInterval(ctx, pings, 0, reply)
	// The nominal last send lands (pings-1)*iv into the round; the reply window applied
	// after it must not spill past the deadline (which would clamp the last echo's wait
	// below the configured timeout). reply <= budget/2 here, so the full window must fit.
	lastSend := time.Duration(pings-1) * iv
	if lastSend+reply > budget {
		t.Errorf("last send at %v + configured reply window %v = %v exceeds the %v round budget: "+
			"the send schedule did not reserve the configured reply timeout (tail clamped to 1s)",
			lastSend, reply, lastSend+reply, budget)
	}
}
