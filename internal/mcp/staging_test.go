package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestValidateDocRejectsUnknownProbeParam(t *testing.T) {
	// The brief's scenario used probe kind "icmp", but internal/probe has no such
	// registered kind (native ICMP echo is registered as "Ping" — see
	// internal/probe/pingprobe/ping.go's probe.Register("Ping", ...)). Substituted per
	// task-7-brief.md's own allowance ("if icmp is not a registered probe kind ... we'll
	// pick a probe/param that does"): "Ping" is the closest analog (native ICMP, no
	// external binary) and its schema (packetsize/interval_ms/timeout_ms/mode) rejects an
	// unknown target param exactly like the brief intended for icmp.
	bad := json.RawMessage(`{"targets":{"children":{"x":{"host":"1.1.1.1","probe":"Ping","params":{"not_a_real_param":"z"}}}}}`)
	if _, err := validateDoc(nil, "", bad); err == nil {
		t.Fatal("expected validation error for unknown Ping param")
	}
	good := json.RawMessage(`{"targets":{"children":{"x":{"host":"1.1.1.1","probe":"Ping"}}}}`)
	if _, err := validateDoc(nil, "", good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestFlattenAndDiff(t *testing.T) {
	base := json.RawMessage(`{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	work := json.RawMessage(`{"targets":{"children":{"g":{"children":{"b":{"host":"2.2.2.2","probe":"Ping"}}}}}}`)
	fb, err := flatten(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fb["g/a"]; !ok {
		t.Fatalf("flatten missing g/a: %v keys", keysOf(fb))
	}
	cs, err := diffDocs(base, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Added) != 1 || cs.Added[0] != "g/b" || len(cs.Removed) != 1 || cs.Removed[0] != "g/a" {
		t.Fatalf("diff wrong: added=%v removed=%v", cs.Added, cs.Removed)
	}
	if len(cs.GroupsAdded)+len(cs.GroupsRemoved)+len(cs.GroupsChanged) != 0 {
		t.Fatalf("group g is unchanged, got groups added=%v removed=%v changed=%v", cs.GroupsAdded, cs.GroupsRemoved, cs.GroupsChanged)
	}
}

// TestStagingConcurrentAddsAllLand proves staging mutations are atomic under the go-sdk's
// concurrent tool dispatch (jsonrpc2.Async): N goroutines each stage a distinct target
// against one shared staging buffer, and every single one must land in the final working
// doc. A read-modify-write across two separate lock cycles (st.working() then, later,
// st.setDoc()) loses updates here — the last writer to call setDoc clobbers every
// intervening goroutine's addition with the stale doc it read before they committed. Run
// under -race: this doesn't race on any individual field (each access is mutex-guarded) —
// it deterministically loses updates, which is exactly the class of bug a lock-per-whole-
// operation (staging.mutate) fixes and a data race detector alone would not catch.
func TestStagingConcurrentAddsAllLand(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = stageAddTarget(st, addTargetIn{
				GroupPath: "G",
				Name:      fmt.Sprintf("t%02d", i),
				Host:      fmt.Sprintf("10.0.0.%d", i),
				Probe:     "Ping",
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("stageAddTarget(%d): %v", i, err)
		}
	}

	f, err := flatten(st.working())
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != n {
		t.Fatalf("expected %d targets to have landed, got %d: %v", n, len(f), keysOf(f))
	}
}

// flatten maps each target's path to its own settings as JSON, for asserting on a staged doc.
func flatten(doc json.RawMessage) (map[string]json.RawMessage, error) {
	tree, err := walkTree(doc)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for p, e := range tree {
		if e.target {
			out[p] = json.RawMessage(e.own)
		}
	}
	return out, nil
}

func keysOf(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// changeSummary is the change-summary shape shared by the config_stage_* results and config_review.
type changeSummary struct {
	Added         []string `json:"added"`
	Removed       []string `json:"removed"`
	Changed       []string `json:"changed"`
	GroupsAdded   []string `json:"groups_added"`
	GroupsRemoved []string `json:"groups_removed"`
	GroupsChanged []string `json:"groups_changed"`
	Warnings      []string `json:"warnings"`
}

// TestReviewReportsInheritedGroupChange is the regression test for a change summary that hid a
// group-level edit: changing only a hostless group's step alters every leaf beneath it, yet the
// stage result and config_review reported added=[] removed=[] changed=[]. The group must be
// reported, and so must every target whose inherited settings it changes.
func TestReviewReportsInheritedGroupChange(t *testing.T) {
	db := `{"targets":{"children":{"g":{"step":"60s","children":{"a":{"id":"a-id","host":"1.1.1.1","probe":"Ping"},"b":{"id":"b-id","host":"2.2.2.2","probe":"Ping"}}}}}}`
	c, _ := newTestClient(t, configHub(t, "", db))
	cs := mcpSession(t, c)

	edited := strings.Replace(db, `"step":"60s"`, `"step":"5s"`, 1)
	var staged changeSummary
	if res := callTool(t, cs, "config_stage_replace", map[string]any{"doc": edited}, &staged); res.IsError {
		t.Fatalf("config_stage_replace: %s", resultText(res))
	}
	var review changeSummary
	res := callTool(t, cs, "config_review", nil, &review)
	if res.IsError {
		t.Fatalf("config_review: %s", resultText(res))
	}
	for name, got := range map[string]changeSummary{"stage result": staged, "config_review": review} {
		if !slices.Equal(got.GroupsChanged, []string{"g"}) {
			t.Errorf("%s: groups_changed=%v, want [g]", name, got.GroupsChanged)
		}
		if !slices.Equal(got.Changed, []string{"g/a", "g/b"}) {
			t.Errorf("%s: changed=%v, want the targets that inherit the new step [g/a g/b]", name, got.Changed)
		}
		if len(got.Added)+len(got.Removed) != 0 {
			t.Errorf("%s: added=%v removed=%v, want none", name, got.Added, got.Removed)
		}
	}
	if text := resultText(res); !strings.Contains(text, "group") || !strings.Contains(text, "[g]") {
		t.Errorf("config_review text does not mention the group change: %q", text)
	}
}
