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
	c, err := resolveConfig(p, "", "", 0, 0, false)
	if err != nil || c.Hub != "https://hub.example" || c.Key != "smk_a_b" || c.Interval != 30*time.Second {
		t.Fatalf("file: %+v err=%v", c, err)
	}
	// flag override wins
	c, err = resolveConfig(p, "https://other.example", "", 0, 0, false)
	if err != nil || c.Hub != "https://other.example" {
		t.Fatalf("override: %+v err=%v", c, err)
	}
	// missing hub/key is an error
	if _, err := resolveConfig("", "", "", 0, 0, false); err == nil {
		t.Fatal("expected error when hub/key absent")
	}
}
