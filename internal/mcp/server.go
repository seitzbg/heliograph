package mcp

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

// NewServer builds the MCP server and registers every Heliograph tool.
//
// Registration is incremental across tasks: this task ships only
// registerStatus. Later tasks add their own register* calls here as their
// tools land (heliograph_sla, heliograph_series, heliograph_triage,
// heliograph_vantages, heliograph_config_get, and the config_stage_*/
// config_review/config_apply/config_discard family).
func NewServer(c *Client, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "heliograph", Version: version}, nil)
	registerStatus(s, c)
	return s
}
