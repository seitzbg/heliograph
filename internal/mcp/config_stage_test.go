package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// fileTargetHub is a hub file config defining one target, like a typical running hub (a hub whose
// whole tree is empty fails its own validation and can't run).
const fileTargetHub = "targets:\n  children:\n    file:\n      host: localhost\n      probe: Ping\n"

// stagedClient returns a client for a hub whose DB config fragment is initial, plus a fresh
// staging buffer.
func stagedClient(t *testing.T, initial string) (*Client, *staging) {
	c, _ := newTestClient(t, configHub(t, fileTargetHub, initial))
	return c, newStaging()
}

func TestStageAddTargetMintsIDAndValidates(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// "Ping" is a registered probe kind (native ICMP, internal/probe/pingprobe) that needs
	// no params — see staging_test.go's TestValidateDocRejectsUnknownProbeParam. The brief's
	// "http" is not a registered kind and would fail setDoc's local validation.
	err := stageAddTarget(st, addTargetIn{GroupPath: "Websites", Name: "example", Host: "example.com", Probe: "Ping"})
	if err != nil {
		t.Fatalf("stageAddTarget: %v", err)
	}
	f, _ := flatten(st.working())
	node, ok := f["Websites/example"]
	if !ok {
		t.Fatalf("target not added: %v", keysOf(f))
	}
	var n map[string]any
	_ = json.Unmarshal(node, &n)
	if n["id"] == nil || n["id"] == "" {
		t.Fatalf("id not minted: %v", n)
	}
}

func TestStageRemoveTargetPrunesEmptyGroup(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	_ = st.ensure(context.Background(), c)
	if err := stageRemoveTarget(st, "g/a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f, _ := flatten(st.working())
	if len(f) != 0 {
		t.Fatalf("expected empty tree, got %v", keysOf(f))
	}
	// The now-empty group g must be pruned (empty groups can't be saved).
	var root struct {
		Targets struct {
			Children map[string]json.RawMessage `json:"children"`
		} `json:"targets"`
	}
	_ = json.Unmarshal(st.working(), &root)
	if _, ok := root.Targets.Children["g"]; ok {
		t.Fatalf("empty group g was not pruned")
	}
}

// TestStageEditTargetRejectsCollisionMoveOntoAnotherTarget guards against the data-loss bug
// found in code review: moving/renaming a target onto a name already held by a DIFFERENT
// target must be rejected, not silently overwrite the occupant (its whole subtree + stable
// ID) with no downstream check ever catching it.
func TestStageEditTargetRejectsCollisionMoveOntoAnotherTarget(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"},"b":{"host":"2.2.2.2","probe":"Ping"}}}}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	before := st.working()

	err := stageEditTarget(st, editTargetIn{Target: "g/a", NewName: "b"})
	if err == nil {
		t.Fatal("expected a collision error, got nil")
	}
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("expected error to wrap ErrConfigInvalid, got: %v", err)
	}
	if string(st.working()) != string(before) {
		t.Fatalf("working doc changed after a rejected move:\nbefore: %s\nafter:  %s", before, st.working())
	}

	f, _ := flatten(st.working())
	if _, ok := f["g/a"]; !ok {
		t.Fatalf("a was removed by the rejected move: %v", keysOf(f))
	}
	bNode, ok := f["g/b"]
	if !ok {
		t.Fatalf("b was clobbered by the rejected move: %v", keysOf(f))
	}
	var n map[string]any
	_ = json.Unmarshal(bNode, &n)
	if n["host"] != "2.2.2.2" {
		t.Fatalf("b's data was overwritten: %v", n)
	}
}

// TestStageEditTargetUpdatesHost is the happy-path field-edit case: stageEditTarget was
// otherwise untested, which is how the collision bug above went unnoticed.
func TestStageEditTargetUpdatesHost(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := stageEditTarget(st, editTargetIn{Target: "g/a", Host: "9.9.9.9"}); err != nil {
		t.Fatalf("stageEditTarget: %v", err)
	}
	f, _ := flatten(st.working())
	node, ok := f["g/a"]
	if !ok {
		t.Fatalf("target missing after edit: %v", keysOf(f))
	}
	var n map[string]any
	_ = json.Unmarshal(node, &n)
	if n["host"] != "9.9.9.9" {
		t.Fatalf("host not updated: %v", n)
	}
}

// TestStageEditTargetMoveKeepsID moves a target to a FREE name/group (no collision) and
// asserts its stable id survives the move — the whole point of detach-then-reattach on the
// same *config.Node instead of delete+recreate.
func TestStageEditTargetMoveKeepsID(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := stageAddTarget(st, addTargetIn{GroupPath: "Websites", Name: "example", Host: "example.com", Probe: "Ping"}); err != nil {
		t.Fatalf("stageAddTarget: %v", err)
	}
	f, _ := flatten(st.working())
	before, ok := f["Websites/example"]
	if !ok {
		t.Fatalf("target not added: %v", keysOf(f))
	}
	var beforeN map[string]any
	_ = json.Unmarshal(before, &beforeN)
	id, _ := beforeN["id"].(string)
	if id == "" {
		t.Fatalf("id not minted: %v", beforeN)
	}

	if err := stageEditTarget(st, editTargetIn{Target: "Websites/example", NewGroupPath: "Other", NewName: "moved"}); err != nil {
		t.Fatalf("stageEditTarget move: %v", err)
	}
	f2, _ := flatten(st.working())
	if _, ok := f2["Websites/example"]; ok {
		t.Fatalf("old path still present after move: %v", keysOf(f2))
	}
	after, ok := f2["Other/moved"]
	if !ok {
		t.Fatalf("target not found at new path: %v", keysOf(f2))
	}
	var afterN map[string]any
	_ = json.Unmarshal(after, &afterN)
	if afterN["id"] != id {
		t.Fatalf("id changed across move: before=%q after=%v", id, afterN["id"])
	}
}

// TestStageEditTargetUpdatesStep covers the Step field on editTargetIn (absent until this
// fix — addTargetIn already supported staging a per-target polling interval on create, but
// editTargetIn had no way to change it after the fact).
func TestStageEditTargetUpdatesStep(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := stageEditTarget(st, editTargetIn{Target: "g/a", Step: "30s"}); err != nil {
		t.Fatalf("stageEditTarget: %v", err)
	}
	var root struct {
		Targets struct {
			Children map[string]struct {
				Children map[string]struct {
					Step string `json:"step"`
				} `json:"children"`
			} `json:"children"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(st.working(), &root); err != nil {
		t.Fatalf("decode working doc: %v", err)
	}
	if got := root.Targets.Children["g"].Children["a"].Step; got != "30s" {
		t.Fatalf("step not updated: got %q", got)
	}
}

// TestStageEditTargetBadStepIsRejected mirrors stageAddTarget's step-parse-failure handling:
// an unparseable duration must be rejected as ErrConfigInvalid, not silently ignored or
// panicking.
func TestStageEditTargetBadStepIsRejected(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	err := stageEditTarget(st, editTargetIn{Target: "g/a", Step: "not-a-duration"})
	if err == nil {
		t.Fatal("expected an error for an unparseable step")
	}
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("expected error to wrap ErrConfigInvalid, got: %v", err)
	}
}

// TestStageMutationSurvivesAPreviouslyStagedStep is a regression test for a bug found while
// implementing the Step field above: config.Duration had a MarshalJSON (emits "30s") but no
// UnmarshalJSON, so once a target's step was staged, the doc's "step" field was a JSON
// string with no way back into the Duration (int64) field. mutateDoc parses the CURRENT
// working doc via encoding/json on every stage call (including through staging.mutate), so
// ANY staging mutation performed after a step was staged — not just another step edit —
// would fail to even parse the doc, wrapped in ErrConfigInvalid. This isn't a Step-specific
// edge case: it breaks staging entirely for a config that has any per-target step at all,
// including one seeded from a real DB config with steps already set.
func TestStageMutationSurvivesAPreviouslyStagedStep(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := stageAddTarget(st, addTargetIn{GroupPath: "G", Name: "a", Host: "1.1.1.1", Probe: "Ping", Step: "45s"}); err != nil {
		t.Fatalf("stageAddTarget with step: %v", err)
	}

	// A second, unrelated staging mutation must succeed — this is where the bug bites: the
	// working doc now carries a "step" field the JSON decoder can't turn back into a Duration.
	if err := stageAddTarget(st, addTargetIn{GroupPath: "G", Name: "b", Host: "2.2.2.2", Probe: "Ping"}); err != nil {
		t.Fatalf("second stageAddTarget failed to parse a working doc with a staged step: %v", err)
	}

	// The first target's step must have survived the round-trip.
	var root struct {
		Targets struct {
			Children map[string]struct {
				Children map[string]struct {
					Step string `json:"step"`
				} `json:"children"`
			} `json:"children"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(st.working(), &root); err != nil {
		t.Fatalf("decode working doc: %v", err)
	}
	if got := root.Targets.Children["G"].Children["a"].Step; got != "45s" {
		t.Fatalf("step did not survive the round-trip: got %q", got)
	}
	if _, ok := root.Targets.Children["G"].Children["b"]; !ok {
		t.Fatalf("second target b did not land: %+v", root.Targets.Children["G"].Children)
	}
}

// TestStageReplaceAcceptsYAML proves stageReplace parses a YAML doc (JSON is valid YAML,
// so this also covers JSON input), replaces the whole working doc, mints ids, and validates
// via setDoc -- the raw-doc escape hatch for anything the typed stage_* tools don't cover.
// The brief's example used `probe: http`, an unregistered kind that would fail setDoc's
// local validation; "Ping" is registered (internal/probe/pingprobe) and needs no params.
func TestStageReplaceAcceptsYAML(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	_ = st.ensure(context.Background(), c)
	yamlDoc := "targets:\n  children:\n    Sites:\n      children:\n        ex:\n          host: example.com\n          probe: Ping\n"
	if err := stageReplace(st, yamlDoc); err != nil {
		t.Fatalf("stageReplace: %v", err)
	}
	f, _ := flatten(st.working())
	if _, ok := f["Sites/ex"]; !ok {
		t.Fatalf("replace did not take: %v", keysOf(f))
	}
}

// TestStageEditTargetRejectsMoveIntoOwnSubtree is the regression test for a move that silently
// deleted its target: moving a node into itself or one of its descendants detached the subtree
// into an unreachable cycle, so the target vanished from the staged doc with no error. Both moves
// must be rejected and leave the working doc exactly as it was.
func TestStageEditTargetRejectsMoveIntoOwnSubtree(t *testing.T) {
	for _, dest := range []string{"g/a", "g/a/sub", "g/a/child/deeper"} {
		t.Run(dest, func(t *testing.T) {
			c, st := stagedClient(t, `{"targets":{"children":{"g":{"children":{"a":{"id":"a-id","host":"1.1.1.1","probe":"Ping","children":{"child":{"id":"c-id","host":"2.2.2.2"}}}}}}}}`)
			if err := st.ensure(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			before := string(st.working())

			err := stageEditTarget(st, editTargetIn{Target: "g/a", NewGroupPath: dest})
			if !errors.Is(err, ErrConfigInvalid) {
				t.Fatalf("move g/a into %s: err=%v, want ErrConfigInvalid", dest, err)
			}
			if got := string(st.working()); got != before {
				t.Fatalf("working doc changed after a rejected move:\nbefore: %s\nafter:  %s", before, got)
			}
		})
	}
}

// inheritedHubFile is a hub file config whose tree root sets the probe, so a DB leaf without its
// own probe inherits Ping — valid on the hub, which composes the DB fragment onto this file.
const inheritedHubFile = "targets:\n  probe: Ping\n  children:\n    file:\n      host: localhost\n"

// inheritedDB is a DB fragment holding one such probe-less leaf plus an ordinary target.
const inheritedDB = `{"targets":{"children":{"db":{"id":"db-id","host":"127.0.0.1"},"g":{"children":{"a":{"id":"a-id","host":"1.1.1.1","probe":"Ping"}}}}}}`

// TestStageAcceptsLeafInheritingFromHubFileConfig is the regression test for local validation that
// ignored the hub's file config: a DB leaf inheriting its probe from the file root is valid on the
// hub, but the MCP validated against defaults only, rejected it as "no probe set", and — since
// every mutation revalidates the whole fragment — refused every edit to any other target.
// Validation now composes onto the hub's real file config, so the edit stages cleanly while
// genuine errors (an unknown probe param, a branch colliding with a file-defined one) still fail.
func TestStageAcceptsLeafInheritingFromHubFileConfig(t *testing.T) {
	c, _ := newTestClient(t, configHub(t, inheritedHubFile, inheritedDB))
	cs := mcpSession(t, c)

	var staged struct {
		Changed  []string `json:"changed"`
		Warnings []string `json:"warnings"`
	}
	res := callTool(t, cs, "config_stage_edit_target", map[string]any{"target": "g/a", "host": "9.9.9.9"}, &staged)
	if res.IsError {
		t.Fatalf("edit beside an inherited leaf was rejected: %s", resultText(res))
	}
	if len(staged.Changed) != 1 || staged.Changed[0] != "g/a" {
		t.Errorf("changed=%v, want [g/a]", staged.Changed)
	}
	if len(staged.Warnings) != 0 {
		t.Errorf("validation against the hub's own composition should be exact, got warnings %v", staged.Warnings)
	}

	if res := callTool(t, cs, "config_stage_add_target", map[string]any{"group_path": "S", "name": "x", "host": "h", "probe": "Ping", "params": map[string]any{"bogus_param": "1"}}, nil); !res.IsError {
		t.Error("an unknown probe param staged; want a validation error")
	}
	res = callTool(t, cs, "config_stage_add_target", map[string]any{"group_path": "file", "name": "x", "host": "h", "probe": "Ping"}, nil)
	if !res.IsError || !strings.Contains(resultText(res), "duplicate top-level target") {
		t.Errorf("a DB branch colliding with the file-defined %q branch staged (or failed for another reason): %s", "file", resultText(res))
	}
}

// TestStageWithoutHubConfigContextIsAdvisory covers a hub whose effective config can't be read:
// local validation can't see the file config the fragment inherits from, so a composition error
// becomes a warning in the stage result instead of blocking the edit (config_apply's server-side
// validation stays authoritative).
func TestStageWithoutHubConfigContextIsAdvisory(t *testing.T) {
	hub := configHub(t, inheritedHubFile, inheritedDB)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("source") == "effective" {
			http.Error(w, `{"error":"effective config unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		hub.ServeHTTP(w, r)
	}))
	cs := mcpSession(t, c)

	var staged struct {
		Changed  []string `json:"changed"`
		Warnings []string `json:"warnings"`
	}
	res := callTool(t, cs, "config_stage_edit_target", map[string]any{"target": "g/a", "host": "9.9.9.9"}, &staged)
	if res.IsError {
		t.Fatalf("edit was blocked by a check that needs the hub's file config: %s", resultText(res))
	}
	if len(staged.Changed) != 1 || staged.Changed[0] != "g/a" {
		t.Errorf("changed=%v, want [g/a]", staged.Changed)
	}
	if !strings.Contains(strings.Join(staged.Warnings, "\n"), "no probe set") {
		t.Errorf("warnings=%v, want the unverifiable inherited-probe problem surfaced as a warning", staged.Warnings)
	}
}

// TestStageAdvisoryWhenStoredDBDiffersFromRunning covers a stored DB fragment the running hub has
// not loaded: `smoked config import` without -config persisted a branch named like a file-defined
// one, which the hub rejects on reload and keeps running the old fragment. The effective config
// then shows the FILE's branch under that name, so deriving the file config by removing the stored
// fragment's branches would drop the real file branch and validate strictly against a base
// missing it. Removing the conflicting DB target (the repair, which the hub accepts because its
// file target remains) must not be blocked as an empty tree.
func TestStageAdvisoryWhenStoredDBDiffersFromRunning(t *testing.T) {
	const runningDB = `{"targets":{"children":{}}}`
	const storedDB = `{"targets":{"children":{"file":{"id":"f-id","host":"10.0.0.1","probe":"Ping"}}}}`
	hub := configHub(t, inheritedHubFile, runningDB)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/admin/config" && r.URL.Query().Get("source") != "effective" {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 8, "doc": json.RawMessage(storedDB)})
			return
		}
		hub.ServeHTTP(w, r)
	}))
	cs := mcpSession(t, c)

	var staged struct {
		Removed  []string `json:"removed"`
		Warnings []string `json:"warnings"`
	}
	res := callTool(t, cs, "config_stage_remove_target", map[string]any{"target": "f-id"}, &staged)
	if res.IsError {
		t.Fatalf("removing the conflicting stored DB target was blocked: %s", resultText(res))
	}
	if len(staged.Removed) != 1 || staged.Removed[0] != "file" {
		t.Errorf("removed=%v, want [file]", staged.Removed)
	}
}
