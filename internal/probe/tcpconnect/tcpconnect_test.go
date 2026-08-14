package tcpconnect_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect"
)

// A large interval_ms must not starve the round of attempts. The inter-attempt sleeps
// happen OUTSIDE the per-attempt contexts, so if they aren't reserved from the round
// budget the total is N*(budget/N) + (N-1)*interval, which overruns the deadline: the
// parent ctx fires mid-sleep and the loop bails after the first attempt, breaking the
// guarantee that a host is probed all N times. With the delays reserved (and interval_ms
// bounded against the budget), all N connects must complete within the round deadline.
func TestLargeIntervalStillProbesAllAttempts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Accept and immediately close every connection so each connect is an instant
	// success recorded as a sample — the count of samples is the count of attempts made.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)

	// interval_ms far larger than the round budget: unreserved, a single (pings-1) sleep
	// alone would blow the deadline.
	p, err := probe.New("TCPConnect", map[string]string{"interval_ms": "10000"})
	if err != nil {
		t.Fatal(err)
	}
	const pings = 4
	const budget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	res, _ := p.Measure(ctx, probe.Target{Host: "127.0.0.1", Params: map[string]string{"port": port}}, pings)
	elapsed := time.Since(start)

	if len(res.Samples) != pings {
		t.Fatalf("got %d attempts, want all %d within the round budget: a large interval_ms "+
			"starved the later attempts (delays not reserved from the budget)", len(res.Samples), pings)
	}
	if elapsed > budget {
		t.Errorf("round took %v, exceeding the %v budget: the inter-attempt delays were not "+
			"bounded/reserved", elapsed, budget)
	}
}

// The configured inter-attempt delay must apply after a failed connect too — a probe
// that skips the delay on failure floods a down host with back-to-back SYNs. Against a
// definitely-closed port (every connect fails), N pings with a 20ms interval must still
// take at least (N-1)*20ms of spacing.
func TestTCPConnectDelaysAfterFailedConnect(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close() // now nothing listens on that port -> connects are refused

	p, err := probe.New("TCPConnect", map[string]string{"interval_ms": "20"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, _ := p.Measure(context.Background(), probe.Target{Host: "127.0.0.1", Params: map[string]string{"port": port}}, 4)
	elapsed := time.Since(start)

	if len(res.Samples) != 0 {
		t.Fatalf("expected 0 samples connecting to a closed port, got %d", len(res.Samples))
	}
	if elapsed < 45*time.Millisecond {
		t.Errorf("4 pings took %v (< 45ms): the inter-attempt delay was skipped after failed connects", elapsed)
	}
}
