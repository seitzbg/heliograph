package configstore_test

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/seitzbg/heliograph/internal/configstore"
)

// nodeID walks doc's targets.children tree by the given key path (each element is one
// node key, mirroring config.Monitors()'s path segments) and returns that node's "id"
// field ("" if absent). Same tree-walk as migrate_test.go's assertNodeID, but returns
// the value instead of asserting it, since this package's tests need to inspect a
// nondeterministic (UUID) id rather than compare it to a fixed expectation.
func nodeID(t *testing.T, doc json.RawMessage, path []string) string {
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
	id, _ := node["id"].(string)
	return id
}

// uuidV4 matches the exact shape newUUID() produces: 8-4-4-4-12 hex, version nibble 4,
// variant nibble 8/9/a/b.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestConfigApplyMintsUUIDForNewNode(t *testing.T) {
	doc := json.RawMessage(`{"targets":{"children":{"a":{"host":"h"}}}}`)
	out, changed := configstore.MintNewIDs(doc)
	if !changed {
		t.Fatal("expected a minted id")
	}
	id := nodeID(t, out, []string{"a"})
	if id == "" || id == "a" {
		t.Fatalf("new node must get an opaque UUID, not the path; got %q", id)
	}
	if !uuidV4.MatchString(id) {
		t.Fatalf("minted id %q does not look like a v4 UUID", id)
	}
	// Idempotent once present.
	if _, changed2 := configstore.MintNewIDs(out); changed2 {
		t.Fatal("existing id must be left alone")
	}
	if got := nodeID(t, out, []string{"a"}); got != id {
		t.Fatalf("second MintNewIDs pass must not change an existing id: got %q, want %q", got, id)
	}
}

// Two nodes minted in the same pass must not collide (crypto/rand, not a
// path-derived value that two id-less nodes could ever share).
func TestMintNewIDsGivesDistinctIDs(t *testing.T) {
	doc := json.RawMessage(`{"targets":{"children":{"a":{"host":"h1"},"b":{"host":"h2"}}}}`)
	out, changed := configstore.MintNewIDs(doc)
	if !changed {
		t.Fatal("expected minted ids")
	}
	idA := nodeID(t, out, []string{"a"})
	idB := nodeID(t, out, []string{"b"})
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("expected distinct minted ids, got %q and %q", idA, idB)
	}
}

// A grouping node (no host) never gets an id, minted or otherwise.
func TestMintNewIDsSkipsGroupingNodes(t *testing.T) {
	doc := json.RawMessage(`{"targets":{"children":{"g":{"children":{"a":{"host":"h"}}}}}}`)
	out, changed := configstore.MintNewIDs(doc)
	if !changed {
		t.Fatal("expected the host-bearing leaf to get a minted id")
	}
	if got := nodeID(t, out, []string{"g"}); got != "" {
		t.Fatalf("grouping node must not get an id, got %q", got)
	}
	if got := nodeID(t, out, []string{"g", "a"}); got == "" {
		t.Fatal("nested host-bearing node must get a minted id")
	}
}
