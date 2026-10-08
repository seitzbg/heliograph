package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/seitzbg/heliograph/internal/config"
)

// mcpSession connects an in-memory MCP client to a fresh server built on c, so a test drives the
// tools exactly as an assistant would (argument decoding, structured output, tool errors).
func mcpSession(t *testing.T, c *Client) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	ss, err := NewServer(c, "test").Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool calls a tool and decodes its structured output into out (when non-nil). It returns the
// raw result so a test can assert IsError and the text content.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) *sdk.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal %s structured content: %v", name, err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s structured content %s: %v", name, b, err)
		}
	}
	return res
}

// resultText concatenates a tool result's text content (the human/LLM-facing summary or error).
func resultText(res *sdk.CallToolResult) string {
	var s string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}

// configHub is an httptest handler that serves the hub's config API the way cmd/smoked does: the
// DB fragment at source=db, and at source=effective the file config with the DB fragment composed
// onto it by the same config.Parse + config.AppendDBFragment path buildRuntime uses. file is the
// hub's YAML file config ("" = no file targets).
func configHub(t *testing.T, file, db string) http.Handler {
	t.Helper()
	cfg, err := config.Parse([]byte(file))
	if err != nil {
		t.Fatalf("parse file config: %v", err)
	}
	if err := config.AppendDBFragment(cfg, []byte(db)); err != nil {
		t.Fatalf("compose fixture: %v", err)
	}
	effective, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal effective: %v", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/admin/login":
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "t", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/config" && r.URL.Query().Get("source") == "effective":
			_ = json.NewEncoder(w).Encode(map[string]any{"readonly": true, "doc": json.RawMessage(effective)})
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 7, "doc": json.RawMessage(db)})
		default:
			http.NotFound(w, r)
		}
	})
}
