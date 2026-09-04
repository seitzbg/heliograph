package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerConfigReview and registerConfigDiscard are the two config-write tools this
// task ships. config_stage_*/config_apply follow in Tasks 8–10 and will extend this file.

type reviewOut struct {
	Staged        bool     `json:"staged"`
	BaseVersion   int      `json:"base_version"`
	LiveVersion   int      `json:"live_version"`
	Drifted       bool     `json:"drifted"`
	Added         []string `json:"added"`
	Removed       []string `json:"removed"`
	Changed       []string `json:"changed"`
	WorkingConfig string   `json:"working_config"`
}

func registerConfigReview(s *sdk.Server, c *Client, st *staging) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_review",
		Description: "Show the currently staged config changes: which targets were added/removed/changed, the working config, and whether the live config version has drifted since staging (a pending conflict). Local-only; nothing is written. Call config_apply to commit.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, reviewOut, error) {
		if !st.active {
			return textResult("no staged changes"), reviewOut{Staged: false}, nil
		}
		added, removed, changed, err := st.diff()
		if err != nil {
			return nil, reviewOut{}, err
		}
		_, liveVer, err := c.getConfigDoc(ctx, "db")
		if err != nil {
			return nil, reviewOut{}, err
		}
		work := string(st.working())
		out := reviewOut{
			Staged: true, BaseVersion: st.baseVersion(), LiveVersion: liveVer,
			Drifted: liveVer != st.baseVersion(), Added: added, Removed: removed, Changed: changed,
			WorkingConfig: work,
		}
		var b strings.Builder
		fmt.Fprintf(&b, "staged (base v%d, live v%d%s)\nadded: %v\nremoved: %v\nchanged: %v\n",
			out.BaseVersion, out.LiveVersion, driftNote(out.Drifted), added, removed, changed)
		return textResult(b.String()), out, nil
	})
}

func driftNote(d bool) string {
	if d {
		return " — DRIFTED, apply will 409"
	}
	return ""
}

type discardOut struct {
	Discarded bool `json:"discarded"`
}

func registerConfigDiscard(s *sdk.Server, st *staging) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "config_discard",
		Description: "Discard all staged config changes and reset the staging buffer. Local-only.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, discardOut, error) {
		st.reset()
		return textResult("staged changes discarded"), discardOut{Discarded: true}, nil
	})
}
