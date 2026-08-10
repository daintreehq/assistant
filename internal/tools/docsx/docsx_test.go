package docsx

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/tools"
)

// fakeDocsMCP records the last forwarded call and returns a canned envelope. It is the
// docs-family seam: the docs tools reach it through Deps (closure capture), not
// ToolContext, so tests inject it via Tools(Deps{MCP: fake}).
type fakeDocsMCP struct {
	connected bool
	lastName  string
	lastArgs  map[string]any
	result    tools.MCPCallResult
	err       error
}

func (f *fakeDocsMCP) Connected() bool { return f.connected }
func (f *fakeDocsMCP) CallTool(_ context.Context, name string, args map[string]any) (tools.MCPCallResult, error) {
	f.lastName = name
	f.lastArgs = args
	return f.result, f.err
}

func findTool(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// run decodes + handles a docs tool against the fake, returning its result.
func run(t *testing.T, f *fakeDocsMCP, name, args string) tools.ToolResult {
	t.Helper()
	tool := findTool(Tools(Deps{MCP: f}), name)
	if tool == nil {
		t.Fatalf("%s not registered", name)
	}
	parsed, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		// A decode error mirrors what the registry would surface as INVALID_ARGS; return
		// it as a Fail so the caller can assert on the code uniformly.
		return tools.Fail(codeInvalidArgs, err.Error())
	}
	return tool.Handle(context.Background(), parsed, &tools.ToolContext{})
}

// docs.search forwards the trimmed query plus the optional topK/pathPrefix, targets the
// docs MCP "search" tool, and surfaces the returned text to the model.
func TestSearchForwards(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: `{"results":[]}`}}
	res := run(t, f, "docs.search", `{"query":"  how to create a worktree  ","topK":5,"pathPrefix":"/docs"}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if f.lastName != "search" {
		t.Fatalf("forwarded to %q, want search", f.lastName)
	}
	if got := f.lastArgs["query"]; got != "how to create a worktree" {
		t.Fatalf("query not trimmed/forwarded: %v", got)
	}
	if got := f.lastArgs["topK"]; got != 5 {
		t.Fatalf("topK not forwarded: %v", got)
	}
	if got := f.lastArgs["pathPrefix"]; got != "/docs" {
		t.Fatalf("pathPrefix not forwarded: %v", got)
	}
	result, _ := res.Result.(map[string]any)
	if result["text"] != `{"results":[]}` {
		t.Fatalf("MCP text not surfaced: %+v", result)
	}
}

// Optional fields are omitted from the forwarded args when unset (so the server applies
// its own defaults) — only query is sent.
func TestSearchOmitsUnsetOptionals(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	res := run(t, f, "docs.search", `{"query":"keybindings"}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if _, ok := f.lastArgs["topK"]; ok {
		t.Fatalf("topK should be omitted when unset: %+v", f.lastArgs)
	}
	if _, ok := f.lastArgs["pathPrefix"]; ok {
		t.Fatalf("pathPrefix should be omitted when unset: %+v", f.lastArgs)
	}
}

// A blank/whitespace query is rejected as INVALID_ARGS before any MCP call.
func TestSearchRequiresQuery(t *testing.T) {
	f := &fakeDocsMCP{connected: true}
	res := run(t, f, "docs.search", `{"query":"   "}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
	if f.lastName != "" {
		t.Fatalf("MCP should not be called on blank query, got %q", f.lastName)
	}
}

// Unknown fields are rejected (strict decode) so the model can't smuggle a typo'd arg.
func TestSearchRejectsUnknownField(t *testing.T) {
	f := &fakeDocsMCP{connected: true}
	res := run(t, f, "docs.search", `{"query":"x","limit":3}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for unknown field, got %+v", res)
	}
}

// A disconnected docs MCP fails cleanly with MCP_UNAVAILABLE (never a panic).
func TestSearchDisconnected(t *testing.T) {
	f := &fakeDocsMCP{connected: false}
	res := run(t, f, "docs.search", `{"query":"x"}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("expected MCP_UNAVAILABLE, got %+v", res)
	}
}

// A tool-level error result (IsError) maps to MCP_TOOL_ERROR with the server text carried
// into details so the model sees why it failed.
func TestSearchToolError(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "rate limited"}}
	res := run(t, f, "docs.search", `{"query":"x"}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeMCPToolError {
		t.Fatalf("expected MCP_TOOL_ERROR, got %+v", res)
	}
}

// A transport error (not a tool-level IsError) also surfaces as MCP_TOOL_ERROR.
func TestSearchTransportError(t *testing.T) {
	f := &fakeDocsMCP{connected: true, err: errors.New("boom")}
	res := run(t, f, "docs.search", `{"query":"x"}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeMCPToolError {
		t.Fatalf("expected MCP_TOOL_ERROR, got %+v", res)
	}
}

// docs.getPage forwards the trimmed path to get_page.
func TestGetPageForwards(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: "# Worktrees"}}
	res := run(t, f, "docs.getPage", `{"path":" /docs/worktrees "}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if f.lastName != "get_page" {
		t.Fatalf("forwarded to %q, want get_page", f.lastName)
	}
	if got := f.lastArgs["path"]; got != "/docs/worktrees" {
		t.Fatalf("path not trimmed/forwarded: %v", got)
	}
}

func TestGetPageRequiresPath(t *testing.T) {
	f := &fakeDocsMCP{connected: true}
	res := run(t, f, "docs.getPage", `{"path":"  "}`)
	if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
}

// docs.getRelatedPages forwards path + optional topK to get_related_pages.
func TestGetRelatedPagesForwards(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: `{"related":[]}`}}
	res := run(t, f, "docs.getRelatedPages", `{"path":"/docs/worktrees","topK":3}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if f.lastName != "get_related_pages" {
		t.Fatalf("forwarded to %q, want get_related_pages", f.lastName)
	}
	if got := f.lastArgs["path"]; got != "/docs/worktrees" {
		t.Fatalf("path not forwarded: %v", got)
	}
	if got := f.lastArgs["topK"]; got != 3 {
		t.Fatalf("topK not forwarded: %v", got)
	}
}

// An out-of-range topK is CLAMPED to the server's ceiling (not bounced), <=0 is omitted.
func TestTopKClamping(t *testing.T) {
	cases := []struct {
		tool, args string
		wantTopK   any // nil ⇒ must be omitted
	}{
		{"docs.search", `{"query":"x","topK":500}`, 100},        // search ceiling 100
		{"docs.search", `{"query":"x","topK":50}`, 50},          // in range
		{"docs.search", `{"query":"x","topK":0}`, nil},          // <=0 omitted
		{"docs.search", `{"query":"x","topK":-3}`, nil},         // negative omitted
		{"docs.getRelatedPages", `{"path":"/a","topK":99}`, 25}, // related ceiling 25
		{"docs.getRelatedPages", `{"path":"/a","topK":10}`, 10},
		{"docs.getRelatedPages", `{"path":"/a","topK":0}`, nil},
	}
	for _, tc := range cases {
		f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
		res := run(t, f, tc.tool, tc.args)
		if !res.Ok {
			t.Fatalf("%s %s: expected ok, got %+v", tc.tool, tc.args, res.Error)
		}
		got, present := f.lastArgs["topK"]
		if tc.wantTopK == nil {
			if present {
				t.Errorf("%s %s: topK should be omitted, got %v", tc.tool, tc.args, got)
			}
			continue
		}
		if got != tc.wantTopK {
			t.Errorf("%s %s: topK = %v, want %v", tc.tool, tc.args, got, tc.wantTopK)
		}
	}
}

// A completely missing required field (not just blank) is still rejected as INVALID_ARGS,
// since StrictDecoder does not enforce JSON-schema "required" — the handler's check does.
func TestMissingRequiredField(t *testing.T) {
	f := &fakeDocsMCP{connected: true}
	if res := run(t, f, "docs.search", `{}`); res.Ok || res.Error.Code != codeInvalidArgs {
		t.Errorf("docs.search {}: want INVALID_ARGS, got %+v", res)
	}
	if res := run(t, f, "docs.getPage", `{}`); res.Ok || res.Error.Code != codeInvalidArgs {
		t.Errorf("docs.getPage {}: want INVALID_ARGS, got %+v", res)
	}
	if f.lastName != "" {
		t.Errorf("no MCP call should happen on missing field, got %q", f.lastName)
	}
}

// Unknown fields are rejected for getPage and getRelatedPages too (not just search).
func TestUnknownFieldRejectedAllTools(t *testing.T) {
	f := &fakeDocsMCP{connected: true}
	if res := run(t, f, "docs.getPage", `{"path":"/a","depth":2}`); res.Ok || res.Error.Code != codeInvalidArgs {
		t.Errorf("docs.getPage unknown field: want INVALID_ARGS, got %+v", res)
	}
	if res := run(t, f, "docs.getRelatedPages", `{"path":"/a","kind":"x"}`); res.Ok || res.Error.Code != codeInvalidArgs {
		t.Errorf("docs.getRelatedPages unknown field: want INVALID_ARGS, got %+v", res)
	}
}

// A nil docs client (family wired with no MCP) fails cleanly with MCP_UNAVAILABLE — never
// a nil-pointer panic.
func TestNilMCPClient(t *testing.T) {
	tool := findTool(Tools(Deps{MCP: nil}), "docs.search")
	parsed, err := tool.Decode(json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, &tools.ToolContext{})
	if res.Ok || res.Error == nil || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("nil MCP: want MCP_UNAVAILABLE, got %+v", res)
	}
}

// A cancelled turn (ctx done + the call errors) maps to an UNRECOVERABLE CANCELLED, not a
// generic tool error.
func TestCancellationUnrecoverable(t *testing.T) {
	f := &fakeDocsMCP{connected: true, err: context.Canceled}
	tool := findTool(Tools(Deps{MCP: f}), "docs.search")
	parsed, _ := tool.Decode(json.RawMessage(`{"query":"x"}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := tool.Handle(ctx, parsed, &tools.ToolContext{})
	if res.Ok || res.Error == nil || res.Error.Code != codeCancelled {
		t.Fatalf("want CANCELLED, got %+v", res)
	}
	if res.Error.Recoverable {
		t.Error("CANCELLED must be unrecoverable")
	}
}

// An IsError result carries the server text + structuredContent into the failure details
// so the model can see why it failed.
func TestErrorDetailsCarried(t *testing.T) {
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "nope", StructuredContent: map[string]any{"code": "X"}}}
	res := run(t, f, "docs.search", `{"query":"x"}`)
	if res.Ok || res.Error == nil {
		t.Fatalf("want failure, got %+v", res)
	}
	details, _ := res.Error.Details.(map[string]any)
	if details["rawText"] != "nope" {
		t.Errorf("rawText not carried into details: %+v", details)
	}
	if details["structuredContent"] == nil {
		t.Errorf("structuredContent not carried into details: %+v", details)
	}
}

// A successful call preserves structuredContent (not just text) in the result payload.
func TestSuccessPreservesStructuredContent(t *testing.T) {
	sc := map[string]any{"results": []any{}}
	f := &fakeDocsMCP{connected: true, result: tools.MCPCallResult{Text: "{}", StructuredContent: sc}}
	res := run(t, f, "docs.search", `{"query":"x"}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	result, _ := res.Result.(map[string]any)
	if result["structuredContent"] == nil {
		t.Errorf("structuredContent dropped from success result: %+v", result)
	}
}

// Every docs tool is risk read (no confirmation, reachable at any tier) and passes the
// no-file-edit guard — these answer help questions, they never mutate Daintree.
func TestDocsToolsAreSafeReads(t *testing.T) {
	ts := Tools(Deps{})
	if len(ts) != 3 {
		t.Fatalf("want 3 docs tools, got %d", len(ts))
	}
	for _, tool := range ts {
		if tool.Risk != domain.RiskRead {
			t.Errorf("%s risk = %v, want read", tool.Name, tool.Risk)
		}
		if safety.IsForbiddenToolName(tool.Name) {
			t.Errorf("%s is a forbidden (file-edit) tool name", tool.Name)
		}
	}
}
