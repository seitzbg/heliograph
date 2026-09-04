package mcp

import (
	"context"
	"fmt"
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
		scope := "vantage-specific"
		if a.healthy == 0 {
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

type triageIn struct {
	Vantage string `json:"vantage,omitempty" jsonschema:"restrict triage to a single vantage (default: all vantages)"`
}
type triageOut struct {
	Problems   []Problem `json:"problems"`
	StaleVants []string  `json:"stale_vantages"`
	Healthy    int       `json:"healthy_targets"`
}

func registerTriage(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_triage",
		Description: "Fast network health triage: classifies every target across vantages (healthy/degraded/down/no-data), separates GLOBAL problems (bad from every vantage → target issue) from VANTAGE-SPECIFIC ones (bad from one vantage → path/ISP issue), and flags stale collectors. Start here for an open-ended 'what's wrong?' investigation.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in triageIn) (*sdk.CallToolResult, triageOut, error) {
		vs, _, err := fetchVantages(ctx, c)
		if err != nil {
			return nil, triageOut{}, err
		}
		names := []string{}
		for _, v := range vs {
			if in.Vantage == "" || v.Name == in.Vantage {
				names = append(names, v.Name)
			}
		}
		if len(names) == 0 { // no vantage list wired; fall back to a single unfiltered read
			names = []string{""}
		}
		byV := map[string][]Target{}
		for _, n := range names {
			rows, err := fetchStatus(ctx, c, n)
			if err != nil {
				return nil, triageOut{}, err
			}
			byV[n] = rows
		}
		probs := analyzeTriage(byV)
		stale := staleVantages(vs)
		healthy := countHealthy(byV)
		var b strings.Builder
		fmt.Fprintf(&b, "%d problem target(s), %d healthy; %d stale vantage(s)\n", len(probs), healthy, len(stale))
		for _, p := range probs {
			fmt.Fprintf(&b, "- [%s/%s] %s (vantages: %s)\n", p.Status, p.Scope, p.Target, strings.Join(p.Vantages, ","))
		}
		if len(stale) > 0 {
			fmt.Fprintf(&b, "stale collectors: %s\n", strings.Join(stale, ","))
		}
		return textResult(b.String()), triageOut{Problems: probs, StaleVants: stale, Healthy: healthy}, nil
	})
}

func countHealthy(byV map[string][]Target) int {
	seen, healthy := map[string]bool{}, 0
	for _, rows := range byV {
		for _, t := range rows {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			if classify(t) == "healthy" {
				healthy++
			}
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
