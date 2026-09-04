package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Target mirrors the /api/targets row (internal/api targetDTO). Pointers where the
// API omits/nulls a field so "no data" is distinguishable from zero.
type Target struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Title         string   `json:"title,omitempty"`
	IP            string   `json:"ip,omitempty"`
	Probe         string   `json:"probe"`
	Metric        string   `json:"metric"`
	MedianMs      *float64 `json:"median_ms"`
	Loss          int      `json:"loss"`
	Pings         int      `json:"pings"`
	LossPct       float64  `json:"loss_pct"`
	RecentLossPct *float64 `json:"recent_loss_pct,omitempty"`
	When          string   `json:"when"`
	Error         string   `json:"error,omitempty"`
	Vantages      []string `json:"vantages,omitempty"`
	NTPOffsetMs   *float64 `json:"ntp_offset_ms,omitempty"`
	Stratum       *int     `json:"stratum,omitempty"`
	NTPMeasure    string   `json:"ntp_measure,omitempty"`
	NoData        bool     `json:"no_data,omitempty"`
}

func fetchStatus(ctx context.Context, c *Client, vantage string) ([]Target, error) {
	q := url.Values{}
	if vantage != "" {
		q.Set("vantage", vantage)
	}
	var env struct {
		Targets []Target `json:"targets"`
	}
	if err := c.getJSON(ctx, "/api/targets", q, &env); err != nil {
		return nil, err
	}
	return env.Targets, nil
}

type statusIn struct {
	Vantage string `json:"vantage,omitempty" jsonschema:"optional vantage name to filter to (omit for the default read vantage)"`
}
type statusOut struct {
	Targets []Target `json:"targets"`
}

func registerStatus(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_status",
		Description: "Current per-target snapshot: probe, latest median latency, loss %, recent loss %, NTP offset/stratum, and which vantages measure it. Use to see the live state of every monitored target.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in statusIn) (*sdk.CallToolResult, statusOut, error) {
		ts, err := fetchStatus(ctx, c, in.Vantage)
		if err != nil {
			return nil, statusOut{}, err
		}
		return textResult(summarizeTargets(ts)), statusOut{Targets: ts}, nil
	})
}

func summarizeTargets(ts []Target) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d targets\n", len(ts))
	for _, t := range ts {
		med := "—"
		if t.MedianMs != nil {
			med = fmt.Sprintf("%.1fms", *t.MedianMs)
		}
		fmt.Fprintf(&b, "- %s [%s] median=%s loss=%.0f%%\n", t.Name, t.Probe, med, t.LossPct)
	}
	return b.String()
}

// textResult wraps a plain-text summary as a CallToolResult (shared by all tools).
func textResult(text string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}
