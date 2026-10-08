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

// fetchVantages reads the hub's remote-vantage registry (GET /api/admin/vantages). The hub serves
// it only when federation is possible — it runs with a database (-dsn) and an admin password — so
// a 404 is reported as ok=false rather than an error: the registry is absent, not failing. Any other
// failure (a 5xx, a proxy auth rejection) is returned as an error, never treated as "no vantages".
func fetchVantages(ctx context.Context, c *Client) (vs []Vantage, federationReady, ok bool, err error) {
	var env struct {
		Vantages        []Vantage `json:"vantages"`
		FederationReady bool      `json:"federation_ready"`
	}
	if err := c.getJSON(ctx, "/api/admin/vantages", nil, &env); err != nil {
		if isNotFound(err) {
			return nil, false, false, nil
		}
		return nil, false, false, err
	}
	return env.Vantages, env.FederationReady, true, nil
}

// noRegistryNote explains an absent registry to the assistant.
const noRegistryNote = "This hub has no remote-vantage registry: GET /api/admin/vantages returned 404. " +
	"The registry is served only by a hub running with a database (-dsn) and an admin password " +
	"(SMOKED_ADMIN_PASSWORD), which federation requires, so this hub has no remote vantages: its targets " +
	"are measured from its built-in \"local\" vantage. heliograph_status and heliograph_triage still work."

type vantagesOut struct {
	RegistryAvailable bool      `json:"registry_available"`
	Vantages          []Vantage `json:"vantages"`
	FederationReady   bool      `json:"federation_ready"`
}

func registerVantages(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_vantages",
		Description: "List the hub's registered REMOTE measurement vantages (collectors): name, created, last-seen, and target count, plus whether the hub runs the federation agent listener. The hub's built-in \"local\" vantage is not a registry entry and is not listed. Use to spot a stale or dead collector (a whole vantage's data missing is a collector fault, not a target fault). On a single-host hub without federation, registry_available is false.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, vantagesOut, error) {
		vs, ready, ok, err := fetchVantages(ctx, c)
		if err != nil {
			return nil, vantagesOut{}, err
		}
		if !ok {
			return textResult(noRegistryNote), vantagesOut{RegistryAvailable: false}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d remote vantages (federation_ready=%v)\n", len(vs), ready)
		for _, v := range vs {
			last := "never"
			if v.LastSeen != nil {
				last = *v.LastSeen
			}
			fmt.Fprintf(&b, "- %s: last_seen=%s\n", v.Name, last)
		}
		return textResult(b.String()), vantagesOut{RegistryAvailable: true, Vantages: vs, FederationReady: ready}, nil
	})
}
