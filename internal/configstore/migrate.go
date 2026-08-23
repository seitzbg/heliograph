package configstore

import (
	"context"
	"encoding/json"
)

// stampIDs walks a DB config fragment's targets tree and, for every host-bearing node
// lacking a non-empty id, sets id = mint(path). path is computed exactly as
// config.(*Config).Monitors() computes it: a top-level child key is bare, and each
// level below appends "/<key>" to its parent's path. A node with no host (a grouping
// node) never gets an id. A node that already carries a non-empty id is left alone.
//
// The doc is decoded into map[string]any (not a typed struct) so unknown/future fields
// round-trip unchanged. Returns the re-marshaled doc and whether anything changed; when
// nothing changed the original doc is returned as-is.
//
// This is the reusable walker: Task 8's migration (MigrateStampIDs) passes an identity
// mint (id = path — the birth-path id under which the store already keys existing prod
// history, so nothing needs re-keying). Task 9's MintNewIDs is expected to reuse this
// same walker with a UUID-minting mint to stamp brand-new nodes.
func stampIDs(doc json.RawMessage, mint func(path string) string) (json.RawMessage, bool) {
	if len(doc) == 0 {
		return doc, false
	}
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return doc, false
	}
	targets, _ := root["targets"].(map[string]any)
	if targets == nil {
		return doc, false
	}
	children, _ := targets["children"].(map[string]any)
	if children == nil {
		return doc, false
	}

	changed := false
	var walk func(path string, children map[string]any)
	walk = func(path string, children map[string]any) {
		for key, v := range children {
			node, ok := v.(map[string]any)
			if !ok {
				continue
			}
			childPath := key
			if path != "" {
				childPath = path + "/" + key
			}
			host, _ := node["host"].(string)
			id, _ := node["id"].(string)
			if host != "" && id == "" {
				node["id"] = mint(childPath)
				changed = true
			}
			if grandchildren, ok := node["children"].(map[string]any); ok {
				walk(childPath, grandchildren)
			}
		}
	}
	walk("", children)

	if !changed {
		return doc, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return doc, false
	}
	return out, true
}

// StampIDsForTest exposes stampIDs with a birth-path (identity) minter, for tests.
func StampIDsForTest(doc json.RawMessage) (json.RawMessage, bool) {
	return stampIDs(doc, func(path string) string { return path })
}

// MigrateStampIDs is the one-time startup migration: it stamps id = <flattened path>
// on every existing host-bearing DB-config node that lacks one. Because the id equals
// the string the store already keys that target's history under (id == path, before
// this migration ran), no existing row needs re-keying and no continuous-aggregate
// surgery is required — this is what lets a stable target id ship without a data
// migration for prod history.
//
// Idempotent: once a node has an id (from this migration, or an explicit config edit),
// re-running is a no-op for it — so a second smoked instance racing this at startup, or
// a restart after a successful migration, does nothing. If another instance already
// migrated and updated the version first, Set returns ErrConflict; the caller is
// expected to treat that as benign (log and continue) rather than fatal.
func MigrateStampIDs(ctx context.Context, s *Store) (bool, error) {
	doc, version, err := s.Get(ctx)
	if err != nil || len(doc) == 0 {
		return false, err
	}
	out, changed := StampIDsForTest(doc)
	if !changed {
		return false, nil
	}
	if err := s.Set(ctx, out, version); err != nil {
		return false, err
	}
	return true, nil
}
