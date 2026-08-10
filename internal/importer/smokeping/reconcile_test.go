package smokeping

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTargetsResolvesPingsFromFixture drives Targets end-to-end off the
// slice-A synthetic fixture and asserts the SmokePing pings precedence
// (target inline > Probes-file probe > Database default) for two of its four
// leaves: the FPing leaf (A/B/leaf) has no probe- or target-level pings
// override anywhere, so it falls all the way through to the Database file's
// default (20); the DNS targets (DNSProbes/Primary, DNSProbes/Secondary)
// inherit the DNSProbes section's `probe = DNS`, and the Probes file's DNS
// section sets pings=5, overriding the Database default.
func TestTargetsResolvesPingsFromFixture(t *testing.T) {
	dir := "testdata/smokeping"
	targets := readFixture(t, filepath.Join(dir, "Targets"))
	probes := readFixture(t, filepath.Join(dir, "Probes"))
	database := readFixture(t, filepath.Join(dir, "Database"))

	its, sum, err := Targets(targets, probes, database)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Targets != 4 {
		t.Errorf("want 4 targets in summary (leaf, Primary, Secondary, Web; Download skipped), got %d", sum.Targets)
	}
	if len(its) != 4 {
		t.Fatalf("want 4 flattened targets, got %d: %+v", len(its), its)
	}

	by := map[string]ImportTarget{}
	for _, it := range its {
		by[it.Name] = it
	}

	leaf, ok := by["A/B/leaf"]
	if !ok {
		t.Fatalf("A/B/leaf missing from %+v", its)
	}
	if leaf.Host != "10.0.0.1" || leaf.Probe != "FPing" || leaf.Pings != 20 {
		t.Errorf("A/B/leaf: want Host=10.0.0.1 Probe=FPing Pings=20 (Database default), got %+v", leaf)
	}

	primary, ok := by["DNSProbes/Primary"]
	if !ok {
		t.Fatalf("DNSProbes/Primary missing from %+v", its)
	}
	if primary.Host != "10.0.0.53" || primary.Probe != "DNS" || primary.Pings != 5 {
		t.Errorf("DNSProbes/Primary: want Host=10.0.0.53 Probe=DNS Pings=5 (Probes-file DNS override), got %+v", primary)
	}

	secondary, ok := by["DNSProbes/Secondary"]
	if !ok || secondary.Pings != 5 {
		t.Errorf("DNSProbes/Secondary: want Pings=5 (Probes-file DNS override), got %+v", secondary)
	}

	web, ok := by["TCPChecks/Web"]
	if !ok || web.Probe != "TCPConnect" || web.Pings != 20 {
		t.Errorf("TCPChecks/Web: want Probe=TCPConnect Pings=20 (Database default, TCPPing has no Probes-file pings), got %+v", web)
	}
}

// TestTargetsInlinePingsOverridesProbeAndDatabase covers the top of the
// precedence chain in isolation: a target-level inline `pings` must win over
// both its SmokePing probe's Probes-file `pings` and the Database default,
// which the fixture alone (no inline pings override) doesn't exercise.
func TestTargetsInlinePingsOverridesProbeAndDatabase(t *testing.T) {
	targets := "*** Targets ***\nprobe = FPing\n+ A\nhost = 10.0.0.1\npings = 3\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\npings = 10\n"
	database := "*** Database ***\nstep = 300\npings = 20\n"

	its, _, err := Targets(targets, probes, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 {
		t.Fatalf("want 1 target, got %d: %+v", len(its), its)
	}
	if its[0].Name != "A" || its[0].Pings != 3 {
		t.Errorf("inline pings=3 must win over probe pings=10 and Database pings=20, got %+v", its[0])
	}
}

// TestReconcileMatchesConfigOnlyAndOrphans mirrors the real drift Reconcile
// exists to surface: three of the fixture's four targets have a matching
// .rrd under a temp dataDir, one (TCPChecks/Web) has none (ConfigOnly), and
// one .rrd exists with no corresponding target at all (an Orphan — e.g. a
// target renamed or removed from Targets after data collection started).
func TestReconcileMatchesConfigOnlyAndOrphans(t *testing.T) {
	dir := "testdata/smokeping"
	targets := readFixture(t, filepath.Join(dir, "Targets"))
	probes := readFixture(t, filepath.Join(dir, "Probes"))
	database := readFixture(t, filepath.Join(dir, "Database"))

	its, _, err := Targets(targets, probes, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 4 {
		t.Fatalf("want 4 targets, got %d: %+v", len(its), its)
	}

	dataDir := t.TempDir()
	touchRRD(t, dataDir, "A/B/leaf")
	touchRRD(t, dataDir, "DNSProbes/Primary")
	touchRRD(t, dataDir, "DNSProbes/Secondary")
	// TCPChecks/Web deliberately has no .rrd -> ConfigOnly.
	touchRRD(t, dataDir, "Orphaned/Ghost") // no matching target -> Orphan.

	rec, err := Reconcile(its, dataDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(rec.Matched) != 3 {
		t.Errorf("want 3 matched, got %d: %+v", len(rec.Matched), rec.Matched)
	}
	matchedNames := map[string]bool{}
	for _, m := range rec.Matched {
		matchedNames[m.Name] = true
	}
	for _, want := range []string{"A/B/leaf", "DNSProbes/Primary", "DNSProbes/Secondary"} {
		if !matchedNames[want] {
			t.Errorf("want %q in Matched, got %+v", want, rec.Matched)
		}
	}

	if len(rec.ConfigOnly) != 1 || rec.ConfigOnly[0].Name != "TCPChecks/Web" {
		t.Errorf("want 1 config-only target (TCPChecks/Web), got %+v", rec.ConfigOnly)
	}

	if len(rec.Orphans) != 1 || rec.Orphans[0] != "Orphaned/Ghost" {
		t.Errorf("want 1 orphan (Orphaned/Ghost), got %+v", rec.Orphans)
	}
}

// TestReconcileOrphansSorted covers Reconcile's documented sort of Orphans:
// WalkDir visits directories in lexical order already, but this pins the
// contract explicitly rather than relying on that incidentally.
func TestReconcileOrphansSorted(t *testing.T) {
	dataDir := t.TempDir()
	touchRRD(t, dataDir, "zzz/last")
	touchRRD(t, dataDir, "aaa/first")
	touchRRD(t, dataDir, "mmm/middle")

	rec, err := Reconcile(nil, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaa/first", "mmm/middle", "zzz/last"}
	if len(rec.Orphans) != len(want) {
		t.Fatalf("want %d orphans, got %d: %+v", len(want), len(rec.Orphans), rec.Orphans)
	}
	for i, w := range want {
		if rec.Orphans[i] != w {
			t.Errorf("Orphans[%d] = %q, want %q (full: %+v)", i, rec.Orphans[i], w, rec.Orphans)
		}
	}
}

func touchRRD(t *testing.T, dataDir, name string) {
	t.Helper()
	p := filepath.Join(dataDir, name+".rrd")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rrd"), 0o644); err != nil {
		t.Fatal(err)
	}
}
