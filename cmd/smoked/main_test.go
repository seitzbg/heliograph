package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smokeping-modern/internal/alert"
	"smokeping-modern/internal/api"
	"smokeping-modern/internal/configstore"
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
	rt, err := buildRuntime(cfg, 3, 30*time.Second, time.Second, nil, nil)
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
	rt, err := buildRuntime(dir, 5, time.Second, time.Second, nil, nil)
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

func TestBuildRuntimeMergesDBFragment(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("targets:\n  children:\n    yaml-t: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"80\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getter := func() ([]byte, error) {
		return []byte(`{"targets":{"children":{"db-t":{"probe":"TCPConnect","host":"127.0.0.1","params":{"port":"80"}}}}}`), nil
	}
	rt, err := buildRuntime(cfgPath, 1, time.Second, time.Second, map[string]alert.Notifier{}, getter)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range rt.monitors {
		names[m.Name] = true
	}
	if !names["yaml-t"] || !names["db-t"] {
		t.Fatalf("want both yaml-t and db-t in monitors, got %v", names)
	}
}

func TestBuildRuntimeDBFragmentCollision(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("targets:\n  children:\n    dup: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"80\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getter := func() ([]byte, error) {
		return []byte(`{"targets":{"children":{"dup":{"probe":"TCPConnect","host":"127.0.0.1","params":{"port":"80"}}}}}`), nil
	}
	if _, err := buildRuntime(cfgPath, 1, time.Second, time.Second, map[string]alert.Notifier{}, getter); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-branch error, got %v", err)
	}
}

func TestSwapRuntimeInstallsAndInherits(t *testing.T) {
	var current atomic.Pointer[runtime]
	var mu sync.Mutex
	old := &runtime{jobs: nil} // nil engine → inheritance guarded off
	current.Store(old)
	nrt := &runtime{}
	swapRuntime(&current, &mu, nrt)
	if current.Load() != nrt {
		t.Fatal("swapRuntime did not install the new runtime")
	}
}

func TestApplyConfigValidatesPersistsSwaps(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run applyConfig test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	var current atomic.Pointer[runtime]
	var mu sync.Mutex
	current.Store(&runtime{})

	built := &runtime{jobs: []scheduler.Job{{}}} // sentinel distinct runtime
	goodBuild := func(func() ([]byte, error)) (*runtime, error) { return built, nil }
	badBuild := func(func() ([]byte, error)) (*runtime, error) { return nil, errors.New("boom") }

	// invalid → ErrConfigInvalid, NOT persisted, NOT swapped
	_, verBefore, _ := cs.Get(ctx)
	if err := applyConfig(cs, &current, &mu, badBuild, json.RawMessage(`{}`), verBefore); !errors.Is(err, api.ErrConfigInvalid) {
		t.Fatalf("want ErrConfigInvalid, got %v", err)
	}
	if _, verAfter, _ := cs.Get(ctx); verAfter != verBefore {
		t.Fatalf("invalid doc must not persist: %d -> %d", verBefore, verAfter)
	}
	if current.Load() == built {
		t.Fatal("invalid doc must not swap")
	}

	// valid → persists (version bumps) + swaps
	if err := applyConfig(cs, &current, &mu, goodBuild, json.RawMessage(`{"targets":{"children":{}}}`), verBefore); err != nil {
		t.Fatalf("valid apply: %v", err)
	}
	if _, verAfter, _ := cs.Get(ctx); verAfter != verBefore+1 {
		t.Fatalf("valid doc should bump version %d -> %d", verBefore, verAfter)
	}
	if current.Load() != built {
		t.Fatal("valid doc should swap in the built runtime")
	}

	// stale version → ErrConfigConflict
	if err := applyConfig(cs, &current, &mu, goodBuild, json.RawMessage(`{}`), verBefore); !errors.Is(err, api.ErrConfigConflict) {
		t.Fatalf("want ErrConfigConflict on stale version, got %v", err)
	}
}

func TestValidateRuntimeFlags(t *testing.T) {
	cases := []struct {
		name          string
		pings         int
		step, timeout time.Duration
		wantErr       bool
	}{
		{"valid", 10, 5 * time.Second, 4 * time.Second, false},
		{"pings zero", 0, 5 * time.Second, 4 * time.Second, true},
		{"pings too many", 1 << 20, 5 * time.Second, 4 * time.Second, true},
		{"step too small", 10, time.Millisecond, 4 * time.Second, true},
		{"timeout zero", 10, 5 * time.Second, 0, true},
		{"timeout negative", 10, 5 * time.Second, -time.Second, true},
	}
	for _, c := range cases {
		if err := validateRuntimeFlags(c.pings, c.step, c.timeout); (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// TestApplyMuSerializesRuntimeSwaps models CODE_REVIEW #1: a slow "SIGHUP" build (A)
// racing an "API apply" (B) that starts later. When both hold applyMu across their whole
// build+swap, A cannot swap a stale runtime AFTER B: B blocks on the lock until A finishes,
// then swaps last, so the later replacement is the live one. Without applyMu around the
// build, A (finishing its build last) would clobber B — the bug.
func TestApplyMuSerializesRuntimeSwaps(t *testing.T) {
	var current atomic.Pointer[runtime]
	var evalMu, applyMu sync.Mutex
	current.Store(&runtime{})
	rtA, rtB := &runtime{}, &runtime{} // distinct runtimes; B (later apply) must win
	reload := func(buildDelay time.Duration, nrt *runtime) {
		applyMu.Lock()
		defer applyMu.Unlock()
		time.Sleep(buildDelay) // simulate build time while holding the lock
		swapRuntime(&current, &evalMu, nrt)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reload(80*time.Millisecond, rtA) }() // A grabs the lock first, slow build
	time.Sleep(15 * time.Millisecond)
	go func() { defer wg.Done(); reload(0, rtB) }() // B blocks on the lock, swaps after A
	wg.Wait()
	if current.Load() != rtB {
		t.Fatal("the later runtime replacement (B) must be live; a stale swap clobbered it")
	}
}
