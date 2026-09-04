package mcp

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

// NewServer builds the MCP server and registers every Heliograph tool.
//
// Registration is incremental across tasks: this task ships registerStatus,
// registerSLA, registerVantages, registerSeries, registerTriage,
// registerConfigGet, registerConfigReview, and registerConfigDiscard. Later
// tasks add their own register* calls here as their tools land (the
// config_stage_*/config_apply family) — they share the staging buffer `st`
// created below.
func NewServer(c *Client, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "heliograph", Version: version}, nil)
	registerStatus(s, c)
	registerSLA(s, c)
	registerVantages(s, c)
	registerSeries(s, c)
	registerTriage(s, c)
	registerConfigGet(s, c)
	st := newStaging()
	registerConfigReview(s, c, st)
	registerConfigDiscard(s, st)
	return s
}
