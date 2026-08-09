package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"smokeping-modern/internal/config"
)

// renderFragmentYAML must marshal a tidy YAML fragment: target fields present,
// but none of the null-emitting Node fields (alerts/alertee/vantages) or a
// literal "null" anywhere in the output.
func TestRenderFragmentYAMLOmitsNulls(t *testing.T) {
	root := &config.Node{Children: map[string]*config.Node{
		"a": {Probe: "FPing", Host: "a.example"},
	}}
	y, err := renderFragmentYAML(root)
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if !strings.Contains(s, "host: a.example") || !strings.Contains(s, "probe: FPing") {
		t.Fatalf("missing target fields:\n%s", s)
	}
	for _, bad := range []string{"alerts:", "alertee:", "vantages:", "null"} {
		if strings.Contains(s, bad) {
			t.Errorf("output should omit %q:\n%s", bad, s)
		}
	}
}

// TestImportCmdWritesCleanYAML runs the CLI end-to-end against a small inline
// SmokePing config dir (no DB): default (no --apply) output should hold the
// expected FPing/DNS targets, skip the unmapped speedtest probe, and contain
// neither a literal "null" nor the word "speedtest".
func TestImportCmdWritesCleanYAML(t *testing.T) {
	dir := t.TempDir()
	targets := "*** Targets ***\nprobe = FPing\n" +
		"+ Local\nhost = localhost\n" +
		"+ Remote\nhost = example.com\nprobe = DNS\nlookup = example.com\n" +
		"+ Bad\nhost = slow.example\nprobe = speedtest\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n+ DNS\nbinary = /usr/bin/dig\n"
	database := "*** Database ***\nstep = 300\npings = 20\n"
	writeFile(t, filepath.Join(dir, "Targets"), targets)
	writeFile(t, filepath.Join(dir, "Probes"), probes)
	writeFile(t, filepath.Join(dir, "Database"), database)

	out := filepath.Join(t.TempDir(), "targets.yaml")
	code := importCmd([]string{"smokeping", dir, "--out", out})
	if code != 0 {
		t.Fatalf("importCmd exit code = %d, want 0", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "host: localhost") {
		t.Errorf("missing Local target:\n%s", s)
	}
	if !strings.Contains(s, "host: example.com") || !strings.Contains(s, "lookup: example.com") {
		t.Errorf("missing Remote DNS target:\n%s", s)
	}
	if strings.Contains(s, "speedtest") {
		t.Errorf("unmapped speedtest probe leaked into output:\n%s", s)
	}
	if strings.Contains(s, "null") {
		t.Errorf("output should never contain a literal null:\n%s", s)
	}
}

// TestImportCmdRejectsMissingDir covers the CLI-argument-error path.
func TestImportCmdRejectsMissingDir(t *testing.T) {
	if code := importCmd([]string{"notsmokeping"}); code != 2 {
		t.Errorf("importCmd with bad subcommand = %d, want 2", code)
	}
	if code := importCmd([]string{"smokeping"}); code != 2 {
		t.Errorf("importCmd with no dir = %d, want 2", code)
	}
	if code := importCmd([]string{"smokeping", "--out", "x"}); code != 2 {
		t.Errorf("importCmd with dir looking like a flag = %d, want 2", code)
	}
}

// TestImportCmdMissingTargetsFileErrors covers the read path: a dir with no
// Targets file (wrong dir, or an install directory the caller pointed at by
// mistake) must fail loudly with a non-zero exit and write nothing, not
// silently succeed with an empty `targets: {}` fragment.
func TestImportCmdMissingTargetsFileErrors(t *testing.T) {
	dir := t.TempDir() // empty: no Targets file (Probes/Database absent too)
	out := filepath.Join(t.TempDir(), "targets.yaml")
	code := importCmd([]string{"smokeping", dir, "--out", out})
	if code == 0 {
		t.Fatalf("importCmd against a dir with no Targets file = %d, want non-zero", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("importCmd should not have written %s when Targets is missing", out)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
