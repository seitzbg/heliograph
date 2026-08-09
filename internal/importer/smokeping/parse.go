// Package smokeping parses SmokePing's legacy `+`/`++` section-format config
// files (Targets, Probes, Alerts, ...) as a first step toward importing a
// SmokePing install into smokeping-modern. This file holds only the
// low-level tokenizer; building the target tree and CLI from these sections
// is later work.
package smokeping

import (
	"bufio"
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

// Summary reports what buildTree produced: how many targets and folders made
// it into the tree, a per-modern-probe target count, and anything skipped.
type Summary struct {
	Targets int
	Folders int
	ByProbe map[string]int // modern probe kind -> target count
	Skipped []SkipNote
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

// buildTree turns parsed Targets sections into a *config.Node tree (root is
// the depth-0 body). A section with a `host` field is a target: Probe is set
// from the mapped, inherited/overridden SmokePing probe and Host is set from
// `host`. A section without `host` is a folder. `probe` inherits down the
// tree and may be overridden per section. Presentation keys (menu, title,
// remark, ...) are dropped — Node.Title is never set here. A target whose
// resolved SmokePing probe has no probeMap entry is skipped (recorded in
// Summary.Skipped, never emitted into the tree), and any folder left with no
// targets underneath, directly or nested, is pruned.
func buildTree(secs []Section) (*config.Node, Summary) {
	sum := Summary{ByProbe: map[string]int{}}
	root := &config.Node{Children: map[string]*config.Node{}}
	// stack[d] holds the node whose section Depth is d; a section at depth D
	// attaches under stack[D-1]. names and spProbe run in lockstep with stack:
	// names[d] is that depth's section name (used only to rebuild SkipNote.Path
	// via pathOf), spProbe[d] is the SmokePing probe inherited at that depth.
	stack := []*config.Node{root}
	names := []string{""}
	spProbe := []string{""}
	if len(secs) > 0 && secs[0].Depth == 0 {
		spProbe[0] = secs[0].Fields["probe"] // top-level default probe
	}
	for _, s := range secs {
		if s.Depth == 0 {
			continue // root body already consumed above for the default probe
		}
		for len(stack) > s.Depth {
			stack = stack[:len(stack)-1]
			names = names[:len(names)-1]
			spProbe = spProbe[:len(spProbe)-1]
		}
		parent := stack[len(stack)-1]
		sp := spProbe[len(spProbe)-1]
		if p, ok := s.Fields["probe"]; ok {
			sp = p
		}

		host, isTarget := s.Fields["host"]
		var node *config.Node
		if isTarget {
			modern, ok := probeMap[sp]
			if !ok {
				sum.Skipped = append(sum.Skipped, SkipNote{Path: pathOf(names, s.Name), Probe: sp, Reason: "unmapped probe"})
				// Unlinked placeholder: keeps stack/names/spProbe in sync so any
				// (malformed) deeper sections still have a frame to pop, but it
				// never reaches parent.Children, so it can't leak into the tree.
				node = &config.Node{Children: map[string]*config.Node{}}
			} else {
				node = &config.Node{Probe: modern, Host: host}
				sum.Targets++
				sum.ByProbe[modern]++
				if parent.Children == nil {
					parent.Children = map[string]*config.Node{}
				}
				parent.Children[s.Name] = node
			}
		} else {
			node = &config.Node{Children: map[string]*config.Node{}}
			sum.Folders++
			if parent.Children == nil {
				parent.Children = map[string]*config.Node{}
			}
			parent.Children[s.Name] = node
		}
		stack = append(stack, node)
		names = append(names, s.Name)
		spProbe = append(spProbe, sp)
	}
	pruneEmptyFolders(root)
	return root, sum
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

// pruneEmptyFolders recursively deletes folder nodes (Children != nil) that
// end up with no children once their own descendants are pruned — e.g. a
// folder whose only targets were all skipped for an unmapped probe. Target
// nodes (Children == nil) are left alone.
func pruneEmptyFolders(n *config.Node) {
	for name, c := range n.Children {
		if c.Children == nil {
			continue // target node, nothing to prune
		}
		pruneEmptyFolders(c)
		if len(c.Children) == 0 {
			delete(n.Children, name)
		}
	}
}
