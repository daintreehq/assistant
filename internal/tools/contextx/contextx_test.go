package contextx

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
	results   map[string]MCPCallResult
	errs      map[string]error
}

func (f *fakeMCP) Connected() bool   { return f.connected }
func (f *fakeMCP) Status() MCPStatus { return MCPStatus{Connected: f.connected} }
func (f *fakeMCP) CallTool(_ context.Context, name string, _ map[string]any) (MCPCallResult, error) {
	if f.errs != nil {
		if err := f.errs[name]; err != nil {
			return MCPCallResult{}, err
		}
	}
	return f.results[name], nil
}

type fakeRouter struct{ summary string }

func (f fakeRouter) Summarize(_ context.Context, _ string, _ string) (string, error) {
	return f.summary, nil
}

type fakeQueue struct{}

func (fakeQueue) Digest(_ domain.QueueDigestOptions) []domain.QueueEvent { return nil }
func (fakeQueue) Format(_ []domain.QueueEvent) string                    { return "" }

func TestSnapshotNeverThrowsWhenDisconnected(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: false}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newSnapshotTool(deps)
	res := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("snapshot must be ok even disconnected: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["actionContext"] != nil || m["worktrees"] != nil {
		t.Error("disconnected reads should be nil")
	}
}

// The backend now owns the summarizer prompt and any token cap, returning only the
// summary string. The CLI relays that body verbatim and — having lost the
// finishReason signal — always reports truncated=false.
func TestSummarizeRelaysModelBody(t *testing.T) {
	deps := Deps{
		MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
			"terminal.getOutput": {Text: "some long output"},
		}},
		Router: fakeRouter{summary: "the gist of it"},
		Queue:  fakeQueue{},
	}
	tool := newSummarizeTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("summarize failed: %+v", res.Error)
	}
	if res.Summary != "the gist of it" {
		t.Fatalf("summary should be the model body, got %q", res.Summary)
	}
	m := res.Result.(map[string]any)
	if m["truncated"].(bool) {
		t.Error("CLI no longer detects truncation; truncated must be false")
	}
}

func TestReadSurfacesMCPUnavailable(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: false}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("want MCP_UNAVAILABLE, got %+v", res)
	}
	// The disconnected read must point at /reconnect (issue #211).
	if !strings.Contains(res.Error.Message, "/reconnect") {
		t.Errorf("disconnected read hint must name /reconnect: %q", res.Error.Message)
	}
}

func TestReadStructuredContentFallback(t *testing.T) {
	// Scrollback in structuredContent.content is preferred over raw text.
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.getOutput": {Text: "raw", StructuredContent: map[string]any{"content": "structured tail"}},
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	m := res.Result.(map[string]any)
	if m["content"].(string) != "structured tail" {
		t.Errorf("structuredContent.content not preferred: %v", m["content"])
	}
}

// A "terminal not found" comes back IsError=false with an error JSON envelope in the text
// body. It must surface as a FAIL, not the old fake "Read 7 line(s)" success that handed
// the error JSON back as scrollback (the ses_f3fdeb08 bug).
func TestReadSurfacesTerminalNotFound(t *testing.T) {
	notFound := `{"terminalId":"terminal-x","content":null,"lineCount":0,"truncated":false,"error":"Terminal not found or has no output"}`
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.getOutput": {Text: notFound}, // terminal.list unset ⇒ resolution fails open
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-x"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok {
		t.Fatalf("a not-found terminal must fail, not return a fake success: %+v", res.Result)
	}
	if res.Error == nil || res.Error.Code != codeTerminalOutput {
		t.Fatalf("want %s, got %+v", codeTerminalOutput, res.Error)
	}
	if !strings.Contains(res.Error.Message, "Terminal not found") {
		t.Fatalf("the failure should surface Daintree's not-found message, got %q", res.Error.Message)
	}
}

// A truncated/prefix id resolves to the canonical id via terminal.list; the result reports
// the full id and reads the resolved terminal's output.
func TestReadResolvesPrefixID(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.list":      {Text: `{"terminals":[{"id":"` + full + `"}]}`},
		"terminal.getOutput": {StructuredContent: map[string]any{"content": "the tail"}},
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-5284bfef"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("a resolvable prefix should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["terminalId"] != full {
		t.Fatalf("read should report the canonical id %q, got %v", full, m["terminalId"])
	}
	if m["content"].(string) != "the tail" {
		t.Fatalf("content should be the resolved terminal's tail, got %v", m["content"])
	}
}

// An id matching no live terminal (roster live & non-empty) fails fast with the live list.
func TestReadUnknownIDFailsFast(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.list": {Text: `{"terminals":[{"id":"` + full + `"}]}`},
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-nope"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok {
		t.Fatalf("an unknown id must fail fast, got %+v", res.Result)
	}
	if res.Error == nil || res.Error.Code != codeTerminalNotFound {
		t.Fatalf("want %s, got %+v", codeTerminalNotFound, res.Error)
	}
	if !strings.Contains(res.Error.Message, full) {
		t.Fatalf("the failure should name the live id, got %q", res.Error.Message)
	}
}

// An ambiguous prefix (matches >1 live terminal) fails fast rather than silently picking
// one — the model must pass the full id.
func TestReadAmbiguousPrefixFailsFast(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.list": {Text: `{"terminals":[{"id":"terminal-abc-1"},{"id":"terminal-abd-2"}]}`},
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-ab"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok || res.Error == nil || res.Error.Code != codeTerminalNotFound {
		t.Fatalf("an ambiguous prefix must fail fast with %s, got %+v", codeTerminalNotFound, res)
	}
	if !strings.Contains(res.Error.Message, "ambiguous") {
		t.Fatalf("the failure should explain the prefix is ambiguous, got %q", res.Error.Message)
	}
}

// terminal.summarize resolves a truncated prefix the same way and reports the canonical id.
func TestSummarizeResolvesPrefixID(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.list":      {Text: `{"terminals":[{"id":"` + full + `"}]}`},
		"terminal.getOutput": {StructuredContent: map[string]any{"content": "some output"}},
	}}, Router: fakeRouter{summary: "the gist"}, Queue: fakeQueue{}}
	tool := newSummarizeTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-5284bfef"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("a resolvable prefix should summarize, got %+v", res.Error)
	}
	if m := res.Result.(map[string]any); m["terminalId"] != full {
		t.Fatalf("summarize should report the canonical id %q, got %v", full, m["terminalId"])
	}
}

// VERBATIM-read guard: real scrollback that merely happens to be a JSON object with an
// "error" key (but NOT the getOutput envelope shape) must be returned as content, never
// misclassified as a not-found error. terminal-x is non-canonical so resolution runs, but
// terminal.list is unset ⇒ fail open ⇒ the read proceeds and returns the literal text.
func TestReadKeepsJSONScrollbackWithoutEnvelope(t *testing.T) {
	scrollback := `{"error":"boom","content":null}`
	deps := Deps{MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
		"terminal.getOutput": {Text: scrollback},
	}}, Router: fakeRouter{}, Queue: fakeQueue{}}
	tool := newReadTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"terminal-x"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("arbitrary JSON scrollback must NOT be misread as a not-found error: %+v", res.Error)
	}
	if c := res.Result.(map[string]any)["content"].(string); c != scrollback {
		t.Fatalf("scrollback must be returned verbatim, got %q", c)
	}
}
