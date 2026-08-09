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
