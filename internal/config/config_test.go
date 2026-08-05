package config

import (
	"testing"
	"time"

	"smokeping-modern/internal/model"

	// register the probe kinds used in the test tree
	_ "smokeping-modern/internal/probe/dns"
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

func TestInvalidPingsAndStepRejected(t *testing.T) {
	cases := map[string]string{
		"negative pings": `
database: { pings: -1 }
targets:
  children:
    x: { probe: TCPConnect, host: 1.2.3.4 }
`,
		"per-node negative pings": `
targets:
  children:
    x: { probe: TCPConnect, host: 1.2.3.4, pings: -5 }
`,
		"absurd pings": `
database: { pings: 100000 }
targets:
  children:
    x: { probe: TCPConnect, host: 1.2.3.4 }
`,
		"negative step": `
database: { pings: 5 }
targets:
  children:
    x: { probe: TCPConnect, host: 1.2.3.4, step: -5s }
`,
	}
	for name, y := range cases {
		c, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if _, err := c.Monitors(); err == nil {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
}

// A valid-YAML but empty child node (`empty:`) decodes to a nil *Node. The tree
// walker must report it as a config problem, never dereference it and panic —
// otherwise a bad edit crashes the collector (and a SIGHUP reload's goroutine).
func TestEmptyChildNodeIsErrorNotPanic(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    good: { host: 1.2.3.4, params: { port: "80" } }
    empty:
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = c.Monitors() // must not panic
	if err == nil {
		t.Fatalf("expected a validation error for the empty child node, got none")
	}
}

// Strict parsing rejects unknown fields so a typo is a loud error, not a silently
// ignored setting.
func TestStrictParseRejectsUnknownFields(t *testing.T) {
	cases := map[string]string{
		"unknown top-level": `
databse: { pings: 5 }
targets: { children: { x: { probe: TCPConnect, host: 1.2.3.4 } } }
`,
		"unknown node field": `
targets:
  children:
    x: { probe: TCPConnect, host: 1.2.3.4, prts: 3 }
`,
		"unknown alert field": `
alerts:
  a: { type: matcher, matcher: "CheckLoss(l=1,x=1)", coment: hi }
targets: { children: { x: { probe: TCPConnect, host: 1.2.3.4 } } }
`,
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected strict parse to reject it, got none", name)
		}
	}
}

// Target params are validated against the probe's schema: an unknown name or a
// probe-scoped var placed in per-target scope is rejected (additionalProperties:
// false + scope), instead of being silently ignored at runtime.
func TestTargetParamScopeValidation(t *testing.T) {
	cases := map[string]string{
		"unknown target param": `
targets: { children: { x: { probe: TCPConnect, host: 1.2.3.4, params: { porrt: "80" } } } }
`,
		"probe-scoped param in target": `
targets: { children: { x: { probe: TCPConnect, host: 1.2.3.4, params: { interval_ms: "10" } } } }
`,
	}
	for name, y := range cases {
		c, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if _, err := c.Monitors(); err == nil {
			t.Errorf("%s: expected a validation error, got none", name)
		}
	}
}

// Probe-level config blocks are validated too: an unknown key under probes.<Kind>
// is a config error, not silently dropped.
func TestProbeLevelParamValidation(t *testing.T) {
	const y = `
probes:
  TCPConnect: { bogus: "1" }
targets: { children: { x: { probe: TCPConnect, host: 1.2.3.4 } } }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil {
		t.Fatalf("expected error for unknown probe-level param, got none")
	}
}

// An explicit empty list clears an inherited alerts/alertee value; an absent field
// keeps inheriting. (Previously len()>0 conflated [] with absent.)
func TestEmptyAlertsListClearsInherited(t *testing.T) {
	const y = `
alerts:
  loss: { type: matcher, matcher: "CheckLoss(l=50,x=2)" }
targets:
  probe: TCPConnect
  alerts: [loss]
  children:
    quiet: { host: 1.2.3.4, params: { port: "80" }, alerts: [] }
    loud:  { host: 1.2.3.5, params: { port: "80" } }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mons, err := c.Monitors()
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	byName := map[string]model.Monitor{}
	for _, m := range mons {
		byName[m.Name] = m
	}
	if len(byName["quiet"].Alerts) != 0 {
		t.Errorf("quiet alerts = %v, want empty (explicit [] clears inherited)", byName["quiet"].Alerts)
	}
	if len(byName["loud"].Alerts) != 1 {
		t.Errorf("loud alerts = %v, want [loss] inherited", byName["loud"].Alerts)
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
