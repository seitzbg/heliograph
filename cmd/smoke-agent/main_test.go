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

// An explicit `-insecure=false` must override a file's `insecure: true`; an omitted flag
// (nil) must leave the file value alone (CodeRabbit #5).
func TestResolveConfigInsecureFlagOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://hub.example\nkey: smk_a_b\ninsecure: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ptr := func(b bool) *bool { return &b }

	// Flag omitted (nil): keep the file's insecure: true.
	if c, err := resolveConfig(p, cliFlags{}); err != nil || !c.Insecure {
		t.Fatalf("omitted flag: Insecure=%v err=%v, want true", c.Insecure, err)
	}
	// Explicit -insecure=false: overrides the file to false.
	if c, err := resolveConfig(p, cliFlags{insecure: ptr(false)}); err != nil || c.Insecure {
		t.Fatalf("explicit false: Insecure=%v err=%v, want false", c.Insecure, err)
	}
	// Explicit -insecure=true against a file with no insecure key: true.
	p2 := filepath.Join(dir, "agent2.yaml")
	if err := os.WriteFile(p2, []byte("hub: https://hub.example\nkey: smk_a_b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := resolveConfig(p2, cliFlags{insecure: ptr(true)}); err != nil || !c.Insecure {
		t.Fatalf("explicit true: Insecure=%v err=%v, want true", c.Insecure, err)
	}
}

// A config file with a trailing second YAML document must be rejected, not silently ignored —
// otherwise settings in the second doc (e.g. a misspelled key) look applied but aren't (CodeRabbit #6).
func TestResolveConfigRejectsMultipleDocuments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	body := "hub: https://hub.example\nkey: smk_a_b\n---\nflushmax: 10\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveConfig(p, cliFlags{}); err == nil {
		t.Fatal("expected an error for a config with multiple YAML documents")
	}
}

func TestResolveConfigSpoolDirFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://h.example\nkey: k\nspool_dir: /var/lib/smoke-agent/spool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveConfig(p, cliFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpoolDir != "/var/lib/smoke-agent/spool" {
		t.Fatalf("SpoolDir = %q", cfg.SpoolDir)
	}
}

func TestResolveConfigSpoolDirFlagOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://h.example\nkey: k\nspool_dir: /from/file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveConfig(p, cliFlags{spoolDir: strp("/from/flag")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpoolDir != "/from/flag" {
		t.Fatalf("SpoolDir = %q, want flag override", cfg.SpoolDir)
	}
}

// strp returns a pointer to s, for setting the *string cliFlags.spoolDir in tests (nil = flag
// omitted; non-nil, incl. the empty string, = flag explicitly passed).
func strp(s string) *string { return &s }

// An explicit empty `-spool-dir=` must disable a file-configured spool (select in-memory mode),
// not be treated as "flag not passed". Regression for the plain-string bug where `-spool-dir=`
// left the YAML spool active (CODE_REVIEW #3).
func TestResolveConfigEmptySpoolDirFlagDisablesFileSpool(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://h.example\nkey: k\nspool_dir: /from/file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveConfig(p, cliFlags{spoolDir: strp("")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpoolDir != "" {
		t.Fatalf("SpoolDir = %q, want empty (explicit -spool-dir= disables the file spool)", cfg.SpoolDir)
	}
}

func TestResolveConfigSpoolDirDefaultsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte("hub: https://h.example\nkey: k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := resolveConfig(p, cliFlags{})
	if cfg.SpoolDir != "" {
		t.Fatalf("SpoolDir = %q, want empty (in-memory only)", cfg.SpoolDir)
	}
}
