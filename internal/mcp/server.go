package mcp

import sdk "github.com/modelcontextprotocol/go-sdk/mcp"

// NewServer builds the MCP server and registers every Heliograph tool.
//
// Registration: registerStatus, registerSLA, registerVantages, registerSeries,
// registerTriage, registerConfigGet, registerConfigReview, registerConfigDiscard,
// registerConfigStage (config_stage_add_target/edit_target/remove_target),
// registerConfigReplace (config_stage_replace), and registerConfigApply
// (config_apply — the only tool that writes to the live hub). The config_*
// tools share the staging buffer `st` created below.
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
	registerConfigStage(s, c, st)
	registerConfigReplace(s, c, st)
	registerConfigApply(s, c, st)
	return s
}
