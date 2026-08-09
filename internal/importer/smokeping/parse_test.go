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
