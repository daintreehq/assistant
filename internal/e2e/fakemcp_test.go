package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCP is a real Daintree-shaped MCP server over the go-sdk's Streamable HTTP
// handler, served on httptest. The assistant's mcp.Client connects to it exactly as
// it would the live Daintree server (Streamable HTTP + Bearer token), so this is a
// genuine transport-level fake — not an interface stub. It advertises the read-only
// tools the assistant probes at boot: project identity, full effective agent
// availability, toolbar state, and the active worktree. These MUST exist: the shared
// mcp.Client degrades its connection status on ANY non-abort CallTool error, so a
// missing boot-read tool would flip Status().Connected to false right after a healthy
// connect (which the runtime-context builder then reports as "not connected"). Live
// Daintree implements all three, so a faithful fake must too.
type fakeMCP struct {
	srv            *httptest.Server
	agentListDelay time.Duration
	agentListCalls atomic.Int32
}

// newFakeMCP starts the fake MCP server. The returned URL is the value to feed
// DAINTREE_MCP_URL (a /mcp endpoint).
func newFakeMCP(t *testing.T) *fakeMCP {
	return newFakeMCPWithAgentDelay(t, 0)
}

// newFakeMCPWithAgentDelay makes the stable agent-catalog read deliberately outlive
// the ~740ms logo. The splash/PTY regression uses it to prove startup discovery keeps
// running once across hand-off while the composer is already accepting input.
func newFakeMCPWithAgentDelay(t *testing.T, delay time.Duration) *fakeMCP {
	t.Helper()
	m := &fakeMCP{agentListDelay: delay}
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

	// project.getCurrent: the stable project snapshot attached before every conversation.
	type projectCurrentIn struct{}
	type projectCurrentOut struct {
		Project map[string]any `json:"project"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "project.getCurrent",
		Description: "Return the current project.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in projectCurrentIn) (*sdkmcp.CallToolResult, projectCurrentOut, error) {
		project := map[string]any{"id": "fake-project-id", "name": "fake-project", "path": "/fake/project", "status": "active"}
		out := projectCurrentOut{Project: project}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"project":{"id":"fake-project-id","name":"fake-project","path":"/fake/project","status":"active"}}`}},
		}, out, nil
	})

	// agent.listAvailable is the authoritative startup agent catalog.
	type availableAgentsIn struct{}
	type availableAgentsOut struct {
		Complete             bool             `json:"complete"`
		AvailabilityComplete bool             `json:"availabilityComplete"`
		Agents               []map[string]any `json:"agents"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "agent.listAvailable",
		Description: "Return every directly launchable agent.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in availableAgentsIn) (*sdkmcp.CallToolResult, availableAgentsOut, error) {
		m.agentListCalls.Add(1)
		if m.agentListDelay > 0 {
			select {
			case <-ctx.Done():
				return nil, availableAgentsOut{}, ctx.Err()
			case <-time.After(m.agentListDelay):
			}
		}
		agents := []map[string]any{
			{"id": "claude", "displayName": "Claude Code", "source": "built-in", "availability": "ready", "installed": true, "toolbarVisible": true, "pinned": true},
			{"id": "team-agent", "displayName": "Team Agent", "source": "user", "availability": "unauthenticated", "installed": true},
		}
		out := availableAgentsOut{Complete: true, AvailabilityComplete: true, Agents: agents}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"complete":true,"availabilityComplete":true,"agents":[{"id":"claude","displayName":"Claude Code","source":"built-in","availability":"ready","installed":true,"toolbarVisible":true,"pinned":true},{"id":"team-agent","displayName":"Team Agent","source":"user","availability":"unauthenticated","installed":true}]}`}},
		}, out, nil
	})

	// Retain the individual discovery reads because the model can still request them.
	type toolbarIn struct{}
	type toolbarOut struct {
		Agents []map[string]any `json:"agents"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "agent.listToolbar",
		Description: "Return toolbar agent state.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in toolbarIn) (*sdkmcp.CallToolResult, toolbarOut, error) {
		agents := []map[string]any{{"id": "claude", "displayName": "Claude Code", "installed": true, "visible": true, "pinned": true}}
		out := toolbarOut{Agents: agents}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"agents":[{"id":"claude","displayName":"Claude Code","installed":true,"visible":true,"pinned":true}]}`}},
		}, out, nil
	})
	type availabilityIn struct{}
	type availabilityOut map[string]string
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "cliAvailability.get",
		Description: "Return effective agent availability.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in availabilityIn) (*sdkmcp.CallToolResult, availabilityOut, error) {
		out := availabilityOut{"claude": "ready", "team-agent": "unauthenticated"}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"claude":"ready","team-agent":"unauthenticated"}`}},
		}, out, nil
	})

	// agentSettings.get remains available for spawn-validation coverage.
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
	m.srv = httptest.NewServer(handler)
	t.Cleanup(m.srv.Close)
	return m
}

// url is the /mcp endpoint to feed DAINTREE_MCP_URL.
func (m *fakeMCP) url() string { return m.srv.URL + "/mcp" }

func (m *fakeMCP) agentListCallCount() int { return int(m.agentListCalls.Load()) }
