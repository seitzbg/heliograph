// Package federation builds the per-vantage work assignments the hub serves to remote
// agents (and, later, uses to scope its own probing). Pure functions over the built
// monitor set — no I/O, no config parsing.
package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strconv"
	"strings"

	"smokeping-modern/internal/model"
)

// AssignmentFor returns the monitors that vantage v is responsible for probing: every
// monitor whose effective vantage list contains v, in the input order (config.Monitors
// already returns them name-sorted). This is what the hub serves an agent identifying as
// v — and, once wired, what the hub itself probes for v = its own vantage.
func AssignmentFor(monitors []model.Monitor, v string) []model.Monitor {
	var out []model.Monitor
	for _, m := range monitors {
		if slices.Contains(m.Vantages, v) {
			out = append(out, m)
		}
	}
	return out
}

// ConfigVersion is a stable content hash of an assignment — the fields an agent acts on
// (name, probe, host, params, step, pings), canonically encoded so it is independent of
// slice order and Params map iteration order. The agent sends the version it holds; the
// hub answers 304 Not Modified when it is unchanged. Format: "sha256:<hex>".
func ConfigVersion(assignment []model.Monitor) string {
	ms := make([]model.Monitor, len(assignment))
	copy(ms, assignment)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(m.Name)
		b.WriteByte(0x1f)
		b.WriteString(m.ProbeKind)
		b.WriteByte(0x1f)
		b.WriteString(m.Host)
		b.WriteByte(0x1f)
		b.WriteString(strconv.Itoa(m.Pings))
		b.WriteByte(0x1f)
		b.WriteString(strconv.FormatInt(int64(m.Step), 10))
		b.WriteByte(0x1f)
		keys := make([]string, 0, len(m.Params))
		for k := range m.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(m.Params[k])
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}
