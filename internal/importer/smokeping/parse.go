// Package smokeping parses SmokePing's legacy `+`/`++` section-format config
// files (Targets, Probes, Alerts, ...) as a first step toward importing a
// SmokePing install into smokeping-modern. This file holds only the
// low-level tokenizer; building the target tree and CLI from these sections
// is later work.
package smokeping

import (
	"bufio"
	"sort"
	"strconv"
	"strings"

	"smokeping-modern/internal/config"
)

// Section is one `+`-delimited block of a SmokePing config file (or, for
// Depth 0, the fields that appear before the first such block).
type Section struct {
	Depth  int               // number of leading '+' (0 = the file's top-level/root body)
	Name   string            // token after the '+'s ("" for the depth-0 root body)
	Fields map[string]string // key = value lines belonging to this section
	Line   int               // 1-based line of the section header (for messages)
}

// parseSections splits SmokePing config text into sections in document order.
// It drops the `*** Header ***` line, skips blank lines and `#` comments, and joins
// continuation lines (a line ending in '\' continues onto the next). Fields before the first
// '+' section belong to the depth-0 root Section (Name "").
func parseSections(text string) ([]Section, error) {
	root := Section{Depth: 0, Name: "", Fields: map[string]string{}}
	var subs []Section
	curIdx := -1 // -1 = the root body; >=0 = index into subs
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	var pending string // accumulates a continued line
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		if pending != "" {
			raw = pending + strings.TrimLeft(raw, " \t")
			pending = ""
		}
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "***") {
			continue
		}
		if strings.HasSuffix(t, "\\") { // line continuation
			pending = strings.TrimSpace(strings.TrimSuffix(t, "\\")) + " "
			continue
		}
		if strings.HasPrefix(t, "+") {
			d := 0
			for d < len(t) && t[d] == '+' {
				d++
			}
			subs = append(subs, Section{Depth: d, Name: strings.TrimSpace(t[d:]), Fields: map[string]string{}, Line: lineNo})
			curIdx = len(subs) - 1
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok {
			key, val := strings.TrimSpace(k), strings.TrimSpace(v)
			if curIdx < 0 {
				root.Fields[key] = val
			} else {
				subs[curIdx].Fields[key] = val
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]Section, 0, len(subs)+1)
	out = append(out, root) // depth-0 root body always first, then sections in document order
	return append(out, subs...), nil
}

// SkipNote records a Targets section that buildTree could not turn into a
// monitor because its (inherited or overridden) SmokePing probe has no
// mapped modern equivalent.
type SkipNote struct {
	Path, Probe, Reason string
}

// Summary reports what buildTree/Parse produced: how many targets and
// folders made it into the tree, a per-modern-probe target count, anything
// skipped, the SmokePing params that had no home in the modern probe's param
// set, and the Database file's advisory step/pings (never emitted into the
// tree — see Parse).
type Summary struct {
	Targets       int
	Folders       int
	ByProbe       map[string]int // modern probe kind -> target count
	Skipped       []SkipNote
	DroppedParams []string // SmokePing param names with no paramMap entry for their modern probe (deduped, sorted)
	Step          string   // Database file's `step`, advisory only
	Pings         int      // Database file's `pings`, advisory only
}

// probeMap translates a SmokePing probe name to its modern equivalent.
// SmokePing probes with no entry here (speedtest, speedtestcli, or anything
// unrecognized) have no modern counterpart, so targets using them are skipped.
var probeMap = map[string]string{
	"FPing":   "FPing",
	"FPing6":  "FPing",
	"DNS":     "DNS",
	"TCPPing": "TCPConnect",
}

// smokeTargetInfo carries, for one target node buildTree created, the
// SmokePing-side data Parse needs later to project probe-level params onto
// it: the resolved (inherited/overridden) SmokePing probe name, and the
// target section's own inline fields — so an inline override (e.g. a
// per-target `port`) can be told apart from the probe's file-level default.
type smokeTargetInfo struct {
	probe  string
	fields map[string]string
}

// buildTree turns parsed Targets sections into a *config.Node tree (root is
// the depth-0 body). A section with a `host` field is a target: Probe is set
// from the mapped, inherited/overridden SmokePing probe and Host is set from
// `host`. A section without `host` is a folder. `probe` inherits down the
// tree and may be overridden per section. Presentation keys (menu, title,
// remark, ...) are dropped — Node.Title is never set here. A target whose
// resolved SmokePing probe has no probeMap entry is skipped (recorded in
// Summary.Skipped) but still contributes a linked, host-less placeholder node
// so that any mappable descendants nested under it survive. A section whose
// name duplicates an existing sibling is also skipped (first one wins). Any
// node left with no Host and no children, directly or nested, is pruned —
// but a node with a Host is never pruned, even if its own children end up
// empty: `*config.Node` legally carries both (config.Monitors emits the
// monitor AND recurses into children), so "target" and "folder" are not
// mutually exclusive and pruning must key off Host, not Children.
//
// The third return value maps each surviving target node back to the
// SmokePing-side data (resolved probe name + the target section's own
// inline fields) that Parse needs to project probe-level params onto it.
func buildTree(secs []Section) (*config.Node, Summary, map[*config.Node]smokeTargetInfo) {
	sum := Summary{ByProbe: map[string]int{}}
	root := &config.Node{Children: map[string]*config.Node{}}
	info := map[*config.Node]smokeTargetInfo{}
	// stack[i]/names[i]/depths[i]/spProbe[i] are one frame, pushed and popped in
	// lockstep: stack[i] is the node, names[i] its section name (used only to
	// rebuild SkipNote.Path via pathOf), depths[i] its Section.Depth (root is a
	// synthetic depth-0 frame), spProbe[i] the SmokePing probe inherited at that
	// depth. depths — not len(stack) — decides how many frames a section pops,
	// so a section that jumps more than one level deeper than its predecessor
	// (e.g. "+" then "+++" with no "++") still nests under the right ancestor
	// instead of under whatever the previous section happened to be.
	stack := []*config.Node{root}
	names := []string{""}
	depths := []int{0}
	spProbe := []string{""}
	if len(secs) > 0 && secs[0].Depth == 0 {
		spProbe[0] = secs[0].Fields["probe"] // top-level default probe
	}
	for _, s := range secs {
		if s.Depth == 0 {
			continue // root body already consumed above for the default probe
		}
		for depths[len(depths)-1] >= s.Depth {
			stack = stack[:len(stack)-1]
			names = names[:len(names)-1]
			depths = depths[:len(depths)-1]
			spProbe = spProbe[:len(spProbe)-1]
		}
		parent := stack[len(stack)-1]
		sp := spProbe[len(spProbe)-1]
		if p, ok := s.Fields["probe"]; ok {
			sp = p
		}

		_, dup := parent.Children[s.Name]
		host, isTarget := s.Fields["host"]
		var node *config.Node
		switch {
		case dup:
			sum.Skipped = append(sum.Skipped, SkipNote{Path: pathOf(names, s.Name), Probe: sp, Reason: "duplicate name"})
			// Unlinked placeholder: keeps the stack in sync for any deeper
			// sections nested under the duplicate, but the duplicate (and
			// anything under it) never reaches the tree — the first section
			// with this name wins, matching parent.Children's single slot.
			node = &config.Node{Children: map[string]*config.Node{}}
		case isTarget:
			modern, ok := probeMap[sp]
			if ok {
				node = &config.Node{Probe: modern, Host: host}
				sum.Targets++
				sum.ByProbe[modern]++
				info[node] = smokeTargetInfo{probe: sp, fields: s.Fields}
			} else {
				sum.Skipped = append(sum.Skipped, SkipNote{Path: pathOf(names, s.Name), Probe: sp, Reason: "unmapped probe"})
				// Linked, host-less placeholder: the skipped target itself is
				// dropped, but a section with a `host` may still (legally, if
				// unusually) have nested subsections — link this in so any
				// mappable descendant survives and nests correctly. Pruned
				// below if nothing rescues it.
				node = &config.Node{Children: map[string]*config.Node{}}
			}
			linkChild(parent, s.Name, node)
		default:
			node = &config.Node{Children: map[string]*config.Node{}}
			sum.Folders++
			linkChild(parent, s.Name, node)
		}
		stack = append(stack, node)
		names = append(names, s.Name)
		depths = append(depths, s.Depth)
		spProbe = append(spProbe, sp)
	}
	pruneEmptyFolders(root)
	return root, sum, info
}

// linkChild attaches node under parent as name, initializing parent.Children
// first if this is parent's first child (a target node starts with a nil
// Children map — see config.Node).
func linkChild(parent *config.Node, name string, node *config.Node) {
	if parent.Children == nil {
		parent.Children = map[string]*config.Node{}
	}
	parent.Children[name] = node
}

// pathOf rebuilds the slash-joined path for a SkipNote from the names on the
// stack (skipping the root's empty name) plus the section's own name.
func pathOf(names []string, name string) string {
	parts := make([]string, 0, len(names)+1)
	for _, n := range names {
		if n != "" {
			parts = append(parts, n)
		}
	}
	parts = append(parts, name)
	return strings.Join(parts, "/")
}

// pruneEmptyFolders recursively deletes nodes that end up with neither a Host
// nor any children once their own descendants are pruned first — e.g. a
// folder whose only targets were all skipped, or a skipped-target placeholder
// that nothing mappable was nested under. A node with a Host is never pruned,
// even if it ends up with zero children: config.Node legally carries both a
// Host and Children (config.Monitors emits the monitor and recurses), so a
// hosted node with pruned-empty Children is still a real target, not an
// empty folder.
func pruneEmptyFolders(n *config.Node) {
	for name, c := range n.Children {
		pruneEmptyFolders(c)
		if c.Host == "" && len(c.Children) == 0 {
			delete(n.Children, name)
		}
	}
}

// paramMap translates a SmokePing probe param name to its modern target
// param name, per modern probe kind (the map key). Only params a target
// itself can meaningfully carry are listed here — probe-wide settings (e.g.
// FPing's `binary`, DNS's `pings`) are execution config for the SmokePing
// probe process, not something a modern target can express, so FPing has no
// entries at all. A SmokePing param with no entry for the target's modern
// probe is dropped (recorded on Summary.DroppedParams) rather than silently
// ignored.
var paramMap = map[string]map[string]string{
	"DNS":        {"lookup": "lookup", "recordtype": "recordtype", "port": "port"},
	"TCPConnect": {"port": "port"},
	"FPing":      {}, // no target-scoped params carried
}

// Parse reads the three SmokePing config bodies (Targets, Probes, Database)
// and returns the modern target tree plus a Summary. It builds the tree
// (buildTree), then projects target-scoped params carried by each target's
// SmokePing probe onto the modern target — see paramMap and projectParams —
// and parses Database only to advise (Summary.Step / Summary.Pings); its
// values are never emitted into the tree, since a modern install's step and
// ping-count are process-wide settings, not something an import should
// silently override.
func Parse(targetsText, probesText, databaseText string) (*config.Node, Summary, error) {
	tsecs, err := parseSections(targetsText)
	if err != nil {
		return nil, Summary{}, err
	}
	root, sum, info := buildTree(tsecs)

	// Index the Probes file's per-probe fields by SmokePing probe name.
	probeParams := map[string]map[string]string{}
	if psecs, err := parseSections(probesText); err == nil {
		for _, s := range psecs {
			if s.Depth >= 1 {
				probeParams[s.Name] = s.Fields
			}
		}
	}
	projectParams(info, probeParams, &sum)
	sort.Strings(sum.DroppedParams)

	// Database is advisory only (never emitted into the tree).
	if dsecs, err := parseSections(databaseText); err == nil && len(dsecs) > 0 {
		sum.Step = dsecs[0].Fields["step"]
		if p := dsecs[0].Fields["pings"]; p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				sum.Pings = n
			}
		}
	}
	return root, sum, nil
}

// projectParams stamps each target node's params from paramMap[node.Probe]:
// for every field on the target's SmokePing probe (from the Probes file,
// keyed by the SmokePing probe name in smokeTargetInfo.probe — e.g. a
// TCPPing target's fields come from the Probes file's TCPPing section) plus
// the target's own inline fields (which override the probe's file-level
// default for the same key), a key present in paramMap[node.Probe] is copied
// onto node.Params under its modern name; a non-empty key absent from
// paramMap[node.Probe] is recorded on sum.DroppedParams instead (`host` and
// `probe` are never candidates either way — they are structural and already
// consumed as node.Host / node.Probe). Empty values are skipped either way.
func projectParams(info map[*config.Node]smokeTargetInfo, probeParams map[string]map[string]string, sum *Summary) {
	dropped := map[string]bool{}
	for node, ti := range info {
		accept, ok := paramMap[node.Probe]
		if !ok {
			continue
		}
		merged := map[string]string{}
		for k, v := range probeParams[ti.probe] {
			merged[k] = v
		}
		for k, v := range ti.fields {
			if k == "host" || k == "probe" {
				continue
			}
			merged[k] = v // inline overrides the probe's file-level default
		}
		for k, v := range merged {
			if v == "" {
				continue
			}
			if modernKey, ok := accept[k]; ok {
				if node.Params == nil {
					node.Params = map[string]string{}
				}
				node.Params[modernKey] = v
			} else if !dropped[k] {
				dropped[k] = true
				sum.DroppedParams = append(sum.DroppedParams, k)
			}
		}
	}
}
