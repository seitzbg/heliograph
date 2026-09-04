package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/seitzbg/heliograph/internal/config"
	"github.com/seitzbg/heliograph/internal/configstore"

	// validateDoc's config.Monitors() call resolves probe kinds against the
	// internal/probe registry, which is populated by each probe package's init().
	// cmd/smoked and cmd/smoke-agent already blank-import allprobes for their own
	// collector use, but this package's local validation must not depend on that:
	// without this import, every probe kind would read as "unknown" here even though
	// the server accepts it, making config_stage_* reject every valid config.
	_ "github.com/seitzbg/heliograph/internal/probe/allprobes"
)

// staging holds a process-lifetime, in-memory working copy of the DB config fragment
// being edited by config_stage_*/config_review/config_apply/config_discard (Tasks 7–10).
// It is shared across those tools via a single instance created in NewServer.
type staging struct {
	mu      sync.Mutex
	active  bool
	baseVer int
	baseDoc json.RawMessage
	workDoc json.RawMessage
}

func newStaging() *staging { return &staging{} }

// ensure seeds the staging buffer from the live DB config on first use; a no-op once
// a staging session is already active (call reset to start over).
func (st *staging) ensure(ctx context.Context, c *Client) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.active {
		return nil
	}
	doc, ver, err := c.getConfigDoc(ctx, "db")
	if err != nil {
		return err
	}
	st.baseDoc = append(json.RawMessage(nil), doc...)
	st.workDoc = append(json.RawMessage(nil), doc...)
	st.baseVer = ver
	st.active = true
	return nil
}

func (st *staging) working() json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.workDoc
}

// isActive reports whether a staging session has been started (via ensure), guarded by
// st.mu so it is safe to call concurrently with ensure/reset from other tool dispatches.
func (st *staging) isActive() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.active
}

func (st *staging) baseVersion() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.baseVer
}

func (st *staging) reset() {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Clear fields in place rather than `*st = staging{}`: that would replace st.mu
	// itself with a fresh, unlocked mutex, so the deferred Unlock() above would then
	// fire on a mutex that was never locked and panic ("unlock of unlocked mutex").
	st.active = false
	st.baseVer = 0
	st.baseDoc = nil
	st.workDoc = nil
}

// setDoc mints ids for new host nodes, validates locally, and stores the working doc.
func (st *staging) setDoc(doc json.RawMessage) error {
	minted, _ := configstore.MintNewIDs(doc)
	if err := validateDoc(minted); err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.workDoc = minted
	return nil
}

// mutate atomically applies a tree-mutation fn to the CURRENT working doc: parse, apply,
// remarshal, mint ids for any new host nodes, validate, and only then store — all under a
// single lock cycle. The go-sdk (v1.7.0, jsonrpc2.Async) dispatches tool-call handlers
// concurrently, so stageAddTarget/stageEditTarget/stageRemoveTarget previously read
// st.workDoc via st.working() (lock, copy, unlock), mutated it unlocked, and stored the
// result via st.setDoc() (lock, store, unlock) — a read-modify-write split across two lock
// cycles that loses updates when two calls interleave (whichever setDoc runs last wins,
// silently discarding every other goroutine's change). mutate closes that window by holding
// st.mu for the whole parse->apply->marshal->mint->validate->store sequence.
//
// fn (and everything else in this method) must be pure CPU work — no network calls — since
// it runs while st.mu is held; ensure() does the one network call staging needs (seeding
// from the live DB config) separately, before any mutate.
func (st *staging) mutate(fn func(root *config.Node) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next, err := mutateDoc(st.workDoc, fn)
	if err != nil {
		return err
	}
	minted, _ := configstore.MintNewIDs(next)
	if err := validateDoc(minted); err != nil {
		return err
	}
	st.workDoc = minted
	return nil
}

// snapshotForApply returns the working doc, its base version, and whether a staging session
// is active, all read under a single lock cycle. applyStaged uses it instead of separately
// calling st.working() and st.baseVersion() (two lock cycles), which could otherwise
// interleave with a concurrent config_discard's st.reset() between the two reads and PUT a
// doc/version pair that no longer corresponds to any staged session.
func (st *staging) snapshotForApply() (doc json.RawMessage, version int, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.workDoc, st.baseVer, st.active
}

// validateDoc runs the daemon's own parser + monitor validation against a defaults-only
// base. It does NOT include the hub's file-defined targets, so the server's apply stays
// authoritative; this catches structural and probe-param errors before any PUT.
func validateDoc(doc json.RawMessage) error {
	base, err := config.Parse(nil)
	if err != nil {
		return err
	}
	if err := config.AppendDBFragment(base, doc); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	if len(base.Targets.Children) == 0 {
		// This local base is defaults-only (config.Parse(nil) above — no file-defined
		// targets from default.yaml/conf.d), so a DB fragment that removes its last
		// target leaves THIS view of the tree wholly empty. Monitors() rejects a wholly
		// empty tree as invalid — a real "zero targets anywhere" check that's a false
		// positive here: the real hub's base almost always carries file-defined targets,
		// so its merged tree won't be empty. A fragment can only ever contribute
		// targets.children (validateFragment forbids probes/alerts/tree-wide fields), so
		// an empty fragment also has nothing left for the probe-level checks above to
		// flag. Skip Monitors() and let config_apply's real PUT stay authoritative for
		// "the hub ends up with zero targets everywhere."
		return nil
	}
	if _, err := base.Monitors(); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	return nil
}

// flatten returns a map of "group/host" path -> canonical JSON of each host-bearing node.
func flatten(doc json.RawMessage) (map[string]json.RawMessage, error) {
	cfg, err := config.Parse(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	out := map[string]json.RawMessage{}
	var walk func(prefix string, n *config.Node)
	walk = func(prefix string, n *config.Node) {
		if n == nil {
			return
		}
		if n.Host != "" {
			b, _ := json.Marshal(n)
			out[prefix] = b
		}
		names := make([]string, 0, len(n.Children))
		for k := range n.Children {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child := prefix + k
			if prefix != "" {
				child = prefix + "/" + k
			}
			walk(child, n.Children[k])
		}
	}
	if cfg.Targets != nil {
		walk("", cfg.Targets)
	}
	return out, nil
}

// diffDocs reports host-node paths added, removed, and changed between two docs.
func diffDocs(base, work json.RawMessage) (added, removed, changed []string, err error) {
	fb, err := flatten(base)
	if err != nil {
		return nil, nil, nil, err
	}
	fw, err := flatten(work)
	if err != nil {
		return nil, nil, nil, err
	}
	for p, wv := range fw {
		if bv, ok := fb[p]; !ok {
			added = append(added, p)
		} else if string(bv) != string(wv) {
			changed = append(changed, p)
		}
	}
	for p := range fb {
		if _, ok := fw[p]; !ok {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed, nil
}

func (st *staging) diff() (added, removed, changed []string, err error) {
	st.mu.Lock()
	base, work := st.baseDoc, st.workDoc
	st.mu.Unlock()
	return diffDocs(base, work)
}
