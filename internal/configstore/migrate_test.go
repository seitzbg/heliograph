package configstore

import (
	"encoding/json"
	"testing"
)

// assertNodeID walks doc's targets.children tree by the given key path (each element
// is one node key, mirroring config.Monitors()'s path segments) and checks the node's
// "id" field equals want.
func assertNodeID(t *testing.T, doc json.RawMessage, path []string, want string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, ok := root["targets"].(map[string]any)
	if !ok {
		t.Fatalf("no targets in %s", doc)
	}
	for i, key := range path {
		children, ok := node["children"].(map[string]any)
		if !ok {
			t.Fatalf("no children at path %v[:%d] in %s", path, i, doc)
		}
		node, ok = children[key].(map[string]any)
		if !ok {
			t.Fatalf("missing node %q at path %v[:%d] in %s", key, path, i, doc)
		}
	}
	got, _ := node["id"].(string)
	if got != want {
		t.Fatalf("node %v: id = %q, want %q", path, got, want)
	}
}

func TestMigrateStampsBirthPathIDsIdempotently(t *testing.T) {
	doc := json.RawMessage(`{"targets":{"children":{
		"Resolvers":{"children":{
			"dns1":{"host":"192.168.1.5"},
			"dns2":{"host":"192.168.1.6","id":"already"}
		}}}}}`)
	out, changed := StampIDsForTest(doc) // pure function under test
	if !changed {
		t.Fatal("expected changes")
	}
	// dns1 gets id = its path; dns2 keeps its explicit id.
	assertNodeID(t, out, []string{"Resolvers", "dns1"}, "Resolvers/dns1")
	assertNodeID(t, out, []string{"Resolvers", "dns2"}, "already")
	// Idempotent: a second pass changes nothing.
	if _, changed2 := StampIDsForTest(out); changed2 {
		t.Fatal("second pass must be a no-op")
	}
}
