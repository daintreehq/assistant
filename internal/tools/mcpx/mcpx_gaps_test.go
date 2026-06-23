package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// The daintree.call denylist redirect NAMES the typed wrapper for every wrapped
// raw tool, and never forwards the raw call.
func TestDaintreeCallDenylistNamesWrapper(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	tool := newCallTool(Deps{MCP: mcp})
	for raw, wrapper := range map[string]string{
		"agent.launch":         "agentTask.spawnForEdits",
		"terminal.sendCommand": "terminal.sendCommand",
		"terminal.arm":         "terminal.arm",
		"git.snapshotDelete":   "git.snapshotDelete",
	} {
		decoded, _ := tool.Decode(json.RawMessage(`{"name":"` + raw + `"}`))
		res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeUseTypedWrapper {
			t.Fatalf("%s should be redirected, got %+v", raw, res)
		}
		if !strings.Contains(res.Error.Message, wrapper) {
			t.Fatalf("redirect for %s must name %q, got %q", raw, wrapper, res.Error.Message)
		}
		if mcp.lastName == raw {
			t.Fatalf("raw %s must never be forwarded", raw)
		}
	}
}

// A disconnected MCP must point the model at /reconnect — the recovery command
// that works in both the REPL and the cockpit (issue #211). Covers the shared
// passthrough plus the three discovery tools that report disconnection.
func TestMCPUnavailableErrorsNameReconnect(t *testing.T) {
	disc := &fakeMCP{connected: false}

	// The shared passthrough every typed wrapper delegates to.
	pres := passthrough(context.Background(), disc, "worktree.list", nil, "")
	if pres.Ok || pres.Error.Code != codeMCPUnavailable {
		t.Fatalf("disconnected passthrough should be MCP_UNAVAILABLE, got %+v", pres)
	}
	if !strings.Contains(pres.Error.Message, "/reconnect") {
		t.Errorf("passthrough hint must name /reconnect: %q", pres.Error.Message)
	}

	// The discovery tools that surface a disconnected MCP to the model.
	cases := []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"daintree.listTools", newListToolsTool(Deps{MCP: disc}), `{}`},
		{"tool.search", newSearchTool(Deps{MCP: disc}), `{"query":"x"}`},
		{"daintree.call", newCallTool(Deps{MCP: disc}), `{"name":"worktree.list"}`},
	}
	for _, tc := range cases {
		decoded, err := tc.tool.Decode(json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s decode: %v", tc.name, err)
		}
		res := tc.tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeMCPUnavailable {
			t.Fatalf("%s disconnected should be MCP_UNAVAILABLE, got %+v", tc.name, res)
		}
		if !strings.Contains(res.Error.Message, "/reconnect") {
			t.Errorf("%s hint must name /reconnect: %q", tc.name, res.Error.Message)
		}
	}
}

// A connection that reports Connected()==true but then errors mid-RPC (a stale
// link dropping during ListTools/search) is also MCP_UNAVAILABLE, and must carry
// the same /reconnect recovery hint as the up-front disconnected check.
func TestMCPStaleConnectionErrorsNameReconnect(t *testing.T) {
	stale := &fakeMCP{connected: true, listErr: errors.New("stream reset")}

	cases := []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"daintree.listTools", newListToolsTool(Deps{MCP: stale}), `{}`},
		{"tool.search", newSearchTool(Deps{MCP: stale}), `{"query":"x"}`},
	}
	for _, tc := range cases {
		decoded, err := tc.tool.Decode(json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s decode: %v", tc.name, err)
		}
		res := tc.tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeMCPUnavailable {
			t.Fatalf("%s mid-RPC failure should be MCP_UNAVAILABLE, got %+v", tc.name, res)
		}
		if !strings.Contains(res.Error.Message, "/reconnect") {
			t.Errorf("%s stale-connection hint must name /reconnect: %q", tc.name, res.Error.Message)
		}
	}
}

// daintree.call forwards requestKey as a dedicated top-level param (not buried in
// the arguments payload).
func TestDaintreeCallForwardsRequestKeyAsParam(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCallTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"name":"worktree.list","arguments":{"a":1},"requestKey":"rk-9"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if mcp.lastArgs["requestKey"] != "rk-9" {
		t.Fatalf("requestKey not forwarded as a param: %v", mcp.lastArgs)
	}
	if mcp.lastArgs["a"] != float64(1) {
		t.Fatalf("arguments payload not forwarded: %v", mcp.lastArgs)
	}
}
