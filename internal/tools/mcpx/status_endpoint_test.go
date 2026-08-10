package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// "Which URL are you connected to?" used to be unanswerable: daintree.status reported
// transport + tool count and nothing else, so the model guessed at a plausible
// localhost endpoint (ses_8cb40b4e). The endpoint must reach the SUMMARY the model
// reads, not just the structured result.
func TestStatusSummaryNamesEndpoint(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: true, transport: "streamable-http", url: "http://127.0.0.1:45454/mcp"}}
	res := newStatusTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("status must succeed: %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "http://127.0.0.1:45454/mcp") {
		t.Errorf("summary must name the endpoint, got %q", res.Summary)
	}
	st, ok := res.Result.(MCPStatus)
	if !ok || st.URL != "http://127.0.0.1:45454/mcp" {
		t.Errorf("structured result must carry the endpoint, got %+v", res.Result)
	}
}

// A DOWN link is exactly when "which server is it even trying?" matters most, so the
// endpoint must survive the disconnected branch too.
func TestStatusSummaryNamesEndpointWhenDisconnected(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: false, url: "http://127.0.0.1:45454/mcp", statusErr: "connection refused"}}
	res := newStatusTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("status must never fail on a broken link: %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "http://127.0.0.1:45454/mcp") || !strings.Contains(res.Summary, "connection refused") {
		t.Errorf("disconnected summary must carry endpoint + reason, got %q", res.Summary)
	}
}

// With no endpoint configured at all the summary must stay clean — no dangling " at ".
func TestStatusSummaryOmitsBlankEndpoint(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: true, transport: "streamable-http"}}
	res := newStatusTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	// Assert Ok first: a zero-value failed result has an empty summary, which would
	// satisfy the "no dangling at" check for the wrong reason.
	if !res.Ok {
		t.Fatalf("status must succeed: %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "streamable-http") {
		t.Fatalf("summary lost its content: %q", res.Summary)
	}
	if strings.Contains(res.Summary, " at ") {
		t.Errorf("blank endpoint must not render, got %q", res.Summary)
	}
}
