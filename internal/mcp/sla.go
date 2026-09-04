package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SLAEntry mirrors the /api/sla row (internal/api slaEntry). Pointers where the API
// nulls a field so "coverage unknown" (step not configured) is distinguishable from
// zero coverage.
type SLAEntry struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Probe        string   `json:"probe"`
	Measured     int      `json:"measured"`
	UpRounds     int      `json:"up_rounds"`
	Availability float64  `json:"availability"`
	Expected     *int     `json:"expected"`
	CoveragePct  *float64 `json:"coverage_pct"`
	AvgLossPct   float64  `json:"avg_loss_pct"`
	CoveredFrom  string   `json:"covered_from"`
	Latest       string   `json:"latest"`
}

func fetchSLA(ctx context.Context, c *Client, window, maxLoss, vantage string) ([]SLAEntry, error) {
	q := url.Values{}
	if window != "" {
		q.Set("window", window)
	}
	if maxLoss != "" {
		q.Set("maxloss", maxLoss)
	}
	if vantage != "" {
		q.Set("vantage", vantage)
	}
	var env struct {
		Targets []SLAEntry `json:"targets"`
	}
	if err := c.getJSON(ctx, "/api/sla", q, &env); err != nil {
		return nil, err
	}
	return env.Targets, nil
}

type slaIn struct {
	Window  string `json:"window,omitempty" jsonschema:"time window as a Go duration, e.g. 24h or 7d-equivalent 168h (default 24h)"`
	MaxLoss string `json:"maxloss,omitempty" jsonschema:"optional loss-percent ceiling for a round to count as up, e.g. 5 (default: up = any reply)"`
	Vantage string `json:"vantage,omitempty" jsonschema:"optional vantage name"`
}
type slaOut struct {
	Targets []SLAEntry `json:"targets"`
}

func registerSLA(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_sla",
		Description: "Per-target availability/uptime over a window (worst-first). Availability %, rounds measured/up, coverage, and average loss. Use for uptime reporting and to rank the least-reliable targets.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in slaIn) (*sdk.CallToolResult, slaOut, error) {
		es, err := fetchSLA(ctx, c, in.Window, in.MaxLoss, in.Vantage)
		if err != nil {
			return nil, slaOut{}, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d targets (worst first)\n", len(es))
		for _, e := range es {
			fmt.Fprintf(&b, "- %s: %.2f%% avail (%d/%d rounds), avg loss %.1f%%\n", e.Name, e.Availability, e.UpRounds, e.Measured, e.AvgLossPct)
		}
		return textResult(b.String()), slaOut{Targets: es}, nil
	})
}
