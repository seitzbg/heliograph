package sshprobe

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// TestHungEndpointMakesBoundedAttempts pins the per-attempt-timeout behavior:
// against an endpoint that accepts the TCP connection but never writes an SSH
// banner (a firewalled/tarpit host — the connect succeeds instantly, the banner
// read is what blocks), the probe must make `pings` bounded attempts within the
// round budget, not burn the entire budget on the first read and record only one.
func TestHungEndpointMakesBoundedAttempts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var accepted int
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			// Hold the connection open, never write a banner; close on test teardown.
			go func(c net.Conn) { <-done; _ = c.Close() }(c)
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, err := probe.New("SSH", nil)
	if err != nil {
		t.Fatal(err)
	}

	const pings = 3
	const budget = 900 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	res, _ := p.Measure(ctx, probe.Target{Host: "127.0.0.1", Params: map[string]string{"port": port}}, pings)
	elapsed := time.Since(start)

	if len(res.Samples) != 0 {
		t.Fatalf("hung endpoint should record no samples, got %d", len(res.Samples))
	}
	mu.Lock()
	got := accepted
	mu.Unlock()
	if got < pings {
		t.Fatalf("want %d bounded attempts against the hung endpoint, got %d "+
			"(probe burned the whole round budget on one read and bailed)", pings, got)
	}
	// Per-attempt shares sum to ~budget, so the round must not overrun it materially.
	if elapsed > budget+400*time.Millisecond {
		t.Fatalf("overran round budget: elapsed %s > budget %s", elapsed, budget)
	}
}
