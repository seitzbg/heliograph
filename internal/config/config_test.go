package config

import (
	"testing"
	"time"

	// register the probe kinds used in the test tree
	_ "smokeping-modern/internal/probe/fping"
	_ "smokeping-modern/internal/probe/httpprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

const sample = `
database:
  step: 30s
  pings: 6
probes:
  FPing: { period_ms: "40" }
  TCPConnect: {}
  HTTP: {}
targets:
  probe: FPing          # inherited default for the whole tree
  children:
    Cloudflare:
      children:
        DNS:
          host: 1.1.1.1
        "443":
          probe: TCPConnect   # override for this leaf
          host: 1.1.1.1
          params: { port: "443" }
    Sites:
      probe: HTTP
      pings: 4              # override pings for this branch
      children:
        example:
          host: example.com
`

func TestInheritanceFlatten(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mons, err := c.Monitors()
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if len(mons) != 3 {
		t.Fatalf("got %d monitors, want 3: %+v", len(mons), mons)
	}
	byName := map[string]int{}
	for i, m := range mons {
		byName[m.Name] = i
	}

	dns := mons[byName["Cloudflare/DNS"]]
	if dns.ProbeKind != "FPing" { // inherited from the tree root
		t.Errorf("Cloudflare/DNS probe = %q, want FPing", dns.ProbeKind)
	}
	if dns.Pings != 6 || dns.Step != 30*time.Second {
		t.Errorf("Cloudflare/DNS pings/step = %d/%v, want 6/30s", dns.Pings, dns.Step)
	}

	tcp := mons[byName["Cloudflare/443"]]
	if tcp.ProbeKind != "TCPConnect" || tcp.Params["port"] != "443" {
		t.Errorf("Cloudflare/443 = %q port=%q, want TCPConnect/443", tcp.ProbeKind, tcp.Params["port"])
	}

	ex := mons[byName["Sites/example"]]
	if ex.ProbeKind != "HTTP" || ex.Pings != 4 { // branch override
		t.Errorf("Sites/example = %q pings=%d, want HTTP/4", ex.ProbeKind, ex.Pings)
	}
}

func TestUnknownProbeIsReported(t *testing.T) {
	const bad = `
targets:
  children:
    x:
      probe: Bogus
      host: 1.2.3.4
`
	c, err := Parse([]byte(bad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mons, err := c.Monitors()
	if err == nil {
		t.Fatalf("expected error for unknown probe, got none")
	}
	if len(mons) != 0 {
		t.Errorf("bad leaf should be excluded, got %d monitors", len(mons))
	}
}
