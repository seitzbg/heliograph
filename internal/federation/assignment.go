// Package federation builds the per-vantage work assignments the hub serves to remote
// agents (and, later, uses to scope its own probing). Pure functions over the built
// monitor set — no I/O, no config parsing.
package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

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
// (name, probe, host, params, step, pings). It encodes them as canonical JSON (monitors
// sorted by name; Params flattened to a name-sorted [k,v] list) and hashes that. JSON
// string-escaping makes the encoding unambiguous, so two distinct configs can never
// collide the way a bare-delimiter join would (e.g. Params{"a":"b=c"} vs {"a=b":"c"}).
// The agent sends the version it holds; the hub answers 304 Not Modified when unchanged.
// Format: "sha256:<hex>".
func ConfigVersion(assignment []model.Monitor) string {
	type kv struct{ K, V string }
	type entry struct {
		Name, Probe, Host string
		Pings             int
		StepNs            int64
		Params            []kv
	}
	entries := make([]entry, 0, len(assignment))
	for _, m := range assignment {
		keys := make([]string, 0, len(m.Params))
		for k := range m.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		params := make([]kv, len(keys))
		for i, k := range keys {
			params[i] = kv{K: k, V: m.Params[k]}
		}
		entries = append(entries, entry{
			Name: m.Name, Probe: m.ProbeKind, Host: m.Host,
			Pings: m.Pings, StepNs: int64(m.Step), Params: params,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	buf, _ := json.Marshal(entries) // marshaling a fixed struct of strings/ints cannot error
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}
