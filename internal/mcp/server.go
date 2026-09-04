package mcp

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

// NewServer builds the MCP server and registers every Heliograph tool.
//
// Registration is incremental across tasks: this task ships registerStatus,
// registerSLA, registerVantages, registerSeries, and registerTriage. Later
// tasks add their own register* calls here as their tools land
// (heliograph_config_get, and the config_stage_*/config_review/config_apply/
// config_discard family).
func NewServer(c *Client, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "heliograph", Version: version}, nil)
	registerStatus(s, c)
	registerSLA(s, c)
	registerVantages(s, c)
	registerSeries(s, c)
	registerTriage(s, c)
	return s
}
