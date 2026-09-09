package contextx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

type truncatedReviewRouter struct{}

func (truncatedReviewRouter) Summarize(context.Context, string, string) (string, bool, error) {
	return "What I'll ship is", true, nil
}

func TestSummaryTruncationVisibleToSupervisor(t *testing.T) {
	tool := newSummarizeTool(Deps{
		MCP:    &fakeMCP{connected: true, results: map[string]MCPCallResult{"terminal.getOutput": {Text: "What I'll ship is the fix. Taking this on myself."}}},
		Router: truncatedReviewRouter{},
	})
	res := tool.Handle(context.Background(), json.RawMessage(`{"terminalId":"t1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatal(res.Error)
	}
	result := res.Result.(map[string]any)
	if result["truncated"] != true || !strings.Contains(res.Summary, "unknown, not absent") || !strings.Contains(res.Summary, "terminal.read") {
		t.Fatalf("cut summary lacks evidence warning: %+v", res)
	}
	if result["summary"] != "What I'll ship is" {
		t.Fatal("partial evidence must remain available")
	}
}
