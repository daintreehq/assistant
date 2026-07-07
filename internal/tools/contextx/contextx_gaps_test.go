package contextx

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// recordingMCP records the last call args so we can assert maxLines forwarding.
type recordingMCP struct {
	connected bool
	result    MCPCallResult
	err       error
	lastName  string
	lastArgs  map[string]any
	calls     int
}

func (r *recordingMCP) Connected() bool   { return r.connected }
func (r *recordingMCP) Status() MCPStatus { return MCPStatus{Connected: r.connected} }
func (r *recordingMCP) CallTool(_ context.Context, name string, args map[string]any) (MCPCallResult, error) {
	r.calls++
	r.lastName = name
	r.lastArgs = args
	return r.result, r.err
}

// recordChat fails the test if Summarize is ever called — terminal.read must never
// consult the model.
type recordChat struct{ called bool }

func (c *recordChat) Summarize(_ context.Context, _ string, _ string) (string, error) {
	c.called = true
	return "should not happen", nil
}

func readResult(t *testing.T, res tools.ToolResult) map[string]any {
	t.Helper()
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	return res.Result.(map[string]any)
}

// terminal.read returns scrollback verbatim, never calls the model, and forwards
// maxLines to terminal.getOutput.
func TestReadVerbatimNoModelAndForwardsMaxLines(t *testing.T) {
	mcp := &recordingMCP{connected: true, result: MCPCallResult{Text: "the exact agent answer"}}
	chat := &recordChat{}
	tool := newReadTool(Deps{MCP: mcp, Router: chat, Queue: fakeQueue{}})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1","maxLines":200}`))
	m := readResult(t, tool.Handle(context.Background(), decoded, &tools.ToolContext{}))
	if m["content"] != "the exact agent answer" {
		t.Fatalf("content not verbatim: %v", m["content"])
	}
	if chat.called {
		t.Fatal("terminal.read must not call the model")
	}
	if mcp.lastName != "terminal.getOutput" {
		t.Fatalf("called %q", mcp.lastName)
	}
	if mcp.lastArgs["maxLines"] != 200 || mcp.lastArgs["terminalId"] != "t1" {
		t.Fatalf("args not forwarded: %v", mcp.lastArgs)
	}
}

// terminal.read caps the returned text to the last tailBytes characters.
func TestReadCapsToTailBytes(t *testing.T) {
	mcp := &recordingMCP{connected: true, result: MCPCallResult{Text: "0123456789"}}
	tool := newReadTool(Deps{MCP: mcp, Router: &recordChat{}, Queue: fakeQueue{}})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1","tailBytes":4}`))
	m := readResult(t, tool.Handle(context.Background(), decoded, &tools.ToolContext{}))
	if m["content"] != "6789" {
		t.Fatalf("tailBytes cap wrong: %v", m["content"])
	}
}

// terminal.read surfaces a terminal.getOutput error as TERMINAL_OUTPUT.
func TestReadSurfacesGetOutputError(t *testing.T) {
	mcp := &recordingMCP{connected: true, result: MCPCallResult{IsError: true, Text: "boom"}}
	tool := newReadTool(Deps{MCP: mcp, Router: &recordChat{}, Queue: fakeQueue{}})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTerminalOutput {
		t.Fatalf("expected TERMINAL_OUTPUT, got %+v", res)
	}
}

// terminal.summarize relays the model body verbatim as the call summary.
func TestSummarizeNotFlaggedOnCleanFinish(t *testing.T) {
	deps := Deps{
		MCP:    &fakeMCP{connected: true, results: map[string]MCPCallResult{"terminal.getOutput": {Text: "output"}}},
		Router: fakeRouter{summary: "a summary"},
		Queue:  fakeQueue{},
	}
	tool := newSummarizeTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	m := readResult(t, res)
	if m["summary"] != "a summary" {
		t.Fatalf("result summary should be the body verbatim, got %v", m["summary"])
	}
	if res.Summary != "a summary" {
		t.Fatalf("summary should be the body verbatim, got %q", res.Summary)
	}
}
