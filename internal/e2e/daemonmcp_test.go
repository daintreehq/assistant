package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NOTE ON ANNOTATIONS: the tools below advertise no MCP `annotations`, so the client
// classifies every one of them single-shot (retry safety is derived from the live
// server's readOnlyHint — see internal/mcp/tools.go). That is harmless here because
// this fake injects no transport faults, so the retry path never runs either way.
// If you ever add fault injection, annotate the READS
// (&sdkmcp.ToolAnnotations{ReadOnlyHint: true, ...}) or the retry you mean to
// exercise will silently not happen and the test will pass for the wrong reason.
// benchmarks/orchestration/world/server.go does inject faults and annotates properly.

// termScript is one scripted terminal's live state on the scriptable MCP.
type termScript struct {
	AgentState    string
	WaitingReason string
	ExitCode      *int
}

// scriptableMCP is the daemon e2e's Daintree stand-in: the same transport-level
// go-sdk Streamable HTTP server as fakeMCP, but with a MUTABLE terminal roster
// the test drives mid-flight (flip an agent working→waiting to complete an
// async future under a live daemon), a bearer-token gate the test can rotate
// (credential revocation), and a recorder for mutating calls
// (terminal.sendCommand) so grant-authorized actions are observable.
type scriptableMCP struct {
	srv *httptest.Server

	mu           sync.Mutex
	terms        map[string]termScript
	token        string // "" ⇒ no auth enforcement
	sendCommands []string
}

func newScriptableMCP(t *testing.T) *scriptableMCP {
	t.Helper()
	m := &scriptableMCP{terms: map[string]termScript{}}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "scriptable-daintree", Version: "v0.0.1"}, nil)

	// Boot-probe reads (must succeed or the client degrades — see fakeMCP).
	type emptyIn struct{}
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "getContext", Description: "ctx"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"projectName":"fake-project"}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "project.getCurrent", Description: "project"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"project":{"id":"fake-project-id","name":"fake-project","path":"/fake/project"}}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "agent.listAvailable", Description: "agents"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"complete":true,"availabilityComplete":true,"agents":[]}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "agent.listToolbar", Description: "toolbar"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"agents":[]}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "cliAvailability.get", Description: "availability"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "agentSettings.get", Description: "roster"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"agents":{}}`), nil, nil
		})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "worktree.getCurrent", Description: "wt"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			return textResult(`{"worktree":null}`), nil, nil
		})

	// terminal.list — the live roster, from the scripted map.
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "terminal.list", Description: "list"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in emptyIn) (*sdkmcp.CallToolResult, any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			rows := ""
			for id, ts := range m.terms {
				if rows != "" {
					rows += ","
				}
				rows += fmt.Sprintf(`{"terminalId":%q,"kind":"agent","agentState":%q}`, id, ts.AgentState)
			}
			return textResult(`{"terminals":[` + rows + `]}`), nil, nil
		})

	// terminal.getStatus — the coordinator/watcher poll read.
	type statusIn struct {
		TerminalIds   []string `json:"terminalIds"`
		IncludeOutput any      `json:"includeOutput,omitempty"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "terminal.getStatus", Description: "status"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in statusIn) (*sdkmcp.CallToolResult, any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			rows := ""
			for _, id := range in.TerminalIds {
				ts, ok := m.terms[id]
				if !ok {
					continue // absent terminal — the roster read confirms exits
				}
				if rows != "" {
					rows += ","
				}
				row := fmt.Sprintf(`{"terminalId":%q,"agentState":%q,"waitingReason":%q`, id, ts.AgentState, ts.WaitingReason)
				if ts.ExitCode != nil {
					row += fmt.Sprintf(`,"exitCode":%d`, *ts.ExitCode)
				}
				row += "}"
				rows += row
			}
			return textResult(`{"terminals":[` + rows + `]}`), nil, nil
		})

	// terminal.sendCommand — the mutating call the wake-grant test authorizes.
	type sendIn struct {
		TerminalID string `json:"terminalId"`
		Command    string `json:"command"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "terminal.sendCommand", Description: "send"},
		func(ctx context.Context, req *sdkmcp.CallToolRequest, in sendIn) (*sdkmcp.CallToolResult, any, error) {
			m.mu.Lock()
			m.sendCommands = append(m.sendCommands, in.TerminalID+": "+in.Command)
			m.mu.Unlock()
			return textResult(`{"ok":true}`), nil, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server { return server },
		&sdkmcp.StreamableHTTPOptions{})
	// Bearer gate wrapper: when a token is armed, anything else is 401 — the
	// revocation the daemon must treat as terminal, not retry fodder.
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		tok := m.token
		m.mu.Unlock()
		if tok != "" && r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func textResult(s string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: s}}}
}

func (m *scriptableMCP) url() string { return m.srv.URL + "/mcp" }

// setTerminal scripts (or updates) one terminal's live state.
func (m *scriptableMCP) setTerminal(id, state, waitingReason string, exit *int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terms[id] = termScript{AgentState: state, WaitingReason: waitingReason, ExitCode: exit}
}

// requireToken arms (or rotates) the bearer gate.
func (m *scriptableMCP) requireToken(tok string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = tok
}

// sentCommands snapshots the recorded terminal.sendCommand calls.
func (m *scriptableMCP) sentCommands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.sendCommands))
	copy(out, m.sendCommands)
	return out
}
