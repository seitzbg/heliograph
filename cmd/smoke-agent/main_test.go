package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigSpoolDirFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	os.WriteFile(p, []byte("hub: https://h.example\nkey: k\nspool_dir: /var/lib/smoke-agent/spool\n"), 0o644)
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
	os.WriteFile(p, []byte("hub: https://h.example\nkey: k\nspool_dir: /from/file\n"), 0o644)
	cfg, err := resolveConfig(p, cliFlags{spoolDir: "/from/flag"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpoolDir != "/from/flag" {
		t.Fatalf("SpoolDir = %q, want flag override", cfg.SpoolDir)
	}
}

func TestResolveConfigSpoolDirDefaultsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.yaml")
	os.WriteFile(p, []byte("hub: https://h.example\nkey: k\n"), 0o644)
	cfg, _ := resolveConfig(p, cliFlags{})
	if cfg.SpoolDir != "" {
		t.Fatalf("SpoolDir = %q, want empty (in-memory only)", cfg.SpoolDir)
	}
}
