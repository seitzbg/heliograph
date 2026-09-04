package mcp

import (
	"context"
	"encoding/json"
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
