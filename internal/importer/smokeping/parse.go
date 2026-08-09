// Package smokeping parses SmokePing's legacy `+`/`++` section-format config
// files (Targets, Probes, Alerts, ...) as a first step toward importing a
// SmokePing install into smokeping-modern. This file holds only the
// low-level tokenizer; building the target tree and CLI from these sections
// is later work.
package smokeping

import (
	"bufio"
	"strings"
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
