package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"

	// register the probe kinds used in the test tree
	_ "github.com/seitzbg/heliograph/internal/probe/dns"
	_ "github.com/seitzbg/heliograph/internal/probe/fping"
	_ "github.com/seitzbg/heliograph/internal/probe/httpprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect"
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
		// #12(d): a non-nil but empty leaf contributes nothing and must be rejected.
		"non-nil empty leaf": `
targets:
  probe: TCPConnect
  children:
    x: {}
`,
		// #12(c): a malformed HTTP urlformat (target param) is a config error, not
		// phantom loss at runtime.
		"bad http urlformat": `
probes: { HTTP: {} }
targets:
  probe: HTTP
  children:
    x: { host: 1.2.3.4, params: { urlformat: "ftp://%host%/" } }
`,
		// #12(c): a bad HTTP method (probe-level param) is a config error.
		"bad http method": `
probes: { HTTP: { method: FETCH } }
targets:
  probe: HTTP
  children:
    x: { host: 1.2.3.4 }
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

// ---- config directory (default.yaml + conf.d concatenation) ----

func writeConfigDir(t *testing.T, def string, frags map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if def != "" {
		if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(def), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(frags) > 0 {
		cd := filepath.Join(dir, "conf.d")
		if err := os.Mkdir(cd, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range frags {
			if err := os.WriteFile(filepath.Join(cd, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

const dirDefault = `
database: { step: 30s, pings: 6 }
probes: { FPing: {}, TCPConnect: {}, HTTP: {} }
targets:
  probe: FPing
`

// LoadDir merges default.yaml with the conf.d fragments: each fragment contributes
// top-level target branches that inherit the tree-wide defaults from default.yaml.
func TestLoadDirConcatenatesBranches(t *testing.T) {
	dir := writeConfigDir(t, dirDefault, map[string]string{
		"10-cloudflare.yaml": "targets:\n  children:\n    Cloudflare:\n      children:\n        DNS: { host: 1.1.1.1 }\n        \"443\": { probe: TCPConnect, host: 1.1.1.1, params: { port: \"443\" } }\n",
		"20-sites.yaml":      "targets:\n  children:\n    Sites:\n      probe: HTTP\n      children:\n        example: { host: example.com }\n",
	})
	c, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	mons, err := c.Monitors()
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	byName := map[string]model.Monitor{}
	for _, m := range mons {
		byName[m.Name] = m
	}
	if len(mons) != 3 {
		t.Fatalf("got %d monitors, want 3: %v", len(mons), byName)
	}
	if m := byName["Cloudflare/DNS"]; m.ProbeKind != "FPing" || m.Host != "1.1.1.1" {
		t.Errorf("Cloudflare/DNS = %+v, want FPing/1.1.1.1 (default probe inherited)", m)
	}
	if m := byName["Cloudflare/443"]; m.ProbeKind != "TCPConnect" {
		t.Errorf("Cloudflare/443 probe = %q, want TCPConnect", m.ProbeKind)
	}
	if m := byName["Sites/example"]; m.ProbeKind != "HTTP" {
		t.Errorf("Sites/example probe = %q, want HTTP", m.ProbeKind)
	}
	// database is inherited from default.yaml (step 30s, pings 6).
	if m := byName["Cloudflare/DNS"]; m.Step != 30*time.Second || m.Pings != 6 {
		t.Errorf("Cloudflare/DNS step/pings = %v/%d, want 30s/6 (from default.yaml database)", m.Step, m.Pings)
	}
}

// A fragment may only contain targets.children — database/probes/alerts and any
// tree-wide targets:-node fields belong in default.yaml, and are loud errors.
func TestLoadDirRejectsNonBranchFragments(t *testing.T) {
	cases := map[string]string{
		"database": "database: { step: 10s }\ntargets:\n  children:\n    X: { host: h }\n",
		"probes":   "probes: { FPing: {} }\ntargets:\n  children:\n    X: { host: h }\n",
		"alerts":   "alerts:\n  a: { type: loss, pattern: \">1\" }\ntargets:\n  children:\n    X: { host: h }\n",
		"rootnode": "targets:\n  probe: FPing\n  children:\n    X: { host: h }\n",
		"vantages": "targets:\n  vantages: [nyc]\n  children:\n    X: { host: h }\n",
	}
	for name, frag := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeConfigDir(t, dirDefault, map[string]string{"10-bad.yaml": frag})
			if _, err := LoadDir(dir); err == nil {
				t.Fatalf("expected LoadDir to reject a fragment setting %s", name)
			}
		})
	}
}

// The same top-level branch in two fragments is ambiguous — a hard error naming both.
func TestLoadDirDuplicateBranch(t *testing.T) {
	dir := writeConfigDir(t, dirDefault, map[string]string{
		"10-a.yaml": "targets:\n  children:\n    Dup: { host: 1.1.1.1 }\n",
		"20-b.yaml": "targets:\n  children:\n    Dup: { host: 8.8.8.8 }\n",
	})
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("expected an error for a duplicate top-level branch")
	}
	if !strings.Contains(err.Error(), "Dup") {
		t.Errorf("error should name the duplicate branch, got: %v", err)
	}
}

func TestLoadDirEmpty(t *testing.T) {
	if _, err := LoadDir(t.TempDir()); err == nil {
		t.Fatal("expected an error for an empty config directory")
	}
}

// LoadPath dispatches: a file goes through Load, a directory through LoadDir.
func TestLoadPathDispatch(t *testing.T) {
	// single file
	f := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(f, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	cf, err := LoadPath(f)
	if err != nil {
		t.Fatalf("LoadPath(file): %v", err)
	}
	if mf, _ := cf.Monitors(); len(mf) != 3 {
		t.Errorf("LoadPath(file) monitors = %d, want 3", len(mf))
	}
	// directory
	dir := writeConfigDir(t, dirDefault, map[string]string{
		"10-x.yaml": "targets:\n  children:\n    X: { host: 1.1.1.1 }\n",
	})
	cd, err := LoadPath(dir)
	if err != nil {
		t.Fatalf("LoadPath(dir): %v", err)
	}
	if md, _ := cd.Monitors(); len(md) != 1 {
		t.Errorf("LoadPath(dir) monitors = %d, want 1", len(md))
	}
}

// A constrained param with a bad value is a loud config error, not a silent
// runtime fallback — target-scoped params (validated per leaf) and probe-level
// params (validated once) both.
func TestConfigRejectsBadParamValues(t *testing.T) {
	cases := map[string]string{
		"target port out of range": `
probes: { TCPConnect: {} }
targets:
  probe: TCPConnect
  children:
    x: { host: h, params: { port: "99999" } }
`,
		"target insecure_ssl typo": `
probes: { HTTP: {} }
targets:
  probe: HTTP
  children:
    x: { host: h, params: { insecure_ssl: "ture" } }
`,
		"target recordtype invalid": `
probes: { DNS: {} }
targets:
  probe: DNS
  children:
    x: { host: h, params: { recordtype: "NOTATYPE" } }
`,
		"probe-level protocol invalid": `
probes: { DNS: { protocol: sctp } }
targets:
  probe: DNS
  children:
    x: { host: h }
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := c.Monitors(); err == nil {
				t.Fatalf("expected a validation error for %s", name)
			}
		})
	}

	// A valid config with the same params must still pass (regression guard).
	good := `
probes: { DNS: { protocol: tcp }, TCPConnect: {}, HTTP: {} }
targets:
  probe: TCPConnect
  children:
    tcp: { host: h, params: { port: "443" } }
    dns: { probe: DNS, host: h, params: { recordtype: AAAA } }
    web: { probe: HTTP, host: h, params: { insecure_ssl: "true" } }
`
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse good: %v", err)
	}
	if mons, err := c.Monitors(); err != nil {
		t.Fatalf("valid params should pass, got: %v (mons=%d)", err, len(mons))
	}
}

// Target identity is the path key used by the scheduler/store/alerts. Two config
// paths that collapse to the same name, or a host on the root (empty name), must be
// a loud error rather than silently merging targets (CODE_REVIEW #6).
func TestMonitorsRejectsAmbiguousIdentity(t *testing.T) {
	// A leaf literally named "a/b" and a nested a -> b both flatten to "a/b".
	collide := `
probes: { FPing: {} }
targets:
  probe: FPing
  children:
    "a/b": { host: 1.1.1.1 }
    a:
      children:
        b: { host: 8.8.8.8 }
`
	c, err := Parse([]byte(collide))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil || !strings.Contains(err.Error(), "cannot contain '/'") {
		t.Errorf("expected a slash-key validation error, got %v", err)
	}

	// Reject a lone slash-containing key too: it need not collide to be unaddressable by the
	// browser's slash-delimited Config-tree paths. Group-only keys are covered by the same walk.
	lone, err := Parse([]byte(`
probes: { FPing: {} }
targets:
  probe: FPing
  children:
    "site/check": { host: 1.1.1.1 }
`))
	if err != nil {
		t.Fatalf("Parse lone slash key: %v", err)
	}
	if _, err := lone.Monitors(); err == nil || !strings.Contains(err.Error(), "cannot contain '/'") {
		t.Errorf("expected lone slash key to be rejected, got %v", err)
	}

	groupOnly, err := Parse([]byte(`
probes: { FPing: {} }
targets:
  probe: FPing
  children:
    "site/check":
      children:
        endpoint: { host: 1.1.1.1 }
`))
	if err != nil {
		t.Fatalf("Parse grouping-only slash key: %v", err)
	}
	if _, err := groupOnly.Monitors(); err == nil || !strings.Contains(err.Error(), "cannot contain '/'") {
		t.Errorf("expected grouping-only slash key to be rejected, got %v", err)
	}

	// A host on the root node yields an empty target name.
	rootHost := "probes: { FPing: {} }\ntargets: { probe: FPing, host: 1.1.1.1 }\n"
	c2, err := Parse([]byte(rootHost))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c2.Monitors(); err == nil {
		t.Error("expected an error for an empty target name (host on the root node)")
	}
}

// A sub-second step must be rejected: the API serializes round timestamps at
// whole-second resolution, so a cadence below 1s produces ambiguous/duplicate
// timestamps that corrupt the graph X-axis and the incremental watermark (#9).
func TestSubSecondStepRejected(t *testing.T) {
	cfg := "database: { step: 500ms, pings: 4 }\ntargets: { probe: FPing, children: { h: { host: 1.1.1.1 } } }\n"
	c, err := Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil || !strings.Contains(err.Error(), "step") {
		t.Fatalf("expected a sub-second step to be rejected mentioning `step`, got %v", err)
	}
	// A whole-second step at the 1s floor is accepted.
	ok := "database: { step: 1s, pings: 4 }\ntargets: { probe: FPing, children: { h: { host: 1.1.1.1 } } }\n"
	c2, err := Parse([]byte(ok))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c2.Monitors(); err != nil {
		t.Fatalf("a 1s step should be accepted, got %v", err)
	}
}

// vantages inherit down the tree like alerts/alertee; unset defaults to [local];
// an explicit override replaces; an explicit [] is a config error (no one would probe it).
func TestVantagesInheritanceAndDefault(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    plain:    { host: 1.2.3.4, params: { port: "80" } }                 # -> [local] (default)
    region:
      vantages: [nyc, lon]
      children:
        edge:  { host: 1.2.3.5, params: { port: "80" } }                # -> [nyc lon] (inherited)
        both:  { host: 1.2.3.6, params: { port: "80" }, vantages: [local, nyc] }
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
	if got := byName["plain"].Vantages; len(got) != 1 || got[0] != "local" {
		t.Errorf("plain vantages = %v, want [local] (default)", got)
	}
	if got := byName["region/edge"].Vantages; len(got) != 2 || got[0] != "nyc" || got[1] != "lon" {
		t.Errorf("region/edge vantages = %v, want [nyc lon] (inherited)", got)
	}
	if got := byName["region/both"].Vantages; len(got) != 2 || got[0] != "local" || got[1] != "nyc" {
		t.Errorf("region/both vantages = %v, want [local nyc] (override)", got)
	}
}

func TestEmptyVantagesListIsRejected(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    orphan: { host: 1.2.3.4, params: { port: "80" }, vantages: [] }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil {
		t.Fatal("expected an error for a target with no vantages, got none")
	}
}

func TestBlankVantageNameIsRejected(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    bad: { host: 1.2.3.4, params: { port: "80" }, vantages: ["", "nyc"] }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil {
		t.Fatal("expected an error for a blank vantage name, got none")
	}
}

// A vantage name that can never be provisioned (a key can't be minted for it, and
// ?vantage= reads reject it) must fail at config load rather than leave the target
// permanently dark (CODE_REVIEW #11 / P3-11).
func TestUnprovisionableVantageNameIsRejected(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    bad: { host: 1.2.3.4, params: { port: "80" }, vantages: ["new york"] }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil {
		t.Fatal(`expected an error for the unprovisionable vantage name "new york", got none`)
	}
}

// A vantage listed twice on one target is a config error — it silently does nothing useful.
func TestDuplicateVantageIsRejected(t *testing.T) {
	const y = `
targets:
  probe: TCPConnect
  children:
    dup: { host: 1.2.3.4, params: { port: "80" }, vantages: [nyc, nyc] }
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Monitors(); err == nil {
		t.Fatal("expected an error for a duplicate vantage, got none")
	}
}

func TestAppendDBFragmentMerges(t *testing.T) {
	base, err := Parse([]byte("targets:\n  children:\n    yaml-one: {probe: HTTP, host: a.example}\n"))
	if err != nil {
		t.Fatal(err)
	}
	// DB fragment as JSON, with a Duration ("30s") to prove the yaml decoder parses JSON
	// AND the custom Duration unmarshaler fires.
	frag := []byte(`{"targets":{"children":{"db-one":{"probe":"HTTP","host":"b.example","step":"30s"}}}}`)
	if err := AppendDBFragment(base, frag); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := base.Targets.Children["db-one"]
	if got == nil || got.Host != "b.example" {
		t.Fatalf("db-one not merged: %+v", base.Targets.Children)
	}
	if time.Duration(got.Step) != 30*time.Second {
		t.Fatalf("step not parsed from JSON doc: %v", got.Step)
	}
}

func TestAppendDBFragmentDuplicateBranch(t *testing.T) {
	base, _ := Parse([]byte("targets:\n  children:\n    dup: {probe: HTTP, host: a.example}\n"))
	err := AppendDBFragment(base, []byte(`{"targets":{"children":{"dup":{"probe":"HTTP","host":"b.example"}}}}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-branch error, got %v", err)
	}
}

func TestAppendDBFragmentRejectsGlobals(t *testing.T) {
	base, _ := Parse([]byte("targets:\n  children: {}\n"))
	err := AppendDBFragment(base, []byte(`{"alerts":{"x":{"type":"loss","pattern":">50%"}}}`))
	if err == nil || !strings.Contains(err.Error(), "alerts") {
		t.Fatalf("want globals-rejected error, got %v", err)
	}
}

func TestAppendDBFragmentEmptyIsNoop(t *testing.T) {
	base, _ := Parse([]byte("targets:\n  children:\n    only: {probe: HTTP, host: a.example}\n"))
	if err := AppendDBFragment(base, []byte("   ")); err != nil {
		t.Fatalf("empty fragment should be a no-op: %v", err)
	}
	if len(base.Targets.Children) != 1 {
		t.Fatalf("empty fragment changed config: %+v", base.Targets.Children)
	}
}

func TestAppendImportAddsAndIsIdempotent(t *testing.T) {
	imp := []byte("targets:\n  children:\n    a: {probe: HTTP, host: a.example}\n    b: {probe: TCPConnect, host: b.example, params: {port: \"80\"}}\n")
	merged, added, unchanged, err := AppendImport(nil, imp) // empty DB
	if err != nil || added != 2 || unchanged != 0 {
		t.Fatalf("first import: added=%d unchanged=%d err=%v", added, unchanged, err)
	}
	// lowercase keys in the marshaled fragment (store/UI convention)
	if !strings.Contains(string(merged), `"probe":"HTTP"`) || !strings.Contains(string(merged), `"host":"a.example"`) {
		t.Fatalf("merged not lowercase-keyed: %s", merged)
	}
	// re-import the exact same file against the merged doc -> all unchanged, no error
	merged2, added2, unchanged2, err2 := AppendImport(merged, imp)
	if err2 != nil || added2 != 0 || unchanged2 != 2 {
		t.Fatalf("re-import: added=%d unchanged=%d err=%v", added2, unchanged2, err2)
	}
	_ = merged2
}

func TestAppendImportConflictWritesNothing(t *testing.T) {
	db := []byte(`{"targets":{"children":{"a":{"probe":"HTTP","host":"a.example"}}}}`)
	imp := []byte("targets:\n  children:\n    a: {probe: HTTP, host: DIFFERENT.example}\n    c: {probe: HTTP, host: c.example}\n")
	_, added, _, err := AppendImport(db, imp)
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("want conflict naming \"a\", got added=%d err=%v", added, err)
	}
	// atomic: "c" must NOT have been added despite being new (whole import aborts)
	if added != 0 {
		t.Fatalf("conflict must write nothing, added=%d", added)
	}
}

// A stored target carries a server-minted stable id, but the re-imported file omits it (id is
// server-owned, never present in the user's import). AppendImport must treat the re-import as
// unchanged, not a conflict on the id difference — otherwise minting an id, which every persist
// path now does, would break import idempotency (CODE_REVIEW M8).
func TestAppendImportIdempotentAcrossMintedID(t *testing.T) {
	// The DB already holds "a" with a minted UUID id, as it would after any persist.
	db := []byte(`{"targets":{"children":{"a":{"probe":"HTTP","host":"a.example","id":"11111111-2222-3333-4444-555555555555"}}}}`)
	imp := []byte("targets:\n  children:\n    a: {probe: HTTP, host: a.example}\n")
	if _, added, unchanged, err := AppendImport(db, imp); err != nil || added != 0 || unchanged != 1 {
		t.Fatalf("re-import of an id-less file against a minted-id store must be idempotent: added=%d unchanged=%d err=%v", added, unchanged, err)
	}
	// Clearing id for the comparison must not mask a real settings change: a differing host still conflicts.
	impDiff := []byte("targets:\n  children:\n    a: {probe: HTTP, host: DIFFERENT.example}\n")
	if _, _, _, err := AppendImport(db, impDiff); err == nil {
		t.Fatal("a genuine settings change must still conflict even when the ids differ")
	}
}

func TestAppendImportRejectsGlobals(t *testing.T) {
	_, _, _, err := AppendImport(nil, []byte("alerts:\n  x: {type: loss, pattern: \">50%\"}\ntargets:\n  children:\n    a: {probe: HTTP, host: a}\n"))
	if err == nil || !strings.Contains(err.Error(), "alerts") {
		t.Fatalf("want globals-rejected, got %v", err)
	}
}

// AppendImport validates its fragment CONTEXT-FREE only (validateFragment's structural
// checks, the additive merge, and the duplicate/idempotency logic) — it must NOT run the
// fragment's targets through Monitors() in isolation, because a leaf's probe/params/alerts
// may be inherited from the real default.yaml, which AppendImport never sees. A schema
// problem that survives full composition (unknown probe kind, missing param, undefined
// alert, or truly no probe anywhere) is instead caught downstream, once the fragment is
// merged with the base config — see cmd/smoked's buildRuntime-level tests. So even a fragment
// naming an unregistered probe kind is accepted here: added=1, err=nil.
func TestAppendImportDoesNotSchemaValidateInIsolation(t *testing.T) {
	_, added, _, err := AppendImport(nil, []byte("targets:\n  children:\n    a: {probe: NoSuchProbe, host: a}\n"))
	if err != nil || added != 1 {
		t.Fatalf("AppendImport must not schema-validate in isolation: added=%d err=%v (want 1/nil)", added, err)
	}
}

// The motivating case for this finding: a fragment whose target sets no `probe` at all,
// relying entirely on the real default.yaml's tree-wide `targets.probe` — invisible to
// AppendImport, which only ever sees the DB-fragment side. Rejecting this in isolation (the
// old behavior) made DB imports stricter than an equivalent conf.d fragment, which inherits
// the tree-wide default fine via LoadDir's full composition. AppendImport must accept it;
// whether the base config actually defines a tree-wide probe is checked later, when the
// fragment is composed with default.yaml (config.AppendDBFragment + Monitors(), exercised by
// cmd/smoked's buildRuntime tests).
func TestAppendImportAcceptsFragmentThatReliesOnInheritedProbe(t *testing.T) {
	imp := []byte("targets:\n  children:\n    inherited:\n      host: 127.0.0.1\n")
	_, added, _, err := AppendImport(nil, imp)
	if err != nil || added < 1 {
		t.Fatalf("fragment relying on inherited probe must be accepted: added=%d err=%v (want >=1/nil)", added, err)
	}
}

func TestAppendImportEmptyIsNoop(t *testing.T) {
	_, added, unchanged, err := AppendImport(nil, []byte("   "))
	if err != nil || added != 0 || unchanged != 0 {
		t.Fatalf("empty import: added=%d unchanged=%d err=%v", added, unchanged, err)
	}
}

func TestDecodeRejectsMultipleDocuments(t *testing.T) {
	_, err := Parse([]byte("targets:\n  children:\n    a: {probe: HTTP, host: a}\n---\ndatabase: {pings: 5}\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("want multi-doc error, got %v", err)
	}
	if _, err := Parse([]byte("targets:\n  children:\n    a: {probe: HTTP, host: a}\n")); err != nil {
		t.Fatalf("single doc should parse: %v", err)
	}
	if _, err := Parse(nil); err != nil {
		t.Fatalf("empty should parse: %v", err)
	}
}

// A file using the explicit-empty-list clear (alerts: []) must re-import idempotently, not
// spuriously conflict — the nil-vs-empty round-trip must be preserved (Task-1 review finding c).
func TestAppendImportIdempotentWithExplicitEmptyList(t *testing.T) {
	imp := []byte("targets:\n  children:\n    a: {probe: HTTP, host: a.example, alerts: []}\n")
	merged, added, _, err := AppendImport(nil, imp)
	if err != nil || added != 1 {
		t.Fatalf("first import: added=%d err=%v", added, err)
	}
	if !strings.Contains(string(merged), `"alerts":[]`) {
		t.Fatalf("explicit empty alerts not preserved in stored JSON: %s", merged)
	}
	_, added2, unchanged2, err2 := AppendImport(merged, imp)
	if err2 != nil || added2 != 0 || unchanged2 != 1 {
		t.Fatalf("re-import with explicit []: added=%d unchanged=%d err=%v (want 0/1/nil)", added2, unchanged2, err2)
	}
}

// A target that carries an empty params/children map must re-import as unchanged, not conflict:
// omitempty drops the empty map when the fragment is stored, so a naive reflect.DeepEqual against
// the re-decoded (nil) node would spuriously conflict. Classification compares canonical JSON, so
// nil and empty collapse to the same absent value and the re-import is idempotent.
func TestAppendImportIdempotentWithEmptyMaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		imp  string
	}{
		{"empty params", "targets:\n  children:\n    a: {probe: HTTP, host: a.example, params: {}}\n"},
		{"empty children", "targets:\n  children:\n    a: {probe: HTTP, host: a.example, children: {}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imp := []byte(tc.imp)
			merged, added, _, err := AppendImport(nil, imp)
			if err != nil || added != 1 {
				t.Fatalf("first import: added=%d err=%v", added, err)
			}
			_, added2, unchanged2, err2 := AppendImport(merged, imp)
			if err2 != nil || added2 != 0 || unchanged2 != 1 {
				t.Fatalf("re-import: added=%d unchanged=%d err=%v (want 0/1/nil)", added2, unchanged2, err2)
			}
		})
	}
}

// A node's `id` (its stable storage identity, decoupled from tree position) must survive
// AppendImport unchanged: it's a plain Node field with a json/yaml tag, merged wholesale
// (base.Targets.Children[k] = imp.Targets.Children[k]), not reconstructed field-by-field.
// Re-importing the same fragment (id included) must be a no-op — the id participates in
// nodesEqual's canonical-JSON comparison the same as every other field, so a differing id
// on an otherwise-identical node would (correctly) be treated as a conflict, and an
// identical id keeps the re-import idempotent (unchanged count, not added).
func TestImportPreservesID(t *testing.T) {
	imp := []byte("targets:\n  children:\n    a: {probe: HTTP, host: a.example, id: existing-id-123}\n")
	merged, added, unchanged, err := AppendImport(nil, imp)
	if err != nil || added != 1 || unchanged != 0 {
		t.Fatalf("first import: added=%d unchanged=%d err=%v", added, unchanged, err)
	}
	if !strings.Contains(string(merged), `"id":"existing-id-123"`) {
		t.Fatalf("id not preserved in merged doc: %s", merged)
	}
	// Re-import the exact same fragment: unchanged, not added -> id round-trips stably.
	merged2, added2, unchanged2, err2 := AppendImport(merged, imp)
	if err2 != nil || added2 != 0 || unchanged2 != 1 {
		t.Fatalf("re-import: added=%d unchanged=%d err=%v (want 0/1/nil)", added2, unchanged2, err2)
	}
	if !strings.Contains(string(merged2), `"id":"existing-id-123"`) {
		t.Fatalf("id lost on re-import: %s", merged2)
	}
}

// TestMonitorCarriesDisplayIPAndTitle: a leaf may carry a pinned display `ip` and a
// `title` (display name); both flow into the Monitor as display-only fields, separate
// from the probed `host`.
func TestMonitorCarriesDisplayIPAndTitle(t *testing.T) {
	y := `
targets:
  probe: FPing
  children:
    cf:
      title: Cloudflare DNS
      host: one.one.one.one
      ip: 1.1.1.1
`
	c, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mons, err := c.Monitors()
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if len(mons) != 1 {
		t.Fatalf("want 1 monitor, got %d", len(mons))
	}
	m := mons[0]
	if m.IP != "1.1.1.1" {
		t.Errorf("Monitor.IP = %q, want 1.1.1.1", m.IP)
	}
	if m.Title != "Cloudflare DNS" {
		t.Errorf("Monitor.Title = %q, want Cloudflare DNS", m.Title)
	}
	if m.Host != "one.one.one.one" {
		t.Errorf("Monitor.Host = %q, want one.one.one.one (probe target unchanged)", m.Host)
	}
}

func TestMonitorsHonorWeight(t *testing.T) {
	cfg := &Config{
		Database: Database{Pings: 20, Step: Duration(time.Second)}, // valid defaults; the test cares about order
		Targets: &Node{Children: map[string]*Node{
			"alpha": {Probe: "FPing", Host: "a", Weight: 10}, // pushed last despite A–Z
			"omega": {Probe: "FPing", Host: "o", Weight: -1}, // pulled first
			"mid":   {Probe: "FPing", Host: "m"},             // weight 0
		}},
	}
	mons, err := cfg.Monitors()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range mons {
		names = append(names, m.Name)
	}
	want := []string{"omega", "mid", "alpha"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("Monitors order = %v, want %v", names, want)
	}
}

func TestOrderedChildren(t *testing.T) {
	m := map[string]*Node{
		"b": {}, "a": {}, // both weight 0 → tie-break by name
		"top":  {Weight: -5}, // negative sorts first
		"last": {Weight: 10},
		"mid":  {Weight: 1},
	}
	got := orderedChildren(m)
	want := []string{"top", "a", "b", "mid", "last"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderedChildren = %v, want %v", got, want)
	}

	if empty := orderedChildren(map[string]*Node{}); len(empty) != 0 {
		t.Fatalf("orderedChildren(empty map) = %v, want empty slice", empty)
	}
}

// Guards the yaml:"weight" tag: existing weight tests build *Node structs directly,
// so a typo in the struct tag would silently ignore `weight:` in real YAML files
// without any test catching it. This parses actual YAML through Parse (the same
// entry point every other config test uses) and confirms the decoded order matches.
func TestWeightRoundTripsThroughYAML(t *testing.T) {
	const src = `
targets:
  children:
    alpha: {probe: FPing, host: a, weight: 10}
    omega: {probe: FPing, host: o, weight: -1}
    mid:   {probe: FPing, host: m}
`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mons, err := c.Monitors()
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	var names []string
	for _, m := range mons {
		names = append(names, m.Name)
	}
	want := []string{"omega", "mid", "alpha"} // weight -1, 0, 10
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("YAML weight order = %v, want %v", names, want)
	}
}

// A node's stable id resolves to its explicit `id:` when set, else falls back to the
// flattened path — so a node moved between parent groups (path changes) keeps its
// storage identity only when it was given an explicit id.
func TestMonitorsResolvesStableID(t *testing.T) {
	c, err := Parse([]byte(`
database: {pings: 1, step: 1s}
probes: {FPing: {}}
targets:
  children:
    Resolvers:
      children:
        dns1: {probe: FPing, host: 192.168.1.5, id: "frozen-id-1"}
        dns2: {probe: FPing, host: 192.168.1.6}
`))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := c.Monitors()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // path -> id
	for _, m := range ms {
		got[m.Name] = m.ID
	}
	if got["Resolvers/dns1"] != "frozen-id-1" {
		t.Errorf("explicit id: got %q want frozen-id-1", got["Resolvers/dns1"])
	}
	if got["Resolvers/dns2"] != "Resolvers/dns2" {
		t.Errorf("id fallback to path: got %q want Resolvers/dns2", got["Resolvers/dns2"])
	}
}

// Two nodes that resolve to the same id (explicit or otherwise) would silently share
// one storage identity, so it must be rejected like a duplicate name.
func TestMonitorsRejectsDuplicateID(t *testing.T) {
	c, _ := Parse([]byte(`
database: {pings: 1, step: 1s}
probes: {FPing: {}}
targets:
  children:
    a: {probe: FPing, host: h1, id: dup}
    b: {probe: FPing, host: h2, id: dup}
`))
	if _, err := c.Monitors(); err == nil {
		t.Fatal("expected duplicate-id error, got nil")
	}
}
