package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveConfigFileAndFlagOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://hub.example\nkey: smk_a_b\nvantage: nyc\ninterval: 30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// file only
	c, err := resolveConfig(p, cliFlags{})
	if err != nil || c.Hub != "https://hub.example" || c.Key != "smk_a_b" || c.Interval != 30*time.Second {
		t.Fatalf("file: %+v err=%v", c, err)
	}
	// flag override wins
	c, err = resolveConfig(p, cliFlags{hub: "https://other.example"})
	if err != nil || c.Hub != "https://other.example" {
		t.Fatalf("override: %+v err=%v", c, err)
	}
	// missing hub/key is an error
	if _, err := resolveConfig("", cliFlags{}); err == nil {
		t.Fatal("expected error when hub/key absent")
	}
}

// resolveConfig must run a single validation over the fully-merged config, so an invalid
// value provided BY FLAG (applied after the file) is still rejected — including the
// flush_max: -1 that would later panic peekBatch (CODE_REVIEW #9 / P2-9).
func TestResolveConfigRejectsInvalidValues(t *testing.T) {
	base := cliFlags{hub: "https://hub.example", key: "smk_a_b"}
	cases := []struct {
		name string
		f    cliFlags
	}{
		{"negative flush_max", withFlushMax(base, -1)},
		{"zero-ish negative workers", withWorkers(base, -1)},
		{"negative buffer", withBuffer(base, -5)},
		{"negative interval", withInterval(base, -time.Second)},
		{"negative timeout", withTimeout(base, -time.Second)},
	}
	for _, tc := range cases {
		if _, err := resolveConfig("", tc.f); err == nil {
			t.Errorf("%s: expected a validation error, got none", tc.name)
		}
	}
	// A clean flag-only config still resolves.
	if _, err := resolveConfig("", base); err != nil {
		t.Fatalf("valid flag-only config rejected: %v", err)
	}
}

func withFlushMax(f cliFlags, v int) cliFlags           { f.flushMax = v; return f }
func withWorkers(f cliFlags, v int) cliFlags            { f.workers = v; return f }
func withBuffer(f cliFlags, v int) cliFlags             { f.buffer = v; return f }
func withInterval(f cliFlags, v time.Duration) cliFlags { f.interval = v; return f }
func withTimeout(f cliFlags, v time.Duration) cliFlags  { f.timeout = v; return f }

// A misspelled/unknown YAML key must be a startup error (strict decode), not silently
// ignored — otherwise a typo'd setting looks applied but isn't (P2-9).
func TestResolveConfigRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://hub.example\nkey: smk_a_b\nflushmax: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveConfig(p, cliFlags{}); err == nil {
		t.Fatal("expected an error for the unknown key 'flushmax' (should be flush_max)")
	}
}
