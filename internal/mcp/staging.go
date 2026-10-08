package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// errStaleStaging is returned by a staging write whose session was reset (discarded or
// applied) out from under it — e.g. a config_discard that lands, under concurrent tool
// dispatch, between a stage tool's ensure and its store.
var errStaleStaging = errors.New("staging session is no longer active (it was discarded or applied); re-stage your changes")

// staging holds a process-lifetime, in-memory working copy of the DB config fragment
// being edited by config_stage_*/config_review/config_apply/config_discard (Tasks 7–10).
// It is shared across those tools via a single instance created in NewServer.
type staging struct {
	mu      sync.Mutex
	active  bool
	baseVer int
	baseDoc json.RawMessage
	workDoc json.RawMessage
	// hubFile is the hub's file-defined config — the base the hub composes the DB fragment onto —
	// captured when the session is seeded; nil when it couldn't be derived. advisory, when
	// non-empty, says why local validation can't reproduce the hub's composition: problems found
	// by config.Monitors() are then reported as warnings instead of blocking the stage.
	hubFile  json.RawMessage
	advisory string
	// warnings are the advisory validation problems of the current workDoc.
	warnings []string
	// seq is a monotonic write-sequence for the current session: bumped whenever the
	// buffer changes (seed, every store, reset). snapshotForApply captures it so
	// applyStaged can reset ONLY if no later write landed (resetIfUnchanged), and so a
	// store into a reset session is rejected via the active flag it guards.
	seq uint64
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
	st.hubFile, st.advisory = hubFileConfig(ctx, c, doc)
	st.warnings = nil
	st.baseDoc = append(json.RawMessage(nil), doc...)
	st.workDoc = append(json.RawMessage(nil), doc...)
	st.baseVer = ver
	st.active = true
	st.seq++
	return nil
}

// hubFileConfig derives the hub's file-defined config, the base its DB fragment is composed onto.
// The hub serves no file-only source, but it does serve the composed result
// (GET /api/admin/config?source=effective), and the composition only ever adds the fragment's
// top-level branches (config.AppendDBFragment; a branch defined in both is an error), so removing
// those branches from the effective config yields the file config exactly.
//
// The returned advisory is empty when validation can be exact. It explains the gap otherwise:
// the effective config is unreadable, the running config wasn't composed from the stored DB
// config (withoutFragmentBranches), or the live DB config does not validate locally against the
// derived base (e.g. a hub newer than this binary, with probe kinds or settings it lacks).
func hubFileConfig(ctx context.Context, c *Client, dbDoc json.RawMessage) (json.RawMessage, string) {
	eff, _, err := c.getConfigDoc(ctx, "effective")
	if err != nil {
		return nil, fmt.Sprintf("the hub's effective config could not be read (%v)", err)
	}
	file, err := withoutFragmentBranches(eff, dbDoc)
	if errors.Is(err, errSnapshotMismatch) {
		return nil, err.Error()
	}
	if err != nil {
		return nil, fmt.Sprintf("the hub's effective config could not be parsed (%v)", err)
	}
	if _, err := validateDoc(file, "", dbDoc); err != nil {
		return file, fmt.Sprintf("the live DB config does not validate locally against the hub's file config (%v)", err)
	}
	return file, ""
}

// errSnapshotMismatch: the effective config (the running runtime's snapshot) doesn't carry the
// stored DB fragment's branches verbatim, so it wasn't composed from that fragment.
var errSnapshotMismatch = errors.New("the hub's running config doesn't match its stored DB config " +
	"(a reload is pending, or the hub rejected the stored fragment and kept the previous one)")

// withoutFragmentBranches returns the effective config with the DB fragment's top-level target
// branches removed. The effective config is the running snapshot while dbDoc is the stored
// fragment, which can differ (e.g. `smoked config import` persisted a branch named like a
// file-defined one; the hub rejects that on reload and keeps running the old fragment). Removing
// such a branch would delete the file's own branch from the derived base, so each stored branch
// must appear unchanged in the effective config, or errSnapshotMismatch is returned.
func withoutFragmentBranches(effective, dbDoc json.RawMessage) (json.RawMessage, error) {
	cfg, err := config.Parse(effective)
	if err != nil {
		return nil, err
	}
	frag, err := config.Parse(dbDoc)
	if err != nil {
		return nil, err
	}
	if frag.Targets != nil {
		for k, branch := range frag.Targets.Children {
			var running *config.Node
			if cfg.Targets != nil {
				running = cfg.Targets.Children[k]
			}
			want, _ := json.Marshal(branch)
			got, _ := json.Marshal(running)
			if running == nil || string(got) != string(want) {
				return nil, errSnapshotMismatch
			}
			delete(cfg.Targets.Children, k)
		}
	}
	return json.Marshal(cfg)
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
	st.clear()
}

// clear empties the session; the caller holds st.mu.
func (st *staging) clear() {
	st.active = false
	st.baseVer = 0
	st.baseDoc = nil
	st.workDoc = nil
	st.hubFile = nil
	st.advisory = ""
	st.warnings = nil
	st.seq++
}

// resetIfUnchanged clears the buffer only if it is still the same active session, with no
// write since the snapshot at wantSeq. applyStaged uses it after a successful PUT so a
// stage or discard that landed during the PUT (bumping seq) is preserved rather than wiped
// by an unconditional reset. Returns whether it reset.
func (st *staging) resetIfUnchanged(wantSeq uint64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.active || st.seq != wantSeq {
		return false
	}
	st.clear()
	return true
}

// setDoc mints ids for new host nodes, validates locally, and stores the working doc. It
// refuses (errStaleStaging) if the session was reset between the caller's ensure and here,
// so a wholesale replace (config_stage_replace) can't store into — and report as staged —
// a buffer a concurrent discard already cleared.
func (st *staging) setDoc(doc json.RawMessage) error {
	minted, _ := configstore.MintNewIDs(doc)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.active {
		return errStaleStaging
	}
	warnings, err := validateDoc(st.hubFile, st.advisory, minted)
	if err != nil {
		return err
	}
	st.workDoc = minted
	st.warnings = warnings
	st.seq++
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
	if !st.active {
		return errStaleStaging
	}
	next, err := mutateDoc(st.workDoc, fn)
	if err != nil {
		return err
	}
	minted, _ := configstore.MintNewIDs(next)
	warnings, err := validateDoc(st.hubFile, st.advisory, minted)
	if err != nil {
		return err
	}
	st.workDoc = minted
	st.warnings = warnings
	st.seq++
	return nil
}

// snapshotForApply returns the working doc, its base version, the session revision (seq), and
// whether a staging session is active, all read under a single lock cycle. applyStaged uses it
// instead of separately
// calling st.working() and st.baseVersion() (two lock cycles), which could otherwise
// interleave with a concurrent config_discard's st.reset() between the two reads and PUT a
// doc/version pair that no longer corresponds to any staged session.
func (st *staging) snapshotForApply() (doc json.RawMessage, version int, seq uint64, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.workDoc, st.baseVer, st.seq, st.active
}

// validateDoc composes doc onto hubFile (the hub's file config; nil = defaults only) with the
// daemon's own config.AppendDBFragment + Monitors(), the same path the hub's apply takes. A
// structural fragment error is always fatal. A Monitors() problem is fatal too, unless advisory
// is set — local validation then can't reproduce the hub's composition, so the problem is returned
// as a warning and config_apply's server-side validation decides.
func validateDoc(hubFile json.RawMessage, advisory string, doc json.RawMessage) ([]string, error) {
	base, err := config.Parse(hubFile)
	if err != nil {
		return nil, fmt.Errorf("%w: hub file config: %v", ErrConfigInvalid, err)
	}
	if err := config.AppendDBFragment(base, doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	if advisory != "" && len(base.Targets.Children) == 0 {
		// Without the hub's file config this view of the tree lacks its file-defined targets, so
		// a fragment that removes its last target looks wholly empty here — a problem the real
		// composition almost never has. Nothing is left to check, so don't warn about it.
		return nil, nil
	}
	if _, err := base.Monitors(); err != nil {
		if advisory == "" {
			return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
		}
		return []string{
			"local validation is advisory: " + advisory + "; config_apply's server-side validation is authoritative",
			err.Error(),
		}, nil
	}
	return nil, nil
}

// treeEntry is one node of a staged target tree, keyed by its slash path.
type treeEntry struct {
	own    string // canonical JSON of the node's own settings, children excluded
	parent string // the parent node's path; "" for a top-level node
	target bool   // host-bearing: the node is a monitored target
}

// walkTree indexes every node below the tree root by path.
func walkTree(doc json.RawMessage) (map[string]treeEntry, error) {
	cfg, err := config.Parse(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	out := map[string]treeEntry{}
	var walk func(path, parent string, n *config.Node)
	walk = func(path, parent string, n *config.Node) {
		if n == nil {
			return
		}
		if path != "" {
			own := *n
			own.Children = nil
			b, _ := json.Marshal(&own)
			out[path] = treeEntry{own: string(b), parent: parent, target: n.Host != ""}
		}
		for k, ch := range n.Children {
			child := k
			if path != "" {
				child = path + "/" + k
			}
			walk(child, path, ch)
		}
	}
	if cfg.Targets != nil {
		walk("", "", cfg.Targets)
	}
	return out, nil
}

// inheritedSettings is a target's own settings followed by those of every ancestor group, so it
// changes whenever a setting the target inherits (step, probe, params, alerts, vantages, ...)
// changes anywhere above it.
func inheritedSettings(tree map[string]treeEntry, path string) string {
	var b strings.Builder
	for p := path; ; {
		e, ok := tree[p]
		if !ok {
			break
		}
		b.WriteString(e.own)
		b.WriteByte('\n')
		if e.parent == "" {
			break
		}
		p = e.parent
	}
	return b.String()
}

// changeSet summarizes how a staged doc differs from its base. Targets are reported by path;
// Changed includes a target whose own settings are unchanged when a group above it changed,
// since that alters what it inherits. Group entries cover hostless grouping nodes, so a change
// confined to a group's own settings is never summarized as "nothing changed".
type changeSet struct {
	Added, Removed, Changed                   []string
	GroupsAdded, GroupsRemoved, GroupsChanged []string
}

func (cs changeSet) empty() bool {
	return len(cs.Added)+len(cs.Removed)+len(cs.Changed)+
		len(cs.GroupsAdded)+len(cs.GroupsRemoved)+len(cs.GroupsChanged) == 0
}

// diffDocs reports the targets and groups added, removed, and changed between two docs.
func diffDocs(base, work json.RawMessage) (changeSet, error) {
	tb, err := walkTree(base)
	if err != nil {
		return changeSet{}, err
	}
	tw, err := walkTree(work)
	if err != nil {
		return changeSet{}, err
	}
	var cs changeSet
	for p, w := range tw {
		b, ok := tb[p]
		switch {
		case w.target && (!ok || !b.target):
			cs.Added = append(cs.Added, p)
		case w.target && inheritedSettings(tb, p) != inheritedSettings(tw, p):
			cs.Changed = append(cs.Changed, p)
		case !w.target && (!ok || b.target):
			cs.GroupsAdded = append(cs.GroupsAdded, p)
		case !w.target && b.own != w.own:
			cs.GroupsChanged = append(cs.GroupsChanged, p)
		}
	}
	for p, b := range tb {
		w, ok := tw[p]
		switch {
		case b.target && (!ok || !w.target):
			cs.Removed = append(cs.Removed, p)
		case !b.target && (!ok || w.target):
			cs.GroupsRemoved = append(cs.GroupsRemoved, p)
		}
	}
	for _, l := range []*[]string{&cs.Added, &cs.Removed, &cs.Changed, &cs.GroupsAdded, &cs.GroupsRemoved, &cs.GroupsChanged} {
		sort.Strings(*l)
	}
	return cs, nil
}

// diff returns the staged change set and the current advisory validation warnings.
func (st *staging) diff() (changeSet, []string, error) {
	st.mu.Lock()
	base, work, warnings := st.baseDoc, st.workDoc, st.warnings
	st.mu.Unlock()
	cs, err := diffDocs(base, work)
	return cs, warnings, err
}
