package mcp

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func effectiveRecentLoss(t Target) float64 {
	if t.RecentLossPct != nil {
		return *t.RecentLossPct
	}
	return t.LossPct
}

func classify(t Target) string {
	if t.NoData {
		return "no_data"
	}
	loss := effectiveRecentLoss(t)
	if t.Error != "" || loss >= 99 {
		return "down"
	}
	if loss >= 2 {
		return "degraded"
	}
	return "healthy"
}

type Problem struct {
	Target   string   `json:"target"`
	Scope    string   `json:"scope"`  // "global" | "vantage-specific"
	Status   string   `json:"status"` // worst status across affected vantages
	Vantages []string `json:"vantages"`
}

// analyzeTriage groups per-vantage status rows by target and splits global from
// vantage-specific problems. byVantage maps vantage name -> its /api/targets rows.
func analyzeTriage(byVantage map[string][]Target) []Problem {
	type agg struct {
		name    string
		bad     []string // vantages where unhealthy
		healthy int      // vantages where healthy
		noData  int      // vantages that returned no data for this target
		worst   string
	}
	rank := map[string]int{"down": 3, "degraded": 2, "no_data": 1, "healthy": 0}
	m := map[string]*agg{}
	for v, rows := range byVantage {
		for _, t := range rows {
			a := m[t.ID]
			if a == nil {
				a = &agg{name: displayNameOf(t)}
				m[t.ID] = a
			}
			st := classify(t)
			if st == "healthy" || st == "no_data" {
				if st == "healthy" {
					a.healthy++
				} else {
					a.noData++
				}
				continue
			}
			a.bad = append(a.bad, v)
			if rank[st] > rank[a.worst] {
				a.worst = st
			}
		}
	}
	var out []Problem
	for _, a := range m {
		if len(a.bad) == 0 {
			continue
		}
		// global = every vantage that returned a reading saw it bad. A healthy reading
		// means a path/ISP difference (vantage-specific); a no-data reading means we can't
		// confirm the target is bad there, so it also downgrades to vantage-specific rather
		// than overstating a "global" (target-wide) fault.
		scope := "vantage-specific"
		if a.healthy == 0 && a.noData == 0 {
			scope = "global"
		}
		sort.Strings(a.bad)
		out = append(out, Problem{Target: a.name, Scope: scope, Status: a.worst, Vantages: a.bad})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] > rank[out[j].Status]
		}
		if (out[i].Scope == "global") != (out[j].Scope == "global") {
			return out[i].Scope == "global"
		}
		return out[i].Target < out[j].Target
	})
	return out
}

func displayNameOf(t Target) string {
	if t.Name != "" {
		return t.Name
	}
	return t.ID
}

// localVantage is the hub's built-in vantage — the one an unfiltered /api/targets read reports,
// and the implicit vantage of a target with no vantage set. It is never in the remote registry.
const localVantage = "local"

// measuredFrom reports whether a target row is measured from vantage v. A row without a vantage
// set is measured only from the hub itself (the config default; a hub without a database does not
// publish vantage sets), mirroring the API's own scoping of the Overview boards.
func measuredFrom(t Target, v string) bool {
	if len(t.Vantages) == 0 {
		return v == localVantage
	}
	return slices.Contains(t.Vantages, v)
}

// measuringVantages returns, sorted, every vantage that measures at least one of rows.
func measuringVantages(rows []Target) []string {
	set := map[string]bool{}
	for _, t := range rows {
		if len(t.Vantages) == 0 {
			set[localVantage] = true
		}
		for _, v := range t.Vantages {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// triageVantageNames resolves which vantages to read, from the targets' own vantage sets in the
// public /api/targets catalog (rows) — which covers the hub's "local" vantage and every remote one
// that measures something — plus the optional remote registry (reg) for an explicit filter. An
// empty filter selects every measuring vantage (just "local" when the catalog carries no vantage
// sets or no targets). A filter naming no known vantage is an error rather than a silent widening.
func triageVantageNames(rows []Target, reg []Vantage, filter string) ([]string, error) {
	measuring := measuringVantages(rows)
	if filter == "" {
		if len(measuring) == 0 {
			return []string{localVantage}, nil
		}
		return measuring, nil
	}
	known := map[string]bool{localVantage: true}
	for _, v := range measuring {
		known[v] = true
	}
	for _, v := range reg {
		known[v.Name] = true
	}
	if !known[filter] {
		names := make([]string, 0, len(known))
		for v := range known {
			names = append(names, v)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown vantage %q (known: %s)", filter, strings.Join(names, ", "))
	}
	return []string{filter}, nil
}

type triageIn struct {
	Vantage string `json:"vantage,omitempty" jsonschema:"restrict triage to a single vantage, e.g. local (default: every vantage that measures a target)"`
}
type triageOut struct {
	Problems          []Problem `json:"problems"`
	StaleVants        []string  `json:"stale_vantages"`
	Healthy           int       `json:"healthy_targets"`
	VantagesChecked   []string  `json:"vantages_checked"`
	RegistryAvailable bool      `json:"registry_available"`
}

func registerTriage(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_triage",
		Description: "Fast network health triage: classifies every target from each vantage that measures it — the hub's own \"local\" vantage and any remote ones — as healthy/degraded/down/no-data, separates GLOBAL problems (bad from every measuring vantage that returned a reading → target issue) from VANTAGE-SPECIFIC ones (bad from some vantages but healthy or no-data from others → path/ISP issue), and flags stale remote collectors when the hub has a vantage registry. Start here for an open-ended 'what's wrong?' investigation.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in triageIn) (*sdk.CallToolResult, triageOut, error) {
		// The unfiltered read is the local vantage's view, and its rows carry each target's
		// vantage set — the catalog the vantages to read are derived from.
		localRows, err := fetchStatus(ctx, c, "")
		if err != nil {
			return nil, triageOut{}, err
		}
		// The registry only adds stale-collector detection; a hub without it (404) still triages.
		reg, _, regOK, err := fetchVantages(ctx, c)
		if err != nil {
			return nil, triageOut{}, err
		}
		names, err := triageVantageNames(localRows, reg, in.Vantage)
		if err != nil {
			return nil, triageOut{}, err
		}
		byV := map[string][]Target{}
		for _, n := range names {
			rows := localRows
			if n != localVantage {
				if rows, err = fetchStatus(ctx, c, n); err != nil {
					return nil, triageOut{}, err
				}
			}
			// Every /api/targets read lists the whole catalog; a target a vantage doesn't measure
			// shows there as no-data, which would wrongly downgrade a global problem.
			var measured []Target
			for _, t := range rows {
				if measuredFrom(t, n) {
					measured = append(measured, t)
				}
			}
			byV[n] = measured
		}
		probs := analyzeTriage(byV)
		stale := staleVantages(reg)
		healthy := countHealthy(byV)
		var b strings.Builder
		fmt.Fprintf(&b, "%d problem target(s), %d healthy, from vantage(s) %s", len(probs), healthy, strings.Join(names, ","))
		if regOK {
			fmt.Fprintf(&b, "; %d stale vantage(s)\n", len(stale))
		} else {
			b.WriteString("; stale-collector check skipped (this hub has no remote-vantage registry)\n")
		}
		for _, p := range probs {
			fmt.Fprintf(&b, "- [%s/%s] %s (vantages: %s)\n", p.Status, p.Scope, p.Target, strings.Join(p.Vantages, ","))
		}
		if len(stale) > 0 {
			fmt.Fprintf(&b, "stale collectors: %s\n", strings.Join(stale, ","))
		}
		return textResult(b.String()), triageOut{Problems: probs, StaleVants: stale, Healthy: healthy, VantagesChecked: names, RegistryAvailable: regOK}, nil
	})
}

// countHealthy counts targets that are healthy from every vantage that measures
// them: no down/degraded reading anywhere, and at least one healthy reading.
// It aggregates ALL of a target's rows across byV (order-independent booleans,
// not first-wins) so the result does not depend on Go's randomized map
// iteration order. A target that is only ever no_data is neither healthy nor a
// problem, so it is excluded here too.
func countHealthy(byV map[string][]Target) int {
	type agg struct {
		hasBad     bool
		hasHealthy bool
	}
	m := map[string]*agg{}
	for _, rows := range byV {
		for _, t := range rows {
			a := m[t.ID]
			if a == nil {
				a = &agg{}
				m[t.ID] = a
			}
			switch classify(t) {
			case "down", "degraded":
				a.hasBad = true
			case "healthy":
				a.hasHealthy = true
			}
		}
	}
	healthy := 0
	for _, a := range m {
		if !a.hasBad && a.hasHealthy {
			healthy++
		}
	}
	return healthy
}

// staleVantages flags a collector whose last_seen is missing (never reported).
// A time-based "older than newest by N" refinement is a follow-up; missing
// last_seen is the unambiguous dead-collector signal.
func staleVantages(vs []Vantage) []string {
	var out []string
	for _, v := range vs {
		if v.LastSeen == nil {
			out = append(out, v.Name)
		}
	}
	sort.Strings(out)
	return out
}
