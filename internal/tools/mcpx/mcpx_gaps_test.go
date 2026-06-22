package mcpx

import (
	"context"
	"encoding/json"
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
