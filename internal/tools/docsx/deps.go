// Package docsx is the Daintree documentation tool family: docs.search,
// docs.getPage, and docs.getRelatedPages. These are READ-ONLY (risk read) wrappers
// over the SECOND MCP server — the public Daintree documentation MCP at
// https://daintree.org/api/mcp — used to answer "how do I use Daintree" / "what is X"
// help questions from live documentation, NOT to drive the running Daintree app.
//
// Unlike the mcpwrap/mcpx families (which reach the primary Daintree control-plane MCP
// through ToolContext.MCP), this family reaches a SEPARATE client passed via Deps and
// captured in each handler's closure — so the docs MCP stays fully isolated from the
// control-plane MCP and the central ToolContext is untouched.
package docsx

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCPClient is the slice of the docs MCP transport these wrappers use. It is a LOCAL
// consumer-defined interface (the same shape as mcpwrap.MCPClient) so this package
// compiles in isolation; the concrete *mcp.Client satisfies it through the adapter in
// the app wiring. CallTool forwards the validated args; Connected gates every call.
type MCPClient interface {
	CallTool(ctx context.Context, name string, args map[string]any) (tools.MCPCallResult, error)
	Connected() bool
}

// Deps wires the docs family to its (docs) MCP client.
type Deps struct {
	// MCP is the docs MCP transport. When nil (or disconnected), every docs tool fails
	// cleanly with MCP_UNAVAILABLE — the family degrades, never panics.
	MCP MCPClient
}
