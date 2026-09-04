package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func stagedClient(t *testing.T, initial string) (*Client, *staging) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/login" {
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "t", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 3, "doc": json.RawMessage(initial)})
	}))
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
