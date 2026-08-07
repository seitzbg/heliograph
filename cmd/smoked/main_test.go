package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"smokeping-modern/internal/federation"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"

	_ "smokeping-modern/internal/probe/tcpconnect" // register TCPConnect for the config
)

// warm-start must seed only the recent, cadence-contiguous, same-host/probe suffix — never
// stale or semantically-different history, which could fire a false alert at boot (#6).
func TestRecentContiguous(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := warmMeta{host: "h", probe: "FPing", step: time.Minute}
	rnd := func(ago time.Duration, host, pk string) scheduler.Outcome {
		return scheduler.Outcome{Target: probe.Target{Name: "t", Host: host}, ProbeName: pk, When: now.Add(-ago)}
	}

	// Old breaching rounds followed by a recent contiguous block: only the recent block seeds.
	hist := []scheduler.Outcome{
		rnd(90*24*time.Hour, "h", "FPing"),
		rnd(90*24*time.Hour-time.Minute, "h", "FPing"),
		rnd(2*time.Minute, "h", "FPing"),
		rnd(1*time.Minute, "h", "FPing"),
		rnd(0, "h", "FPing"),
	}
	if got := recentContiguous(hist, m, now); len(got) != 3 {
		t.Errorf("expected the 3 recent contiguous rounds, got %d", len(got))
	}

	// Newest stored round is stale -> seed nothing (the target is dark).
	stale := []scheduler.Outcome{rnd(2*time.Hour, "h", "FPing"), rnd(2*time.Hour-time.Minute, "h", "FPing")}
	if got := recentContiguous(stale, m, now); got != nil {
		t.Errorf("stale newest round should seed nothing, got %d", len(got))
	}

	// Newest round is from a different host (name reused) -> seed nothing.
	mismatch := []scheduler.Outcome{rnd(time.Minute, "h", "FPing"), rnd(0, "other", "FPing")}
	if got := recentContiguous(mismatch, m, now); got != nil {
		t.Errorf("host mismatch on the newest round should seed nothing, got %d", len(got))
	}

	// A cadence gap truncates to the contiguous suffix after it.
	gapped := []scheduler.Outcome{rnd(10*time.Minute, "h", "FPing"), rnd(time.Minute, "h", "FPing"), rnd(0, "h", "FPing")}
	if got := recentContiguous(gapped, m, now); len(got) != 2 {
		t.Errorf("expected 2 rounds after the gap, got %d", len(got))
	}
}

// The hub builds local probe jobs only for targets assigned to its own vantage (local):
// a remote-only target (`vantages: [nyc]`) must NOT be probed here (it would be a false
// local measurement), while a target whose vantages include `local` must be.
func TestBuildRuntimeHonorsVantages(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(`
database: { step: 30s, pings: 3 }
probes: { TCPConnect: {} }
targets:
  probe: TCPConnect
  children:
    here:   { host: 127.0.0.1, params: { port: "80" } }
    remote: { host: 127.0.0.1, params: { port: "80" }, vantages: [nyc] }
    both:   { host: 127.0.0.1, params: { port: "80" }, vantages: [local, nyc] }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := buildRuntime(cfg, 3, 30*time.Second, time.Second, nil)
	if err != nil {
		t.Fatalf("buildRuntime: %v", err)
	}
	got := map[string]bool{}
	for _, j := range rt.jobs {
		got[j.Target.Name] = true
	}
	if !got["here"] || !got["both"] {
		t.Errorf("expected local jobs for here+both (vantages include local), got %v", got)
	}
	if got["remote"] {
		t.Error("remote-only target (vantages: [nyc]) must not create a local probe job")
	}
	if len(rt.jobs) != 2 {
		t.Errorf("expected exactly 2 local jobs, got %d (%v)", len(rt.jobs), got)
	}
}

// The swappable runtime must retain the FULL post-inheritance monitor set (all
// vantages), not just the hub's local-filtered slice — the agent assignment endpoint
// (Task 4/5) computes a remote vantage's targets from it. The hub still builds local
// probe jobs only for its own vantage.
//
// Uses TCPConnect (no external binary dependency) rather than FPing so the test is
// portable to CI images without fping installed.
func TestRuntimeRetainsFullMonitorSet(t *testing.T) {
	dir := t.TempDir()
	cfg := "targets:\n" +
		"  probe: TCPConnect\n" +
		"  children:\n" +
		"    local-one: {host: 127.0.0.1}\n" +
		"    nyc-one:   {host: 1.1.1.1, vantages: [nyc]}\n"
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := buildRuntime(dir, 5, time.Second, time.Second, nil)
	if err != nil {
		t.Fatalf("buildRuntime: %v", err)
	}
	// The hub builds a local job only for the local target...
	if got := len(rt.jobs); got != 1 {
		t.Fatalf("local jobs=%d want 1", got)
	}
	// ...but retains the full set so it can serve the nyc assignment.
	nyc := federation.AssignmentFor(rt.monitors, "nyc")
	if len(nyc) != 1 || nyc[0].Name != "nyc-one" {
		t.Fatalf("nyc assignment=%+v", nyc)
	}
}
