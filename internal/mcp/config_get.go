package mcp

import (
	"context"
	"fmt"
	"net/url"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fetchConfig returns the config rendered as text plus (for source=db, format=json) a version.
func fetchConfig(ctx context.Context, c *Client, source, format string) (string, int, error) {
	if source == "" {
		source = "db"
	}
	if format == "" {
		format = "yaml"
	}
	if format == "yaml" {
		q := url.Values{}
		q.Set("source", source)
		b, err := c.getBytes(ctx, "/api/admin/config.yaml", q)
		if err != nil {
			return "", 0, err
		}
		return string(b), 0, nil
	}
	doc, version, err := c.getConfigDoc(ctx, source)
	if err != nil {
		return "", 0, err
	}
	return string(doc), version, nil
}

type configGetIn struct {
	Source string `json:"source,omitempty" jsonschema:"db (editable DB fragment, default) or effective (file+DB merged, read-only)"`
	Format string `json:"format,omitempty" jsonschema:"yaml (default) or json"`
}
type configGetOut struct {
	Source  string `json:"source"`
	Format  string `json:"format"`
	Version int    `json:"version,omitempty"`
	Content string `json:"content"`
}

func registerConfigGet(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_config_get",
		Description: "Read the monitoring config. source=db is the editable DB fragment (with a version for optimistic-concurrency writes); source=effective is the read-only file+DB merged config the collector runs. format yaml (default) or json.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in configGetIn) (*sdk.CallToolResult, configGetOut, error) {
		src, fmtv := in.Source, in.Format
		if src == "" {
			src = "db"
		}
		if fmtv == "" {
			fmtv = "yaml"
		}
		content, version, err := fetchConfig(ctx, c, src, fmtv)
		if err != nil {
			return nil, configGetOut{}, err
		}
		summary := fmt.Sprintf("config source=%s format=%s version=%d\n%s", src, fmtv, version, content)
		return textResult(summary), configGetOut{Source: src, Format: fmtv, Version: version, Content: content}, nil
	})
}
