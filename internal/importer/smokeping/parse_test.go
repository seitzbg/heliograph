package smokeping

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProjectsProbeParams(t *testing.T) {
	targets := "*** Targets ***\nprobe = FPing\n+ DNSProbes\nprobe = DNS\n++ G\nhost = 8.8.8.8\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n+ DNS\nbinary = /usr/bin/dig\nlookup = google.com\npings = 5\n"
	database := "*** Database ***\nstep = 300\npings = 20\n"
	root, sum, err := Parse(targets, probes, database)
	if err != nil {
		t.Fatal(err)
	}
	g := root.Children["DNSProbes"].Children["G"]
	if g.Params["lookup"] != "google.com" {
		t.Errorf("DNS lookup not projected: %+v", g.Params)
	}
	if sum.Pings != 20 || sum.Step != "300" {
		t.Errorf("database advisory wrong: step=%q pings=%d", sum.Step, sum.Pings)
	}
}

// Review finding 1: an inline presentation key (menu/title) on a target must
// never land in Summary.DroppedParams — only a genuinely-unsupported probe
// setting (here FPing's `binary`, which paramMap["FPing"] accepts nothing
// for) should. On a real SmokePing install nearly every target carries
// inline menu/title, so if those counted as "dropped" the field meant to
// flag real data loss would be swamped with presentation noise.
func TestParseDropsUnsupportedProbeParamsNotPresentationKeys(t *testing.T) {
	targets := "*** Targets ***\nprobe = FPing\n+ A\nhost = 10.0.0.1\nmenu = A Node\ntitle = Node A\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n"
	root, sum, err := Parse(targets, probes, "")
	if err != nil {
		t.Fatal(err)
	}
	a := root.Children["A"]
	if a == nil {
		t.Fatal("A missing")
	}
	if len(a.Params) != 0 {
		t.Errorf("FPing accepts no target params, want empty Params, got %+v", a.Params)
	}
	var foundBinary, foundMenu, foundTitle bool
	for _, d := range sum.DroppedParams {
		switch d {
		case "binary":
			foundBinary = true
		case "menu":
			foundMenu = true
		case "title":
			foundTitle = true
		}
	}
	if !foundBinary {
		t.Errorf("want unsupported probe setting `binary` in DroppedParams, got %+v", sum.DroppedParams)
	}
	if foundMenu || foundTitle {
		t.Errorf("presentation keys must never appear in DroppedParams, got %+v", sum.DroppedParams)
	}
}

// Review finding 2: Parse must propagate a Probes-body parse error instead
// of silently proceeding as if the Probes file were empty. parseSections
// only errors on a bufio.Scanner failure (in practice: a single line longer
// than its 1<<20-byte max buffer), so a lone, newline-free 2MB line is used
// to force that failure deterministically.
func TestParsePropagatesProbesParseError(t *testing.T) {
	targets := "*** Targets ***\nprobe = FPing\n+ A\nhost = a.example\n"
	tooLong := strings.Repeat("x", 2<<20) // no newline: one token > the 1<<20 scanner buffer cap
	_, _, err := Parse(targets, tooLong, "*** Database ***\nstep = 300\npings = 20\n")
	if err == nil {
		t.Fatal("want error from a malformed Probes body, got nil")
	}
}

// Review finding 2 (Database side): same as above, but for the Database
// body — Parse must not silently proceed with a zero-value advisory when
// Database itself fails to parse.
func TestParsePropagatesDatabaseParseError(t *testing.T) {
	targets := "*** Targets ***\nprobe = FPing\n+ A\nhost = a.example\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n"
	tooLong := strings.Repeat("x", 2<<20)
	_, _, err := Parse(targets, probes, tooLong)
	if err == nil {
		t.Fatal("want error from a malformed Database body, got nil")
	}
}

func TestParseSectionsNestingAndFields(t *testing.T) {
	in := "*** Targets ***\n" +
		"probe = FPing\n" +
		"menu = Top\n" +
		"\n" +
		"+ Local\n" +
		"menu = Local Sites\n" +
		"\n" +
		"++ pve1\n" +
		"host = pve1.example\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 3 {
		t.Fatalf("want 3 sections, got %d", len(secs))
	}
	if secs[0].Depth != 0 || secs[0].Name != "" || secs[0].Fields["probe"] != "FPing" {
		t.Errorf("root: %+v", secs[0])
	}
	if secs[1].Depth != 1 || secs[1].Name != "Local" {
		t.Errorf("Local: %+v", secs[1])
	}
	if secs[2].Depth != 2 || secs[2].Name != "pve1" || secs[2].Fields["host"] != "pve1.example" {
		t.Errorf("pve1: %+v", secs[2])
	}
}

func TestParseSectionsCommentsAndContinuation(t *testing.T) {
	in := "*** Targets ***\n" +
		"# a comment\n" +
		"remark = hello \\\n" +
		"         world\n" +
		"+ A\n" +
		"host = a.example\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	if secs[0].Fields["remark"] != "hello world" {
		t.Errorf("continuation not joined: %q", secs[0].Fields["remark"])
	}
	if _, ok := secs[0].Fields["# a comment"]; ok {
		t.Errorf("comment leaked into fields")
	}
}

func TestBuildTreeFoldersTargetsInheritanceAndProbeMap(t *testing.T) {
	in := "*** Targets ***\n" +
		"probe = FPing\n" +
		"menu = Top\n" +
		"+ Local\n" +
		"menu = Local Sites\n" +
		"++ pve1\n" +
		"host = pve1.example\n" +
		"+ DNSProbes\n" +
		"probe = DNS\n" +
		"++ GoogleDNS1\n" +
		"host = 8.8.8.8\n" +
		"+ Speed\n" +
		"probe = speedtestcli\n" +
		"++ Download\n" +
		"host = dummy\n"
	secs, _ := parseSections(in)
	root, sum, _ := buildTree(secs)

	local := root.Children["Local"]
	if local == nil || local.Host != "" {
		t.Fatalf("Local should be a folder: %+v", local)
	}
	pve1 := local.Children["pve1"]
	if pve1 == nil || pve1.Host != "pve1.example" || pve1.Probe != "FPing" {
		t.Errorf("pve1 wrong: %+v", pve1)
	}
	g := root.Children["DNSProbes"].Children["GoogleDNS1"]
	if g == nil || g.Probe != "DNS" || g.Host != "8.8.8.8" {
		t.Errorf("GoogleDNS1 wrong: %+v", g)
	}
	if _, ok := root.Children["Speed"]; ok {
		t.Errorf("speedtest folder should be pruned when it has no mappable targets")
	}
	if sum.Targets != 2 {
		t.Errorf("want 2 targets, got %d", sum.Targets)
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0].Probe != "speedtestcli" {
		t.Errorf("want 1 speedtest skip, got %+v", sum.Skipped)
	}
	// presentation dropped
	if g.Title != "" {
		t.Errorf("title should be dropped, got %q", g.Title)
	}
}

// Finding 1: a hosted node must never be pruned, even if its only child (a
// host-less, child-less "Junk" section) is itself pruned away. Pruning must
// key off Host, not "has no children".
func TestBuildTreePruneKeepsHostedNodeWithEmptyChildren(t *testing.T) {
	in := "*** Targets ***\n" +
		"probe = FPing\n" +
		"+ pve1\n" +
		"host = pve1.example\n" +
		"++ Junk\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	root, sum, _ := buildTree(secs)

	pve1 := root.Children["pve1"]
	if pve1 == nil || pve1.Host != "pve1.example" || pve1.Probe != "FPing" {
		t.Fatalf("pve1 (has a Host) must survive pruning even though its only child was pruned: %+v", pve1)
	}
	if len(pve1.Children) != 0 {
		t.Errorf("Junk (no host, no children) should have been pruned from pve1: %+v", pve1.Children)
	}
	if sum.Targets != 1 {
		t.Errorf("want 1 target (pve1), got %d", sum.Targets)
	}
}

// Finding 2: a mappable target nested under a skipped (unmapped-probe) target
// must still land in the tree, at the skipped target's place.
func TestBuildTreeMappedChildOfSkippedTargetSurvives(t *testing.T) {
	in := "*** Targets ***\n" +
		"+ Speed\n" +
		"probe = speedtestcli\n" +
		"++ Download\n" +
		"host = dummy\n" +
		"+++ SubFile\n" +
		"host = other\n" +
		"probe = FPing\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	root, sum, _ := buildTree(secs)

	download := root.Children["Speed"].Children["Download"]
	if download == nil {
		t.Fatalf("Download placeholder should stay linked (it has a rescued descendant): %+v", root)
	}
	sub := download.Children["SubFile"]
	if sub == nil || sub.Host != "other" || sub.Probe != "FPing" {
		t.Fatalf("SubFile (mapped FPing probe) should survive at Speed/Download/SubFile: %+v", download.Children)
	}
	if sum.Targets != 1 {
		t.Errorf("want 1 target (SubFile; Download itself was skipped), got %d", sum.Targets)
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0].Probe != "speedtestcli" {
		t.Errorf("want 1 skip for Download (speedtestcli), got %+v", sum.Skipped)
	}
}

// Finding 3: two sibling sections with the same name must not silently
// last-wins or double-count; the first wins and the second is recorded as a
// duplicate-name skip.
func TestBuildTreeDuplicateNameKeepsFirstAndSkipsSecond(t *testing.T) {
	in := "*** Targets ***\n" +
		"probe = FPing\n" +
		"+ dup\n" +
		"host = first.example\n" +
		"+ dup\n" +
		"host = second.example\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	root, sum, _ := buildTree(secs)

	d := root.Children["dup"]
	if d == nil || d.Host != "first.example" {
		t.Fatalf("first `dup` section should win: %+v", d)
	}
	if sum.Targets != 1 {
		t.Errorf("want 1 target (duplicate not double-counted), got %d", sum.Targets)
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0].Reason != "duplicate name" {
		t.Errorf("want 1 duplicate-name skip, got %+v", sum.Skipped)
	}
}

// Finding 4: a section whose depth jumps more than one level past its
// predecessor (e.g. "+" then "+++" with no "++" in between) must still nest
// under the right ancestor — and a following section at that same jumped-to
// depth must be its sibling, not get nested under it.
func TestBuildTreeDepthJumpSiblingNotChild(t *testing.T) {
	in := "*** Targets ***\n" +
		"probe = FPing\n" +
		"+ A\n" +
		"+++ C\n" +
		"host = c.example\n" +
		"+++ D\n" +
		"host = d.example\n"
	secs, err := parseSections(in)
	if err != nil {
		t.Fatal(err)
	}
	root, _, _ := buildTree(secs)

	a := root.Children["A"]
	if a == nil {
		t.Fatalf("A missing: %+v", root.Children)
	}
	c := a.Children["C"]
	if c == nil || c.Host != "c.example" {
		t.Fatalf("C should be a child of A: %+v", a.Children)
	}
	if _, ok := a.Children["D"]; !ok {
		t.Errorf("D should be a sibling of C under A (depth jump must not mis-nest it under C): %+v", a.Children)
	}
	if _, ok := c.Children["D"]; ok {
		t.Errorf("D must NOT be nested under C")
	}
}

// TestParseFixture drives Parse end-to-end off a checked-in synthetic
// SmokePing install (testdata/smokeping/{Targets,Probes,Database} — no real
// hostnames/IPs) covering: a nested folder->folder->target chain on the
// inherited top-level FPing probe (its leaf target carries inline
// menu/title, like nearly every target on a real SmokePing install), a
// DNSProbes folder overriding probe = DNS with two targets (probe-level
// `lookup` projected from the Probes file), a TCPPing target with an inline
// `port` overriding the probe's file-level default, a speedtestcli target
// that must be skipped (and its now-empty folder pruned), and the Database
// file's step/pings advisory.
func TestParseFixture(t *testing.T) {
	dir := "testdata/smokeping"
	targets := readFixture(t, filepath.Join(dir, "Targets"))
	probes := readFixture(t, filepath.Join(dir, "Probes"))
	database := readFixture(t, filepath.Join(dir, "Database"))

	root, sum, err := Parse(targets, probes, database)
	if err != nil {
		t.Fatal(err)
	}

	if sum.Targets != 4 {
		t.Errorf("want 4 targets (leaf, Primary, Secondary, Web; Download skipped), got %d", sum.Targets)
	}

	leaf := root.Children["A"].Children["B"].Children["leaf"]
	if leaf == nil || leaf.Host != "10.0.0.1" || leaf.Probe != "FPing" {
		t.Fatalf("deep path A/B/leaf wrong: %+v", leaf)
	}
	if len(leaf.Params) != 0 {
		t.Errorf("leaf's inline menu/title are presentation, not FPing params: want empty Params, got %+v", leaf.Params)
	}

	primary := root.Children["DNSProbes"].Children["Primary"]
	if primary == nil || primary.Probe != "DNS" || primary.Params["lookup"] != "example.com" {
		t.Fatalf("DNSProbes/Primary lookup not projected from Probes file: %+v", primary)
	}

	web := root.Children["TCPChecks"].Children["Web"]
	if web == nil || web.Probe != "TCPConnect" || web.Params["port"] != "8443" {
		t.Fatalf("TCPChecks/Web inline port override not projected: %+v", web)
	}

	if _, ok := root.Children["Speed"]; ok {
		t.Errorf("Speed folder should be pruned: its only target (speedtestcli) was skipped")
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0].Probe != "speedtestcli" {
		t.Errorf("want 1 speedtestcli skip, got %+v", sum.Skipped)
	}

	if sum.Step != "300" || sum.Pings != 20 {
		t.Errorf("database advisory wrong: step=%q pings=%d", sum.Step, sum.Pings)
	}

	var foundBinary, foundMenu, foundTitle bool
	for _, d := range sum.DroppedParams {
		switch d {
		case "binary":
			foundBinary = true
		case "menu":
			foundMenu = true
		case "title":
			foundTitle = true
		}
	}
	if !foundBinary {
		t.Errorf("want probe-level `binary` (shared by every probe, not in any paramMap entry) deduped into DroppedParams, got %+v", sum.DroppedParams)
	}
	if foundMenu || foundTitle {
		t.Errorf("leaf's inline menu/title are presentation, not probe params: must not appear in DroppedParams, got %+v", sum.DroppedParams)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	return string(b)
}
