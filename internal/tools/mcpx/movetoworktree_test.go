package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// canonical builds a full terminal-<uuid> id from a short 8-hex stem, so a test can
// state "this id is canonical" without a 36-character literal at every use site.
func canonical(stem string) string {
	return fmt.Sprintf("terminal-%s-3d11-424c-90cb-136f24046295", stem)
}

// rosterResult is a terminal.list result carrying ids, in the shape terminalid's
// parser reads (Daintree returns the payload in the TEXT body).
func rosterResult(ids ...string) MCPCallResult {
	rows := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]any{"id": id})
	}
	body, _ := json.Marshal(map[string]any{"terminals": rows})
	return MCPCallResult{Text: string(body)}
}

// failDetail pulls one key out of a failure's Details payload. Details is typed
// `any` on the envelope, so every read has to go through the map assertion first.
func failDetail(t *testing.T, res tools.ToolResult, key string) any {
	t.Helper()
	if res.Error == nil {
		t.Fatalf("want a failure carrying details, got %+v", res)
	}
	m, ok := res.Error.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details is %T, want map[string]any", res.Error.Details)
	}
	return m[key]
}

// moveTool decodes+dispatches one terminal.moveToWorktree call against a fake MCP,
// exercising the SAME Decode→Handle path dispatch uses (so a Validate() rejection
// shows up here as a decode error, exactly as it would before the confirmation).
func moveTool(t *testing.T, mcp *fakeMCP, args string) (tools.ToolResult, error) {
	t.Helper()
	tool := newTerminalMoveToWorktreeTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tool.Handle(context.Background(), decoded, &tools.ToolContext{}), nil
}

// Daintree's raw terminal.moveToWorktree moves exactly ONE pane per call, so the
// cohort form — the whole reason the action is wrapped — has to fan out internally:
// one confirmed wrapper call, N raw calls, each carrying the SAME destination.
func TestTerminalMoveToWorktreeBatchMovesAllInOneCall(t *testing.T) {
	a, b, c := canonical("aaaaaaaa"), canonical("bbbbbbbb"), canonical("cccccccc")
	mcp := &fakeMCP{connected: true}
	res, err := moveTool(t, mcp, fmt.Sprintf(
		`{"terminalIds":[%q,%q,%q],"worktreeId":"/w/feature-x"}`, a, b, c))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("batch move failed: %+v", res.Error)
	}
	moves := mcp.callsTo("terminal.moveToWorktree")
	if len(moves) != 3 {
		t.Fatalf("want 3 raw moves, got %d (%v)", len(moves), mcp.calls)
	}
	for i, want := range []string{a, b, c} {
		if moves[i]["terminalId"] != want {
			t.Errorf("move %d terminalId = %v, want %s", i, moves[i]["terminalId"], want)
		}
		if moves[i]["worktreeId"] != "/w/feature-x" {
			t.Errorf("move %d worktreeId = %v, want /w/feature-x", i, moves[i]["worktreeId"])
		}
	}
	// The follow-up is the sentence that actually relocates the work, so the SUMMARY —
	// what the model reads back and what lands in the audit row — must carry it.
	if !strings.Contains(res.Summary, "Please continue in the directory /w/feature-x") {
		t.Errorf("summary must carry the follow-up instruction, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "NOT restarted") {
		t.Errorf("summary must say the process was not restarted, got %q", res.Summary)
	}
}

// The model reaches for the singular `terminalId` by analogy with every other
// terminal.* wrapper; both shapes must work, and both ids and the destination path
// are trimmed before they reach Daintree (which matches exactly).
func TestTerminalMoveToWorktreeAcceptsSingularIdAndTrims(t *testing.T) {
	a := canonical("aaaaaaaa")
	mcp := &fakeMCP{connected: true}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalId":"  %s  ","worktreeId":"  /w/x  "}`, a))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("singular move failed: %+v", res.Error)
	}
	moves := mcp.callsTo("terminal.moveToWorktree")
	if len(moves) != 1 {
		t.Fatalf("want 1 raw move, got %d", len(moves))
	}
	if moves[0]["terminalId"] != a {
		t.Errorf("terminalId not trimmed: %q", moves[0]["terminalId"])
	}
	if moves[0]["worktreeId"] != "/w/x" {
		t.Errorf("worktreeId not trimmed: %q", moves[0]["worktreeId"])
	}
}

// Naming the same terminal in both fields must not move it twice — a duplicate raw
// call is a second confirmed mutation the human never agreed to.
func TestTerminalMoveToWorktreeDedupesAcrossBothFields(t *testing.T) {
	a, b := canonical("aaaaaaaa"), canonical("bbbbbbbb")
	mcp := &fakeMCP{connected: true}
	res, err := moveTool(t, mcp, fmt.Sprintf(
		`{"terminalId":%q,"terminalIds":[%q,"  ",%q],"worktreeId":"/w/x"}`, a, a, b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("move failed: %+v", res.Error)
	}
	moves := mcp.callsTo("terminal.moveToWorktree")
	if len(moves) != 2 {
		t.Fatalf("want 2 deduped moves, got %d (%v)", len(moves), moves)
	}
	if moves[0]["terminalId"] != a || moves[1]["terminalId"] != b {
		t.Errorf("dedupe lost input order: %v", moves)
	}
}

// Rejection happens at DECODE — i.e. before dispatch prompts the human — so an
// unusable call never becomes a confirmation for a move that then fails.
func TestTerminalMoveToWorktreeRejectsEmptyAtDecodeBeforeConfirm(t *testing.T) {
	for _, bad := range []string{
		`{}`,
		`{"worktreeId":"/w/x"}`,
		`{"terminalIds":[],"worktreeId":"/w/x"}`,
		`{"terminalId":"   ","worktreeId":"/w/x"}`,
		`{"terminalId":"t1"}`,
		`{"terminalId":"t1","worktreeId":"   "}`,
		// The strict decoder must also reject the dotted/flattened key shape the model
		// has been observed inventing from prose.
		`{"terminalId":"t1","worktreeId":"/w/x","worktree.path":"/w/x"}`,
	} {
		mcp := &fakeMCP{connected: true}
		if _, err := moveTool(t, mcp, bad); err == nil {
			t.Errorf("%s should be rejected at decode", bad)
		}
		if len(mcp.calls) != 0 {
			t.Errorf("%s must not reach the MCP, called %v", bad, mcp.calls)
		}
	}
}

// The model routinely truncates terminal-<uuid> ids to a short prefix. A cohort of
// prefixes must resolve against ONE roster snapshot — never one read per id, which
// would let two ids in a single confirmed operation disagree about what was live.
func TestTerminalMoveToWorktreeResolvesCohortWithOneTerminalList(t *testing.T) {
	a, b := canonical("aaaaaaaa"), canonical("bbbbbbbb")
	mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
		"terminal.list": rosterResult(a, b),
	}}
	res, err := moveTool(t, mcp, `{"terminalIds":["terminal-aaaaaaaa","terminal-bbbbbbbb"],"worktreeId":"/w/x"}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("prefix cohort should resolve and move: %+v", res.Error)
	}
	if lists := mcp.callsTo("terminal.list"); len(lists) != 1 {
		t.Fatalf("want exactly 1 roster read for the whole cohort, got %d", len(lists))
	}
	moves := mcp.callsTo("terminal.moveToWorktree")
	if len(moves) != 2 || moves[0]["terminalId"] != a || moves[1]["terminalId"] != b {
		t.Fatalf("raw moves must carry the FULL canonical ids, got %v", moves)
	}
}

// An already-canonical cohort is the common case (ids echoed straight from a spawn
// or a listing) and must not pay for a roster read on the way to the mutation.
func TestTerminalMoveToWorktreeCanonicalIDsSkipTerminalList(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	res, err := moveTool(t, mcp, fmt.Sprintf(
		`{"terminalIds":[%q,%q],"worktreeId":"/w/x"}`, canonical("aaaaaaaa"), canonical("bbbbbbbb")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("canonical move failed: %+v", res.Error)
	}
	if lists := mcp.callsTo("terminal.list"); len(lists) != 0 {
		t.Fatalf("a canonical cohort must skip the roster read, got %d list calls", len(lists))
	}
}

// An unreadable roster is the transport-hiccup symptom, not "every id you named is
// wrong" — resolution must FAIL OPEN and let the human-confirmed move proceed on the
// ids as given, with Daintree's own not-found error as the backstop.
func TestTerminalMoveToWorktreeResolutionFailsOpen(t *testing.T) {
	cases := map[string]*fakeMCP{
		"transport error": {connected: true,
			errsByName: map[string]error{"terminal.list": fmt.Errorf("stream closed")}},
		"daintree error result": {connected: true,
			resultsByName: map[string]MCPCallResult{"terminal.list": {IsError: true, Text: "nope"}}},
		"empty roster": {connected: true,
			resultsByName: map[string]MCPCallResult{"terminal.list": rosterResult()}},
		"unparseable roster": {connected: true,
			resultsByName: map[string]MCPCallResult{"terminal.list": {Text: "not json"}}},
	}
	for name, mcp := range cases {
		res, err := moveTool(t, mcp, `{"terminalId":"terminal-aaaaaaaa","worktreeId":"/w/x"}`)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !res.Ok {
			t.Errorf("%s: must fail OPEN and attempt the move, got %+v", name, res.Error)
		}
		moves := mcp.callsTo("terminal.moveToWorktree")
		if len(moves) != 1 || moves[0]["terminalId"] != "terminal-aaaaaaaa" {
			t.Errorf("%s: want the unresolved id forwarded verbatim, got %v", name, moves)
		}
	}
}

// When the roster IS readable and an id definitively matches nothing (or matches
// several), the whole call fails with ZERO moves. Half a relocated cohort with no
// signal about which half is harder to recover from than a clean rejection.
func TestTerminalMoveToWorktreeRejectsPartiallyResolvableCohortAtomically(t *testing.T) {
	a := canonical("aaaaaaaa")
	// Two live ids share the "terminal-cc" stem, so that prefix is ambiguous.
	c1 := "terminal-cccccccc-1111-424c-90cb-136f24046295"
	c2 := "terminal-cccccccc-2222-424c-90cb-136f24046295"
	for name, ids := range map[string]string{
		"unknown id":       fmt.Sprintf(`[%q,"terminal-zzzzzzzz"]`, a),
		"ambiguous prefix": fmt.Sprintf(`[%q,"terminal-cccccccc"]`, a),
	} {
		mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
			"terminal.list": rosterResult(a, c1, c2),
		}}
		res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalIds":%s,"worktreeId":"/w/x"}`, ids))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if res.Ok || res.Error == nil || res.Error.Code != codeTerminalNotFound {
			t.Fatalf("%s: want %s, got %+v", name, codeTerminalNotFound, res)
		}
		if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 0 {
			t.Errorf("%s: an unresolvable cohort must move NOTHING, moved %v", name, moves)
		}
		// The recovery is "use the full id" — which needs the live list in hand.
		if !strings.Contains(res.Error.Message, a) {
			t.Errorf("%s: the failure must name the live terminals, got %q", name, res.Error.Message)
		}
		if !strings.Contains(res.Error.Message, "Nothing was moved") {
			t.Errorf("%s: the failure must say nothing moved, got %q", name, res.Error.Message)
		}
	}
}

// An ordinary per-id refusal (a closed pane, a bad destination) must not abort the
// rest of the batch, and the aggregate must be a FAILURE naming every id that did not
// move — a partial outcome is never narrated as a clean success.
func TestTerminalMoveToWorktreeReportsPartialFailureFaithfully(t *testing.T) {
	a, b, c := canonical("aaaaaaaa"), canonical("bbbbbbbb"), canonical("cccccccc")
	mcp := &fakeMCP{connected: true, failOn: map[string]bool{b: true}}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalIds":[%q,%q,%q],"worktreeId":"/w/x"}`, a, b, c))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Ok {
		t.Fatalf("a partial move must NOT report success: %+v", res)
	}
	if res.Error.Code != codeMCPToolError {
		t.Errorf("code = %q, want %q", res.Error.Code, codeMCPToolError)
	}
	// The failure in the MIDDLE must not stop the last id.
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 3 {
		t.Errorf("an ordinary refusal must not abort the batch, got %d moves", len(moves))
	}
	if !strings.Contains(res.Error.Message, b) {
		t.Errorf("the summary must name the unmoved id, got %q", res.Error.Message)
	}
	moved, _ := failDetail(t, res, "moved").([]string)
	if len(moved) != 2 {
		t.Errorf("details.moved should hold the 2 that moved, got %v", moved)
	}
	// Daintree's own refusal text is the only thing that distinguishes "that pane is
	// gone" from "that destination path is wrong" — two opposite recoveries.
	refusals, _ := failDetail(t, res, "refusals").(map[string]string)
	if refusals[b] == "" {
		t.Errorf("details.refusals must preserve Daintree's message per id, got %v", refusals)
	}
}

// A dead link fails every remaining call identically, so the batch stops — and the
// ids it never tried are reported rather than silently vanishing.
func TestTerminalMoveToWorktreeAbortsBatchWhenMCPGone(t *testing.T) {
	a, b := canonical("aaaaaaaa"), canonical("bbbbbbbb")
	mcp := &fakeMCP{connected: false}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalIds":[%q,%q],"worktreeId":"/w/x"}`, a, b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("want %s, got %+v", codeMCPUnavailable, res)
	}
	if len(mcp.callsTo("terminal.moveToWorktree")) != 0 {
		t.Error("a disconnected MCP must not be called")
	}
	notAttempted, _ := failDetail(t, res, "notAttempted").([]string)
	if len(notAttempted) != 1 || notAttempted[0] != b {
		t.Errorf("the untried id must be reported, got %v", notAttempted)
	}
}

// A link that drops PART-WAY keeps the abort's own code (and the /reconnect hint it
// carries) rather than flattening it to a generic tool error.
func TestTerminalMoveToWorktreeAbortsMidBatchPreservingCode(t *testing.T) {
	a, b, c := canonical("aaaaaaaa"), canonical("bbbbbbbb"), canonical("cccccccc")
	mcp := &fakeMCP{connected: true, disconnectAfter: 1}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalIds":[%q,%q,%q],"worktreeId":"/w/x"}`, a, b, c))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("want the abort's own %s, got %+v", codeMCPUnavailable, res)
	}
	if moved, _ := failDetail(t, res, "moved").([]string); len(moved) != 1 || moved[0] != a {
		t.Errorf("the one completed move must still be reported, got %v", moved)
	}
	if notAttempted, _ := failDetail(t, res, "notAttempted").([]string); len(notAttempted) != 1 || notAttempted[0] != c {
		t.Errorf("the untried tail must be reported, got %v", notAttempted)
	}
}

// The wrapper's model-facing contract. Each clause here is load-bearing: the model
// only knows the literal argument shape, the path-not-branch rule, and the mandatory
// follow-up because this description says so.
func TestTerminalMoveToWorktreeSchemaAndDescriptionContract(t *testing.T) {
	tool := newTerminalMoveToWorktreeTool(Deps{MCP: &fakeMCP{connected: true}})
	if tool.Name != "terminal.moveToWorktree" {
		t.Fatalf("name = %q", tool.Name)
	}
	// Mutates functional state (and can carry a whole tab group), so it confirms —
	// Daintree's own danger:"safe" label governs THEIR dialog, not our gate.
	if tool.Risk != domain.RiskTerminal {
		t.Errorf("risk = %q, want %q", tool.Risk, domain.RiskTerminal)
	}
	for _, want := range []string{
		`"terminalId":"terminal-<uuid>"`,   // the literal singular shape
		`terminalIds:["terminal-<uuid>"`,   // the literal cohort shape
		"exact PATH from worktree.list",    // never a branch name
		"does NOT restart the process",     // the trap this wrapper exists to close
		"Please continue in the directory", // the sentence that actually relocates work
	} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description is missing %q:\n%s", want, tool.Description)
		}
	}
	if tool.Consequence == "" {
		t.Error("a confirming tool must state its consequence")
	}
	// Bounds are encoded as real JSON Schema keywords because the schema is forwarded
	// to the model VERBATIM — a bound described only in prose is not a bound.
	var schema struct {
		AdditionalProperties bool     `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           map[string]struct {
			Type      string `json:"type"`
			MinLength *int   `json:"minLength"`
			MinItems  *int   `json:"minItems"`
			Items     *struct {
				MinLength *int `json:"minLength"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema.AdditionalProperties {
		t.Error("schema must be strict (additionalProperties:false)")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "worktreeId" {
		t.Errorf("required = %v, want [worktreeId] (the id fields are a one-of enforced by Validate)", schema.Required)
	}
	if p := schema.Properties["worktreeId"]; p.MinLength == nil || *p.MinLength != 1 {
		t.Error("worktreeId needs minLength:1")
	}
	if p := schema.Properties["terminalIds"]; p.MinItems == nil || *p.MinItems != 1 ||
		p.Items == nil || p.Items.MinLength == nil || *p.Items.MinLength != 1 {
		t.Error("terminalIds needs minItems:1 and items.minLength:1")
	}
}
