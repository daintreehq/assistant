package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCP is a real Daintree-shaped MCP server over the go-sdk's Streamable HTTP
// handler, served on httptest. The assistant's mcp.Client connects to it exactly as
// it would the live Daintree server (Streamable HTTP + Bearer token), so this is a
// genuine transport-level fake — not an interface stub. It advertises the read-only
// tools the assistant probes at boot: `getContext` (project identity), plus the two
// startup-context reads `agentSettings.get` (configured-agents roster) and
// `worktree.getCurrent` (active worktree). The latter two MUST exist: the shared
// mcp.Client degrades its connection status on ANY non-abort CallTool error, so a
// missing boot-read tool would flip Status().Connected to false right after a healthy
// connect (which the runtime-context builder then reports as "not connected"). Live
// Daintree implements all three, so a faithful fake must too.
type fakeMCP struct {
	srv *httptest.Server
}

// newFakeMCP starts the fake MCP server. The returned URL is the value to feed
// DAINTREE_MCP_URL (a /mcp endpoint).
func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-daintree", Version: "v0.0.1"}, nil)

	// getContext: the project-identity read the assistant probes at boot / in doctor.
	type getContextIn struct{}
	type getContextOut struct {
		ProjectName string `json:"projectName"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "getContext",
		Description: "Return the bound project context.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in getContextIn) (*sdkmcp.CallToolResult, getContextOut, error) {
		out := getContextOut{ProjectName: "fake-project"}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"projectName":"fake-project"}`}},
		}, out, nil
	})

	// agentSettings.get: the configured-agents roster read (agenttaskx.ConfiguredAgentIDs)
	// the assistant runs on every (re)connect. An empty roster is a valid, successful
	// answer — what matters is that the call SUCCEEDS so it doesn't degrade the connection.
	type agentSettingsIn struct{}
	type agentSettingsOut struct {
		Agents map[string]any `json:"agents"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "agentSettings.get",
		Description: "Return the configured-agents settings map.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in agentSettingsIn) (*sdkmcp.CallToolResult, agentSettingsOut, error) {
		out := agentSettingsOut{Agents: map[string]any{}}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"agents":{}}`}},
		}, out, nil
	})

	// worktree.getCurrent: the active-worktree read (refreshStartupContext). A definitive
	// null ("not in a worktree") is a clean, successful answer.
	type worktreeIn struct{}
	type worktreeOut struct {
		Worktree any `json:"worktree"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "worktree.getCurrent",
		Description: "Return the current worktree, or null when not in one.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in worktreeIn) (*sdkmcp.CallToolResult, worktreeOut, error) {
		out := worktreeOut{Worktree: nil}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"worktree":null}`}},
		}, out, nil
	})

	// terminal.list: the live terminal inventory the boot ledger reconcile reads once on
	// the first connect (app.ReconcileLedger → readLiveTerminalIDs). An empty terminals
	// array is a valid answer (no live terminals) and, crucially, a SUCCESSFUL call — so it
	// doesn't degrade the connection the way a missing tool would.
	type terminalListIn struct{}
	type terminalListOut struct {
		Terminals []any `json:"terminals"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "terminal.list",
		Description: "List the live terminals.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in terminalListIn) (*sdkmcp.CallToolResult, terminalListOut, error) {
		out := terminalListOut{Terminals: []any{}}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"terminals":[]}`}},
		}, out, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &fakeMCP{srv: srv}
}

// url is the /mcp endpoint to feed DAINTREE_MCP_URL.
func (m *fakeMCP) url() string { return m.srv.URL + "/mcp" }
