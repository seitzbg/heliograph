package smokeping

import "testing"

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
	root, sum := buildTree(secs)

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
	root, sum := buildTree(secs)

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
	root, sum := buildTree(secs)

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
	root, sum := buildTree(secs)

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
	root, _ := buildTree(secs)

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
