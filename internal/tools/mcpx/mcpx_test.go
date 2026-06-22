package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
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

func TestTruncateCommand(t *testing.T) {
	// Under the cap is returned verbatim.
	if got := truncateCommand("git status", 80); got != "git status" {
		t.Errorf("short command altered: %q", got)
	}
	// Exactly at the cap → no ellipsis.
	exact := strings.Repeat("a", 80)
	if got := truncateCommand(exact, 80); got != exact {
		t.Errorf("at-cap command should be verbatim, got len %d", len(got))
	}
	// One over the cap → clipped to cap + "...".
	over := strings.Repeat("a", 81)
	if got := truncateCommand(over, 80); got != exact+"..." {
		t.Errorf("over-cap command not clipped with ellipsis: %q", got)
	}
	// Newlines/tabs collapse to single spaces and ends are trimmed, so a
	// heredoc-style command stays on one line.
	if got := truncateCommand("  echo hi\n\tthen\r\nbye  ", 80); got != "echo hi then bye" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	// Rune-aware: a multibyte command is never cut mid-codepoint.
	multi := strings.Repeat("é", 81)
	got := truncateCommand(multi, 80)
	if !strings.HasSuffix(got, "...") || strings.Count(got, "é") != 80 {
		t.Errorf("multibyte truncation wrong: %q (count %d)", got, strings.Count(got, "é"))
	}
}

func TestTerminalSendCommandSummary(t *testing.T) {
	// Success: the generic "Called terminal.sendCommand." is replaced with a
	// concrete summary echoing the terminalId + command.
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok", StructuredContent: map[string]any{"k": "v"}}}
	tool := newTerminalSendCommandTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(`{"terminalId":"t7","command":"go test ./..."}`))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if res.Summary != "Sent to terminal t7: go test ./...." {
		t.Errorf("summary not self-describing: %q", res.Summary)
	}
	if mcp.lastName != "terminal.sendCommand" {
		t.Errorf("forwarded wrong MCP name: %q", mcp.lastName)
	}
	// The original result payload is preserved, not discarded.
	if rm, _ := res.Result.(map[string]any); rm["structuredContent"] == nil {
		t.Errorf("result payload not preserved: %v", res.Result)
	}

	// A long command is clipped in the summary (still single-line, ellipsised).
	long := strings.Repeat("x", 200)
	d2, _ := tool.Decode(json.RawMessage(`{"terminalId":"t7","command":"` + long + `"}`))
	r2 := tool.Handle(context.Background(), d2, &tools.ToolContext{})
	if !strings.HasPrefix(r2.Summary, "Sent to terminal t7: "+strings.Repeat("x", 80)+"...") {
		t.Errorf("long command not clipped: %q", r2.Summary)
	}

	// Whitespace-only command is rejected before any MCP call.
	mcp2 := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool2 := newTerminalSendCommandTool(Deps{MCP: mcp2})
	d3, _ := tool2.Decode(json.RawMessage(`{"terminalId":"t7","command":"   "}`))
	r3 := tool2.Handle(context.Background(), d3, &tools.ToolContext{})
	if r3.Ok || r3.Error.Code != domain.CodeValidation {
		t.Errorf("blank command should be rejected: %+v", r3)
	}
	if mcp2.lastName != "" {
		t.Errorf("blank command must not reach MCP, called %q", mcp2.lastName)
	}
}

func TestTerminalSendCommandFailurePreserved(t *testing.T) {
	// A disconnected MCP fails through passthrough; the wrapper must NOT manufacture
	// a success "Sent to terminal" summary over a failed delivery.
	mcp := &fakeMCP{connected: false}
	res := terminalSendCommandPassthrough(context.Background(), mcp, "t1", "ls",
		map[string]any{"terminalId": "t1", "command": "ls"})
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("disconnected send should fail loudly: %+v", res)
	}
	if strings.Contains(res.Summary, "Sent to terminal") {
		t.Errorf("failed send must not claim delivery: %q", res.Summary)
	}
}

func TestCopyTreeInjectSummary(t *testing.T) {
	// Success: concrete summary naming the destination terminal.
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCopyTreeInjectTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t42"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if res.Summary != "Injected copy tree into terminal t42." {
		t.Errorf("inject summary not self-describing: %q", res.Summary)
	}
	if mcp.lastName != "copyTree.injectToTerminal" {
		t.Errorf("forwarded wrong MCP name: %q", mcp.lastName)
	}

	// Whitespace-only terminalId is rejected before any MCP call.
	mcp2 := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool2 := newCopyTreeInjectTool(Deps{MCP: mcp2})
	d2, _ := tool2.Decode(json.RawMessage(`{"terminalId":"  "}`))
	r2 := tool2.Handle(context.Background(), d2, &tools.ToolContext{})
	if r2.Ok || r2.Error.Code != domain.CodeValidation {
		t.Errorf("blank terminalId should be rejected: %+v", r2)
	}
	if mcp2.lastName != "" {
		t.Errorf("blank terminalId must not reach MCP, called %q", mcp2.lastName)
	}

	// A failed passthrough must not produce a success summary.
	mcp3 := &fakeMCP{connected: false}
	r3 := copyTreeInjectPassthrough(context.Background(), mcp3, "t1", map[string]any{"terminalId": "t1"})
	if r3.Ok || strings.Contains(r3.Summary, "Injected copy tree") {
		t.Errorf("failed inject must not claim delivery: %+v", r3)
	}
}
