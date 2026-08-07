package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "smokeping-modern/internal/probe/tcpconnect" // register TCPConnect for the config
)

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
