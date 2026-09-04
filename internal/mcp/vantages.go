package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Vantage mirrors one row of the /api/admin/vantages listing (internal/api
// listVantages). LastSeen is nil for a vantage that has never reported in; Targets
// is nil (omitted) when the server can't compute a per-vantage target count.
type Vantage struct {
	Name     string  `json:"name"`
	Created  string  `json:"created"`
	LastSeen *string `json:"last_seen"`
	Targets  *int    `json:"targets,omitempty"`
}

func fetchVantages(ctx context.Context, c *Client) ([]Vantage, bool, error) {
	var env struct {
		Vantages        []Vantage `json:"vantages"`
		FederationReady bool      `json:"federation_ready"`
	}
	if err := c.getJSON(ctx, "/api/admin/vantages", nil, &env); err != nil {
		return nil, false, err
	}
	return env.Vantages, env.FederationReady, nil
}

type vantagesOut struct {
	Vantages        []Vantage `json:"vantages"`
	FederationReady bool      `json:"federation_ready"`
}

func registerVantages(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_vantages",
		Description: "List measurement vantages (collectors): name, created, last-seen, and target count, plus whether the hub runs the federation agent listener. Use to spot a stale or dead collector (a whole vantage's data missing is a collector fault, not a target fault).",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, vantagesOut, error) {
		vs, ready, err := fetchVantages(ctx, c)
		if err != nil {
			return nil, vantagesOut{}, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d vantages (federation_ready=%v)\n", len(vs), ready)
		for _, v := range vs {
			last := "never"
			if v.LastSeen != nil {
				last = *v.LastSeen
			}
			fmt.Fprintf(&b, "- %s: last_seen=%s\n", v.Name, last)
		}
		return textResult(b.String()), vantagesOut{Vantages: vs, FederationReady: ready}, nil
	})
}
