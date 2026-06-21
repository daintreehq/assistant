package mcpx

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type fakeMCP struct {
	connected bool
	result    MCPCallResult
	lastName  string
	lastArgs  map[string]any
}

func (f *fakeMCP) Connected() bool { return f.connected }
func (f *fakeMCP) Status() MCPStatus {
	return MCPStatus{Connected: f.connected}
}
func (f *fakeMCP) CallTool(_ context.Context, name string, args map[string]any) (MCPCallResult, error) {
	f.lastName = name
	f.lastArgs = args
	return f.result, nil
}
func (f *fakeMCP) ListTools(_ context.Context, _ bool) ([]MCPToolInfo, error) { return nil, nil }

func TestExtractArmedSet(t *testing.T) {
	// structuredContent wins.
	if set, ok := extractArmedSet(map[string]any{
		"structuredContent": map[string]any{"armed": []any{"t1", "t2"}},
	}); !ok || len(set) != 2 {
		t.Errorf("structured armed not read: %v %v", set, ok)
	}
	// Empty set is preserved (legitimate after disarmAll).
	if set, ok := extractArmedSet(map[string]any{
		"structuredContent": map[string]any{"armed": []any{}},
	}); !ok || len(set) != 0 {
		t.Errorf("empty armed should be ok with len 0: %v %v", set, ok)
	}
	// JSON text fallback.
	if set, ok := extractArmedSet(map[string]any{"text": `{"armed":["x"]}`}); !ok || len(set) != 1 {
		t.Errorf("text armed not read: %v %v", set, ok)
	}
	// Missing set → not ok (so the wrapper fails loudly).
	if _, ok := extractArmedSet(map[string]any{"text": "no json"}); ok {
		t.Error("missing armed set should not be ok")
	}
}

func TestTerminalArmingReportsSetOrFailsLoudly(t *testing.T) {
	// Reports the armed list on success.
	mcp := &fakeMCP{connected: true, result: MCPCallResult{
		StructuredContent: map[string]any{"armed": []any{"t1"}},
	}}
	res := terminalArmingPassthrough(context.Background(), mcp, "terminal.arm",
		map[string]any{"terminalId": "t1"}, "Armed terminal t1.")
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}

	// No armed set anywhere → MCP_TOOL_ERROR (never a silent success).
	mcp2 := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok done"}}
	res2 := terminalArmingPassthrough(context.Background(), mcp2, "terminal.arm",
		map[string]any{"terminalId": "t1"}, "Armed terminal t1.")
	if res2.Ok || res2.Error.Code != codeMCPToolError {
		t.Fatalf("expected MCP_TOOL_ERROR, got %+v", res2)
	}
}

func TestDaintreeCallDenylistAndFileEditGuard(t *testing.T) {
	tool := newCallTool(Deps{MCP: &fakeMCP{connected: true}})
	dispatch := func(args string) tools.ToolResult {
		decoded, err := tool.Decode(json.RawMessage(args))
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		return tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	}

	// A wrapped tool is redirected, never forwarded.
	res := dispatch(`{"name":"agent.launch"}`)
	if res.Ok || res.Error.Code != codeUseTypedWrapper {
		t.Errorf("agent.launch should be redirected: %+v", res)
	}
	// A file-editing raw name is refused (no-file-edit guard).
	res = dispatch(`{"name":"fs.write"}`)
	if res.Ok || res.Error.Code != "FILE_EDIT_FORBIDDEN" {
		t.Errorf("fs.write should be forbidden: %+v", res)
	}
}

func TestMakeCallablePredicate(t *testing.T) {
	// nil ⇒ everything callable.
	all := makeCallable(nil)
	if !all("anything") {
		t.Error("nil active set should be unconstrained")
	}
	// constrained set.
	only := makeCallable([]string{"fs.read"})
	if !only("fs.read") || only("fs.write") {
		t.Error("constrained predicate wrong")
	}
}

func TestTerminalFocusMapsToPanelFocus(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newTerminalFocusTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t9"}`))
	tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if mcp.lastName != "panel.focus" {
		t.Errorf("terminal.focus should call panel.focus, called %q", mcp.lastName)
	}
	if mcp.lastArgs["panelId"] != "t9" {
		t.Errorf("panelId not set: %v", mcp.lastArgs)
	}
}
