package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServerRegistersExpectedTools asserts registration by driving the server through
// an in-memory client session and listing tools, rather than via a direct enumerator
// on *sdk.Server: v1.7.0 exposes no such enumerator (registered tools are unexported
// state on Server; only ClientSession has a public ListTools/Tools).
func TestServerRegistersExpectedTools(t *testing.T) {
	c, _ := newTestClient(t, nil)
	s := NewServer(c, "test")

	ctx := context.Background()
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	if _, err := s.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer cs.Close()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{
		"heliograph_status",
		"heliograph_sla",
		"heliograph_vantages",
		"heliograph_series",
		"heliograph_triage",
		"heliograph_config_get",
		"config_review",
		"config_discard",
	} {
		if !got[name] {
			t.Errorf("tool %q not registered", name)
		}
	}
}
