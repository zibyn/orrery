package cli

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zibyn/stars-sandbox/internal/mcpserver"
)

// runMCP serves the MCP Tool API over stdio (api.md §7). Stdout carries only
// protocol frames; diagnostics go to stderr.
func runMCP(c *client, args []string, stderr io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("mcp takes no arguments")
	}
	// A Sandbox Token only works for POST /git/credential (api.md §1).
	if strings.HasPrefix(c.Token(), "sst_") {
		return fmt.Errorf("Sandbox Tokens are not accepted by the MCP server; use an API Token")
	}
	srv := mcpserver.New(c.Client, slog.New(slog.NewTextHandler(stderr, nil)))
	return srv.Run(ctx, &sdk.StdioTransport{})
}
