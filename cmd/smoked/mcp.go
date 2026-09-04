package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	hmcp "github.com/seitzbg/heliograph/internal/mcp"
)

func mcpCmd(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	url := fs.String("url", os.Getenv("HELIOGRAPH_URL"), "Heliograph hub base URL, e.g. https://heliograph.example (or HELIOGRAPH_URL)")
	basicUser := fs.String("basic-user", os.Getenv("HELIOGRAPH_BASIC_USER"), "proxy Basic Auth username (or HELIOGRAPH_BASIC_USER)")
	basicPass := fs.String("basic-pass", os.Getenv("HELIOGRAPH_BASIC_PASS"), "proxy Basic Auth password (or HELIOGRAPH_BASIC_PASS)")
	adminPass := fs.String("admin-pass", os.Getenv("HELIOGRAPH_ADMIN_PASS"), "admin password; enables config writes and unredacted reads (or HELIOGRAPH_ADMIN_PASS)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := hmcp.NewClient(hmcp.Config{BaseURL: *url, BasicUser: *basicUser, BasicPass: *basicPass, AdminPass: *adminPass})
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoked mcp:", err)
		return 1
	}
	srv := hmcp.NewServer(c, version)
	if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "smoked mcp:", err)
		return 1
	}
	return 0
}
