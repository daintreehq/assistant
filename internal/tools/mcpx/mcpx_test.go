package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

type fakeMCP struct {
	connected bool
	result    MCPCallResult
	lastName  string
	lastArgs  map[string]any
	listErr   error           // when set, ListTools returns it (a stale connection that drops mid-RPC)
	toolList  []MCPToolInfo   // when set, ListTools returns it (tool.search / daintree.listTools tests)
	callCount int             // total CallTool invocations (batch wrappers loop internally)
	failOn    map[string]bool // terminalId → return an IsError result (close-batch tests)
	// disconnectAfter > 0 makes Connected() flip false once callCount reaches it, so a
	// batch wrapper can be tested against a link that drops PART-WAY through the loop.
	disconnectAfter int
	url             string // endpoint reported by Status()
	transport       string
	statusErr       string
	// resultsByName routes a per-MCP-tool result, so a batching wrapper that also
	// READS (terminal.moveToWorktree resolving ids through terminal.list) can be given
	// a roster and a void move result in the same fake. Falls back to `result` for any
	// name it does not carry, keeping every pre-existing single-result test unchanged.
	resultsByName map[string]MCPCallResult
	errsByName    map[string]error
	// onCall fires at the top of every CallTool, so a test can mutate the world
	// mid-flight — cancelling the context between the roster read and the mutation.
	onCall func()
	// calls is the ORDERED call log (name + a copy of the args). lastName/lastArgs only
	// remember the final call, which cannot distinguish "listed then moved three" from
	// "moved three".
	calls []fakeMCPCall
	// listCount / lastListForce record ListTools traffic so a test can prove
	// tool.schema reads the WARM cache (force=false) and never probes the network.
	listCount     int
	lastListForce bool
}

// fakeMCPCall is one recorded CallTool invocation.
type fakeMCPCall struct {
	name string
	args map[string]any
}

func (f *fakeMCP) Connected() bool {
	if f.disconnectAfter > 0 && f.callCount >= f.disconnectAfter {
		return false
	}
	return f.connected
}
func (f *fakeMCP) Status() MCPStatus {
	return MCPStatus{Connected: f.connected, Transport: f.transport, URL: f.url, Error: f.statusErr}
}
func (f *fakeMCP) CallTool(_ context.Context, name string, args map[string]any) (MCPCallResult, error) {
	if f.onCall != nil {
		f.onCall()
	}
	f.lastName = name
	f.lastArgs = args
	f.callCount++
	argsCopy := make(map[string]any, len(args))
	for k, v := range args {
		argsCopy[k] = v
	}
	f.calls = append(f.calls, fakeMCPCall{name: name, args: argsCopy})
	if err, ok := f.errsByName[name]; ok && err != nil {
		return MCPCallResult{}, err
	}
	if f.failOn != nil {
		if id, _ := args["terminalId"].(string); f.failOn[id] {
			return MCPCallResult{IsError: true, Text: "no such terminal"}, nil
		}
	}
	if res, ok := f.resultsByName[name]; ok {
		return res, nil
	}
	return f.result, nil
}

// callsTo returns the ordered args of every recorded call to one MCP tool.
func (f *fakeMCP) callsTo(name string) []map[string]any {
	var out []map[string]any
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c.args)
		}
	}
	return out
}
func (f *fakeMCP) ListTools(_ context.Context, force bool) ([]MCPToolInfo, error) {
	f.listCount++
	f.lastListForce = force
	return f.toolList, f.listErr
}

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

func TestTerminalCloseBatchClosesAllInOneCall(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "trashed"}}
	tool := newTerminalCloseTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(`{"terminalIds":["t1","t2","t3"]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	// ONE wrapper call (so ONE confirmation) fans out to one MCP call per id, because
	// the Daintree terminal.close MCP tool takes a single terminalId.
	if mcp.callCount != 3 {
		t.Fatalf("expected 3 terminal.close calls, got %d", mcp.callCount)
	}
	if mcp.lastName != "terminal.close" {
		t.Fatalf("expected terminal.close, got %q", mcp.lastName)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if !strings.Contains(res.Summary, id) {
			t.Errorf("summary should name closed id %q: %q", id, res.Summary)
		}
	}
	// The Ok result carries the closed ids in order for the caller to record.
	result, _ := res.Result.(map[string]any)
	cl, _ := result["closed"].([]string)
	if len(cl) != 3 || cl[0] != "t1" || cl[2] != "t3" {
		t.Fatalf("ok result should carry [t1,t2,t3] in order, got %v", result["closed"])
	}
}

func TestTerminalCloseAcceptsSingularIdAndTrims(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "trashed"}}
	tool := newTerminalCloseTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"  t1  "}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if mcp.callCount != 1 || mcp.lastArgs["terminalId"] != "t1" {
		t.Fatalf("expected one trimmed close of t1, got count=%d args=%v", mcp.callCount, mcp.lastArgs)
	}
}

func TestTerminalCloseDedupesAcrossBothFields(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "trashed"}}
	tool := newTerminalCloseTool(Deps{MCP: mcp})
	// singular + plural overlap, plus a duplicate and a blank — one close per UNIQUE id.
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1","terminalIds":["t1","t2","  "]}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if mcp.callCount != 2 {
		t.Fatalf("expected 2 unique closes (t1,t2), got %d", mcp.callCount)
	}
}

func TestTerminalCloseRejectsEmptyAtDecodeBeforeConfirm(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	tool := newTerminalCloseTool(Deps{MCP: mcp})
	// Decode runs BEFORE the confirmation prompt in dispatch, so an empty/blank call must
	// fail at decode (no spurious "confirm a close" prompt for a call that closes nothing).
	for _, args := range []string{`{}`, `{"terminalIds":[]}`, `{"terminalId":"   "}`} {
		if _, err := tool.Decode(json.RawMessage(args)); err == nil {
			t.Errorf("%s should be rejected at decode (no id), got no error", args)
		}
	}
	if mcp.callCount != 0 {
		t.Fatalf("a no-id call must never reach the MCP; got %d calls", mcp.callCount)
	}
}

func TestTerminalCloseReportsPartialFailureFaithfully(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "trashed"}, failOn: map[string]bool{"t2": true}}
	tool := newTerminalCloseTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalIds":["t1","t2","t3"]}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if res.Ok {
		t.Fatalf("a partial failure must NOT report ok: %+v", res)
	}
	if res.Error.Code != codeMCPToolError {
		t.Fatalf("expected MCP_TOOL_ERROR, got %q", res.Error.Code)
	}
	// A single tool-level error does NOT abort the batch — the other two are still tried.
	if mcp.callCount != 3 {
		t.Fatalf("expected 3 attempts, got %d", mcp.callCount)
	}
	if !strings.Contains(res.Error.Message, "t2") {
		t.Errorf("failure must name the unclosed id t2: %q", res.Error.Message)
	}
	// Exact contents + order: t2 errored, t1 and t3 still closed in order.
	details, _ := res.Error.Details.(map[string]any)
	closed, _ := details["closed"].([]string)
	failed, _ := details["failed"].([]string)
	if len(closed) != 2 || closed[0] != "t1" || closed[1] != "t3" {
		t.Fatalf("expected closed=[t1,t3] in order, got %v", closed)
	}
	if len(failed) != 1 || failed[0] != "t2" {
		t.Fatalf("expected failed=[t2], got %v", failed)
	}
	// Nothing was skipped (a per-id error does not abort), so no notAttempted list.
	if _, ok := details["notAttempted"]; ok {
		t.Errorf("a per-id error must not skip the rest of the batch: %v", details["notAttempted"])
	}
}

func TestTerminalCloseAbortsBatchWhenMCPGone(t *testing.T) {
	mcp := &fakeMCP{connected: false}
	// A disconnected link fails on the first id; the remaining ids must NOT be looped.
	res := terminalClosePassthrough(context.Background(), mcp, []string{"t1", "t2", "t3"})
	// The abort PRESERVES the MCP_UNAVAILABLE code so the model gets the /reconnect hint,
	// rather than flattening it to a generic MCP_TOOL_ERROR.
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("disconnected close should preserve MCP_UNAVAILABLE, got %+v", res)
	}
	if mcp.callCount != 0 {
		t.Fatalf("disconnected passthrough short-circuits before CallTool; got %d calls", mcp.callCount)
	}
	if !strings.Contains(res.Error.Message, "/reconnect") {
		t.Errorf("the disconnected hint must reach the model: %q", res.Error.Message)
	}
	// Every id is named as unclosed — none silently dropped from the report.
	for _, id := range []string{"t1", "t2", "t3"} {
		if !strings.Contains(res.Error.Message, id) {
			t.Errorf("every unclosed id must be named; missing %q in %q", id, res.Error.Message)
		}
	}
}

// A link that drops PART-WAY through the batch: the already-closed id is reported, the
// remaining ones are recorded as not-attempted (never silently lost), and the abort's
// MCP_UNAVAILABLE code is preserved.
func TestTerminalCloseAbortsMidBatchPreservingCode(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "trashed"}, disconnectAfter: 1}
	res := terminalClosePassthrough(context.Background(), mcp, []string{"t1", "t2", "t3"})
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("a mid-batch disconnect must fail with the preserved MCP_UNAVAILABLE, got %+v", res)
	}
	if mcp.callCount != 1 {
		t.Fatalf("expected exactly 1 close before the link dropped, got %d", mcp.callCount)
	}
	details, _ := res.Error.Details.(map[string]any)
	closed, _ := details["closed"].([]string)
	notAttempted, _ := details["notAttempted"].([]string)
	if len(closed) != 1 || closed[0] != "t1" {
		t.Fatalf("expected closed=[t1], got %v", closed)
	}
	// t2 errored on the dead link; t3 was never attempted.
	if len(notAttempted) != 1 || notAttempted[0] != "t3" {
		t.Fatalf("expected notAttempted=[t3], got %v", notAttempted)
	}
	if !strings.Contains(res.Error.Message, "t2") || !strings.Contains(res.Error.Message, "t3") {
		t.Errorf("message must name both unclosed ids t2 and t3: %q", res.Error.Message)
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
	// Exotic Unicode whitespace (U+2028 LINE SEPARATOR, vertical tab) collapses
	// the same way, so no separator can sneak a line break into the summary.
	if got := truncateCommand("a b\vc", 80); got != "a b c" {
		t.Errorf("unicode whitespace not collapsed: %q", got)
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
	// Both args reach Daintree verbatim — a wrapper that dropped command would
	// otherwise still pass the name/summary assertions above.
	if mcp.lastArgs["terminalId"] != "t7" || mcp.lastArgs["command"] != "go test ./..." {
		t.Errorf("args not forwarded verbatim: %v", mcp.lastArgs)
	}
	// The original result payload is preserved in full (text + structuredContent),
	// not discarded when the summary is overridden.
	rm, _ := res.Result.(map[string]any)
	if rm["text"] != "ok" {
		t.Errorf("result text not preserved: %v", rm["text"])
	}
	if sc, _ := rm["structuredContent"].(map[string]any); sc == nil || sc["k"] != "v" {
		t.Errorf("structuredContent not preserved: %v", rm["structuredContent"])
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

	// A connected MCP that reports a tool-level error (IsError) must also surface as
	// a failure, never a fabricated "Sent to terminal" success.
	mcpErr := &fakeMCP{connected: true, result: MCPCallResult{IsError: true, Text: "terminal not found"}}
	resErr := terminalSendCommandPassthrough(context.Background(), mcpErr, "t1", "ls",
		map[string]any{"terminalId": "t1", "command": "ls"})
	if resErr.Ok || resErr.Error.Code != codeMCPToolError {
		t.Fatalf("Daintree-reported error should fail: %+v", resErr)
	}
	if strings.Contains(resErr.Summary, "Sent to terminal") {
		t.Errorf("tool-error send must not claim delivery: %q", resErr.Summary)
	}
}

// The runbooks tell the model to label every copy-tree run, and `name` is a
// TOP-LEVEL sibling of worktreeId/options on the Daintree action rather than an
// option key. These wrappers decode strictly, so a missing schema property is not
// ignored — it fails the call outright, which is how a documented, harmless label
// turned into a guaranteed wasted round trip on the first generate of every
// bundle. Both directions are pinned: the field decodes, and it reaches MCP.
func TestCopyTreeRunNameIsForwarded(t *testing.T) {
	for _, tc := range []struct {
		label string
		tool  func(Deps) tools.Tool
		args  string
	}{
		{"generate", newCopyTreeGenerateTool, `{"worktreeId":"wt-1","name":"auth flow context"}`},
		{"generateAndCopyFile", newCopyTreeGenerateAndCopyFileTool, `{"worktreeId":"wt-1","name":"auth flow context"}`},
		{"injectToTerminal", newCopyTreeInjectTool, `{"terminalId":"t1","name":"auth flow context"}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := tc.tool(Deps{MCP: mcp})
			decoded, err := tool.Decode(json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("name must decode, got %v", err)
			}
			if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if mcp.lastArgs["name"] != "auth flow context" {
				t.Errorf("name not forwarded: %v", mcp.lastArgs)
			}
		})
	}

	// Blank is absent, not an empty label: Daintree treats whitespace as unnamed
	// and derives a fallback, so forwarding "" would only be noise on the wire.
	// Padding around a real label is trimmed rather than dropped. Both generate
	// and inject are covered — inject builds its own map, so a fix to one says
	// nothing about the other.
	for _, tc := range []struct {
		label string
		tool  func(Deps) tools.Tool
		base  string
	}{
		{"generate", newCopyTreeGenerateTool, `"worktreeId":"wt-1"`},
		{"injectToTerminal", newCopyTreeInjectTool, `"terminalId":"t1"`},
	} {
		t.Run(tc.label+"/blank", func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := tc.tool(Deps{MCP: mcp})
			decoded, err := tool.Decode(json.RawMessage(`{` + tc.base + `,"name":"   "}`))
			if err != nil {
				t.Fatalf("blank name must decode, got %v", err)
			}
			if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if _, present := mcp.lastArgs["name"]; present {
				t.Errorf("blank name should be dropped, got %v", mcp.lastArgs)
			}
		})

		t.Run(tc.label+"/padded", func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := tc.tool(Deps{MCP: mcp})
			decoded, err := tool.Decode(json.RawMessage(`{` + tc.base + `,"name":"  auth flow context  "}`))
			if err != nil {
				t.Fatalf("padded name must decode, got %v", err)
			}
			if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if mcp.lastArgs["name"] != "auth flow context" {
				t.Errorf("padded name should be trimmed, got %v", mcp.lastArgs["name"])
			}
		})
	}
}

// The struct field and the ADVERTISED schema are separate plumbing: dropping
// `properties.name` would stop the wire inventory offering the field while every
// decode/forward assertion above stayed green, and the model would go back to
// never sending a label. Pin the schema itself, `additionalProperties:false`
// included — the strictness is what made the missing property fatal rather than
// merely ignored.
func TestCopyTreeSchemasAdvertiseName(t *testing.T) {
	for label, raw := range map[string]json.RawMessage{
		"generate": copyTreeGenerateSchema,
		"inject":   copyTreeInjectSchema,
	} {
		var schema struct {
			AdditionalProperties bool `json:"additionalProperties"`
			Properties           map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", label, err)
		}
		if schema.AdditionalProperties {
			t.Errorf("%s: schema must stay strict (additionalProperties:false)", label)
		}
		if got := schema.Properties["name"].Type; got != "string" {
			t.Errorf("%s: schema must advertise a string `name`, got %q", label, got)
		}
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

	// Optional worktreeId + options reach Daintree verbatim — guards against the
	// wrapper's map-building dropping the optional fields.
	mcpOpt := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	toolOpt := newCopyTreeInjectTool(Deps{MCP: mcpOpt})
	dOpt, _ := toolOpt.Decode(json.RawMessage(`{"terminalId":"t1","worktreeId":"wt-prod","options":{"depth":2}}`))
	rOpt := toolOpt.Handle(context.Background(), dOpt, &tools.ToolContext{})
	if !rOpt.Ok {
		t.Fatalf("expected ok with options, got %+v", rOpt.Error)
	}
	if mcpOpt.lastArgs["worktreeId"] != "wt-prod" {
		t.Errorf("worktreeId not forwarded: %v", mcpOpt.lastArgs)
	}
	if opts, _ := mcpOpt.lastArgs["options"].(map[string]any); opts == nil || opts["depth"] != float64(2) {
		t.Errorf("options not forwarded: %v", mcpOpt.lastArgs["options"])
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

	// A Daintree-reported tool error (connected, IsError) likewise must not be
	// dressed up as a successful injection.
	mcp4 := &fakeMCP{connected: true, result: MCPCallResult{IsError: true, Text: "terminal not found"}}
	r4 := copyTreeInjectPassthrough(context.Background(), mcp4, "t1", map[string]any{"terminalId": "t1"})
	if r4.Ok || r4.Error.Code != codeMCPToolError || strings.Contains(r4.Summary, "Injected copy tree") {
		t.Errorf("tool-error inject must not claim delivery: %+v", r4)
	}
}

// recordObserver captures MarkCommandSent calls (the settle-memory invalidation
// hook the send wrappers feed).
type recordObserver struct {
	marked []string
}

func (r *recordObserver) MarkCommandSent(terminalID string, _ int64) {
	r.marked = append(r.marked, terminalID)
}

// Every ATTEMPTED send/inject marks the terminal in the settle memory
// (invalidating cross-call "seen working" evidence) — including failed calls: an
// ambiguous transport failure may still have delivered the input, so the mark
// cannot wait for a confirmed success. Only a local validation reject (nothing
// ever sent) skips the mark.
func TestSendWrappersMarkCommandSentOnAttempt(t *testing.T) {
	obs := &recordObserver{}
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newTerminalSendCommandTool(Deps{MCP: mcp, Observer: obs})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t7","command":"ls"}`))
	if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("send should succeed: %+v", res.Error)
	}
	if len(obs.marked) != 1 || obs.marked[0] != "t7" {
		t.Fatalf("successful send must mark the terminal, got %v", obs.marked)
	}

	inject := newCopyTreeInjectTool(Deps{MCP: mcp, Observer: obs})
	d2, _ := inject.Decode(json.RawMessage(`{"terminalId":"t8"}`))
	if res := inject.Handle(context.Background(), d2, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("inject should succeed: %+v", res.Error)
	}
	if len(obs.marked) != 2 || obs.marked[1] != "t8" {
		t.Fatalf("successful inject must mark the terminal, got %v", obs.marked)
	}

	// A failed call still marks (the delivery is ambiguous from here).
	obs2 := &recordObserver{}
	failMCP := &fakeMCP{connected: true, result: MCPCallResult{IsError: true, Text: "no such terminal"}}
	tool2 := newTerminalSendCommandTool(Deps{MCP: failMCP, Observer: obs2})
	d3, _ := tool2.Decode(json.RawMessage(`{"terminalId":"t9","command":"ls"}`))
	if res := tool2.Handle(context.Background(), d3, &tools.ToolContext{}); res.Ok {
		t.Fatal("tool-error send should fail")
	}
	if len(obs2.marked) != 1 || obs2.marked[0] != "t9" {
		t.Fatalf("an attempted-but-failed send must still mark the terminal, got %v", obs2.marked)
	}

	// A local validation reject sends nothing → no mark.
	obs3 := &recordObserver{}
	tool3 := newTerminalSendCommandTool(Deps{MCP: mcp, Observer: obs3})
	d4, _ := tool3.Decode(json.RawMessage(`{"terminalId":"t9","command":"   "}`))
	if res := tool3.Handle(context.Background(), d4, &tools.ToolContext{}); res.Ok {
		t.Fatal("blank command should be rejected")
	}
	if len(obs3.marked) != 0 {
		t.Fatalf("a locally rejected send must not mark, got %v", obs3.marked)
	}

	// nil Observer is safe (unwired tests / stripped builds).
	tool4 := newTerminalSendCommandTool(Deps{MCP: mcp})
	d5, _ := tool4.Decode(json.RawMessage(`{"terminalId":"t7","command":"ls"}`))
	if res := tool4.Handle(context.Background(), d5, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("nil observer send should succeed: %+v", res.Error)
	}
}
