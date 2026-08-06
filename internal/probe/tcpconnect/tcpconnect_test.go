package tcpconnect_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"smokeping-modern/internal/probe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

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
