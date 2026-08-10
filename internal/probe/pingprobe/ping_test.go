package pingprobe

import (
	"context"
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
