package contextx

import (
	"context"
	"encoding/json"
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

type fakeRouter struct{ res ChatResult }

func (f fakeRouter) Chat(_ context.Context, _ domain.ModelTier, _ []ChatMessage, _ int) (ChatResult, error) {
	return f.res, nil
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

func TestSummarizeTruncationWarning(t *testing.T) {
	deps := Deps{
		MCP: &fakeMCP{connected: true, results: map[string]MCPCallResult{
			"terminal.getOutput": {Text: "some long output"},
		}},
		Router: fakeRouter{res: ChatResult{Content: "partial summary", FinishReason: "length"}},
		Queue:  fakeQueue{},
	}
	tool := newSummarizeTool(deps)
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("summarize failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if !m["truncated"].(bool) {
		t.Error("length finishReason should set truncated=true")
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
