package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
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
	// Assert the COMPLETE ordered log, not a filtered view: a filtered check would pass
	// even if the wrapper also fired a stray read or an unrequested sendCommand.
	if len(mcp.calls) != 3 {
		t.Fatalf("want exactly 3 MCP calls total, got %d (%v)", len(mcp.calls), mcp.calls)
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
	result, _ := res.Result.(map[string]any)
	gotMoved, _ := result["moved"].([]string)
	if len(gotMoved) != 3 || gotMoved[0] != a || gotMoved[1] != b || gotMoved[2] != c {
		t.Errorf("result.moved must list every id in order, got %v", result["moved"])
	}
	if result["worktreeId"] != "/w/feature-x" {
		t.Errorf("result.worktreeId = %v, want /w/feature-x", result["worktreeId"])
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
	// terminal-deadbeef is a CREDIBLE truncation (valid hex, full 8 chars) that simply
	// matches nothing live — so it reaches resolution rather than the too-vague guard,
	// which is the path this test is about.
	for name, ids := range map[string]string{
		"unknown id":       fmt.Sprintf(`[%q,"terminal-deadbeef"]`, a),
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
	if len(moved) != 2 || moved[0] != a || moved[1] != c {
		t.Errorf("details.moved must be exactly [a c], got %v", moved)
	}
	if gotFailed, _ := failDetail(t, res, "failed").([]string); len(gotFailed) != 1 || gotFailed[0] != b {
		t.Errorf("details.failed must be exactly [b], got %v", gotFailed)
	}
	// An ordinary refusal does not abort, so nothing is left untried.
	if na := failDetail(t, res, "notAttempted"); na != nil {
		t.Errorf("an ordinary refusal must leave nothing not-attempted, got %v", na)
	}
	// The terminals that DID move are now filed under the destination while their
	// processes still run in the old directory. Failure details are NOT written to the
	// audit row, so the summary is the only durable record — it must name them AND
	// repeat the follow-up, or those agents stay half-relocated.
	for _, want := range []string{a, c, "Please continue in the directory /w/x"} {
		if !strings.Contains(res.Error.Message, want) {
			t.Errorf("a partial failure summary must contain %q, got %q", want, res.Error.Message)
		}
	}
	if !strings.Contains(res.Error.Message, "not certain") {
		t.Errorf("a failed move's outcome is ambiguous and the summary must say so, got %q", res.Error.Message)
	}
	// Daintree's own refusal text is the only thing that distinguishes "that pane is
	// gone" from "that destination path is wrong" — two opposite recoveries.
	refusals, _ := failDetail(t, res, "refusals").(map[string]string)
	if !strings.Contains(refusals[b], "no such terminal") {
		t.Errorf("details.refusals must preserve Daintree's OWN message per id, got %v", refusals)
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
	// MCP_UNAVAILABLE is RECOVERABLE and carries the one command that fixes it.
	if !res.Error.Recoverable {
		t.Error("a dead link is recoverable — /reconnect fixes it")
	}
	if !strings.Contains(res.Error.Message, "/reconnect") {
		t.Errorf("the failure must point at /reconnect, got %q", res.Error.Message)
	}
	for _, want := range []string{a, b} {
		if !strings.Contains(res.Error.Message, want) {
			t.Errorf("every unmoved id must be named; %q missing from %q", want, res.Error.Message)
		}
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
	// The one terminal that DID move is half-relocated; the abort summary must still
	// name it and carry the follow-up.
	for _, want := range []string{a, "Please continue in the directory /w/x"} {
		if !strings.Contains(res.Error.Message, want) {
			t.Errorf("an aborted batch with a completed move must contain %q, got %q", want, res.Error.Message)
		}
	}
}

// The wrapper's model-facing contract. Each clause here is load-bearing: the model
// only knows the literal argument shape, the path-not-branch rule, and the mandatory
// follow-up because this description says so. The assertions pin whole normative
// CLAUSES rather than isolated nouns on purpose — a substring check for
// "Please continue in the directory" is satisfied by a description that says NOT to
// send it, and this repo has a history of prose being edited to satisfy a
// mis-specified assertion rather than the other way round.
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
	// Whitespace is normalized so a re-wrap of the string literal doesn't break these,
	// but the wording itself has to survive intact.
	desc := strings.Join(strings.Fields(tool.Description), " ")
	for _, want := range []string{
		// The literal argument shapes. Prose alone gets flattened into invented keys.
		`{"terminalId":"terminal-<uuid>","worktreeId":"<worktreePath>"}`,
		`terminalIds:["terminal-<uuid>", ...]`,
		// The path-not-branch rule, with its polarity intact.
		"worktreeId is the exact PATH from worktree.list — a branch name is NEVER accepted",
		// The trap this wrapper exists to close, as a complete instruction: the negation,
		// the obligation, the tool to use, and why the sentence is the operative part.
		"The move does NOT restart the process",
		"you MUST follow up with terminal.sendCommand",
		`"Please continue in the directory <worktreePath>" — that sentence, not this move, relocates the work`,
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("description is missing the clause %q:\n%s", want, desc)
		}
	}
	// The consequence is what the human reads in the approval sheet, so the two
	// surprising facts — the tab group travels, the process does not restart — belong
	// there and not only in the model-facing description.
	cons := strings.Join(strings.Fields(tool.Consequence), " ")
	for _, want := range []string{"tab group", "Running processes keep going"} {
		if !strings.Contains(cons, want) {
			t.Errorf("consequence is missing %q, got %q", want, cons)
		}
	}

	// Bounds are encoded as real JSON Schema keywords because the schema is forwarded
	// to the model VERBATIM — a bound described only in prose is not a bound. Pointers
	// throughout so an ABSENT keyword is distinguishable from a zero value.
	var schema struct {
		Type                 string   `json:"type"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Required             []string `json:"required"`
		Properties           map[string]struct {
			Type      string `json:"type"`
			MinLength *int   `json:"minLength"`
			MinItems  *int   `json:"minItems"`
			Items     *struct {
				Type      string `json:"type"`
				MinLength *int   `json:"minLength"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema root type = %q, want object", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Error("schema must be strict: additionalProperties must be present and false")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "worktreeId" {
		t.Errorf("required = %v, want [worktreeId] (the id fields are a one-of enforced by Validate)", schema.Required)
	}
	// The exact property set — an extra or missing property is a contract change.
	if len(schema.Properties) != 3 {
		t.Errorf("want exactly 3 properties, got %d (%v)", len(schema.Properties), schema.Properties)
	}
	for _, name := range []string{"terminalId", "worktreeId"} {
		p, ok := schema.Properties[name]
		if !ok {
			t.Errorf("schema is missing property %q", name)
			continue
		}
		if p.Type != "string" {
			t.Errorf("%s.type = %q, want string", name, p.Type)
		}
		if p.MinLength == nil || *p.MinLength != 1 {
			t.Errorf("%s needs minLength:1", name)
		}
	}
	ids, ok := schema.Properties["terminalIds"]
	if !ok {
		t.Fatal("schema is missing property terminalIds")
	}
	if ids.Type != "array" {
		t.Errorf("terminalIds.type = %q, want array", ids.Type)
	}
	if ids.MinItems == nil || *ids.MinItems != 1 {
		t.Error("terminalIds needs minItems:1")
	}
	if ids.Items == nil || ids.Items.Type != "string" ||
		ids.Items.MinLength == nil || *ids.Items.MinLength != 1 {
		t.Errorf("terminalIds.items must be a string with minLength:1, got %+v", ids.Items)
	}
}

// A prefix too short to identify a terminal is REFUSED rather than expanded. Expansion
// happens after the human confirmed the raw args, so substituting on a weak prefix
// could move a terminal — and its whole tab group — that nobody approved, if the
// intended one closed while the approval was pending.
func TestTerminalMoveToWorktreeRefusesVaguePrefixWithoutResolving(t *testing.T) {
	a := canonical("aaaaaaaa")
	for _, vague := range []string{"terminal-5", "terminal-", "t1", "terminal-zzz"} {
		mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
			"terminal.list": rosterResult(a),
		}}
		res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalId":%q,"worktreeId":"/w/x"}`, vague))
		if err != nil {
			t.Fatalf("%s: decode: %v", vague, err)
		}
		if res.Ok || res.Error.Code != codeTerminalNotFound {
			t.Fatalf("%q must be refused as too vague, got %+v", vague, res)
		}
		// Refused BEFORE the roster read: we do not even look for a match to substitute.
		if len(mcp.calls) != 0 {
			t.Errorf("%q must be refused without touching the MCP, called %v", vague, mcp.calls)
		}
	}
	// The standard truncation — terminal- plus the first 8 hex — still resolves.
	mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
		"terminal.list": rosterResult(a),
	}}
	res, err := moveTool(t, mcp, `{"terminalId":"terminal-aaaaaaaa","worktreeId":"/w/x"}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("the standard 8-hex truncation must still resolve: %+v", res.Error)
	}
}

// The roster parser UNIONs structuredContent and the text body. Exercising only the
// text body would leave a regression that drops StructuredContent entirely green.
func TestTerminalMoveToWorktreeResolvesFromStructuredRoster(t *testing.T) {
	a := canonical("aaaaaaaa")
	mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
		"terminal.list": {StructuredContent: map[string]any{
			"terminals": []any{map[string]any{"id": a}},
		}},
	}}
	res, err := moveTool(t, mcp, `{"terminalId":"terminal-aaaaaaaa","worktreeId":"/w/x"}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("a structuredContent-only roster must resolve: %+v", res.Error)
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 1 || moves[0]["terminalId"] != a {
		t.Errorf("want the canonical id from structuredContent, got %v", moves)
	}
}

// Two DIFFERENT requested spellings of the same terminal (a full id and a prefix of it)
// collapse to one canonical id — and therefore ONE raw move. Two moves would be a
// second confirmed mutation on the same pane.
func TestTerminalMoveToWorktreeCollapsesTwoSpellingsOfOneTerminal(t *testing.T) {
	a := canonical("aaaaaaaa")
	mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
		"terminal.list": rosterResult(a),
	}}
	res, err := moveTool(t, mcp, fmt.Sprintf(
		`{"terminalIds":["terminal-aaaaaaaa",%q],"worktreeId":"/w/x"}`, a))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("move failed: %+v", res.Error)
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 1 {
		t.Fatalf("two spellings of one terminal must produce ONE move, got %d (%v)", len(moves), moves)
	}
}

// terminalid.Resolve's exact-match-wins rule, exercised through the wrapper: a request
// that is BOTH an exact live id and a prefix of a longer live id resolves to itself,
// never to the longer one, and is never reported ambiguous.
func TestTerminalMoveToWorktreeExactMatchBeatsPrefixCollision(t *testing.T) {
	short := "terminal-aaaaaaaa"
	long := canonical("aaaaaaaa") // has `short` as a prefix
	mcp := &fakeMCP{connected: true, resultsByName: map[string]MCPCallResult{
		"terminal.list": rosterResult(short, long),
	}}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalId":%q,"worktreeId":"/w/x"}`, short))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("an exact live id must win outright: %+v", res.Error)
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 1 || moves[0]["terminalId"] != short {
		t.Errorf("exact match must not be lost to a coincidental prefix, got %v", moves)
	}
}

// A wrong destination fails EVERY id with the same Daintree message. We deliberately
// do not sniff that prose to abort early (it would break the day it is reworded), so
// the batch runs to completion — but the report must name every unmoved id and must
// NOT claim anything moved.
func TestTerminalMoveToWorktreeBadDestinationFailsEveryIDFaithfully(t *testing.T) {
	a, b := canonical("aaaaaaaa"), canonical("bbbbbbbb")
	mcp := &fakeMCP{connected: true, failOn: map[string]bool{a: true, b: true}}
	res, err := moveTool(t, mcp, fmt.Sprintf(`{"terminalIds":[%q,%q],"worktreeId":"not-a-path"}`, a, b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Ok {
		t.Fatalf("every id failed; this must not report success: %+v", res)
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 2 {
		t.Errorf("an ordinary refusal must not abort the batch, got %d moves", len(moves))
	}
	if moved, _ := failDetail(t, res, "moved").([]string); len(moved) != 0 {
		t.Errorf("nothing moved; details.moved must be empty, got %v", moved)
	}
	if !strings.Contains(res.Error.Message, "none moved") {
		t.Errorf("the summary must say nothing moved, got %q", res.Error.Message)
	}
	// With nothing moved there is no agent to instruct, so the follow-up must NOT be
	// there — a spurious instruction teaches the model to send it after a failed move.
	if strings.Contains(res.Error.Message, "Please continue in the directory") {
		t.Errorf("no terminal moved, so no follow-up should be suggested: %q", res.Error.Message)
	}
}

// A cancelled turn must not start moving terminals. Resolution treats an unreadable
// roster as fail-open, so cancellation has to be checked explicitly or an abandoned
// turn would fall straight through into the mutation loop.
func TestTerminalMoveToWorktreeCancelledDuringResolutionMovesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mcp := &fakeMCP{connected: true, onCall: func() { cancel() }}
	tool := newTerminalMoveToWorktreeTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(`{"terminalId":"terminal-aaaaaaaa","worktreeId":"/w/x"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(ctx, decoded, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeCancelled {
		t.Fatalf("want %s, got %+v", codeCancelled, res)
	}
	if res.Error.Recoverable {
		t.Error("a cancelled turn is not recoverable by retrying the same call")
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 0 {
		t.Errorf("a cancelled turn must move NOTHING, moved %v", moves)
	}
}

// A destination path with spaces and non-ASCII must reach Daintree byte-for-byte and
// render intact in the summary — the summary is where the model reads the path back to
// build the follow-up command.
func TestTerminalMoveToWorktreeForwardsAwkwardDestinationVerbatim(t *testing.T) {
	a := canonical("aaaaaaaa")
	dest := "/Users/me/Pro jects/café-100%/wt"
	mcp := &fakeMCP{connected: true}
	raw, _ := json.Marshal(map[string]any{"terminalId": a, "worktreeId": dest})
	res, err := moveTool(t, mcp, string(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Ok {
		t.Fatalf("move failed: %+v", res.Error)
	}
	if moves := mcp.callsTo("terminal.moveToWorktree"); len(moves) != 1 || moves[0]["worktreeId"] != dest {
		t.Errorf("destination must be forwarded verbatim, got %v", moves)
	}
	if !strings.Contains(res.Summary, "Please continue in the directory "+dest) {
		t.Errorf("the summary must render the destination intact, got %q", res.Summary)
	}
}

// Decode really is the pre-confirmation gate: dispatch decodes, THEN tier-gates, THEN
// confirms. Driven through the real Registry.Dispatch with a confirmation spy so a
// regression that moved validation after the prompt would fail here — the unit tests
// above call Decode directly and could not see it.
func TestTerminalMoveToWorktreeInvalidArgsNeverReachConfirmation(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	reg := tools.NewRegistry()
	tool := newTerminalMoveToWorktreeTool(Deps{MCP: mcp})
	if err := reg.Register(&tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	confirmations := 0
	tctx := &tools.ToolContext{
		Actor:  domain.ActorMain,
		Config: config.AppConfig{Tier: domain.TierSystem},
		Confirm: func(context.Context, tools.ConfirmRequest) (bool, error) {
			confirmations++
			return true, nil
		},
	}
	res := reg.Dispatch(context.Background(), "terminal.moveToWorktree",
		json.RawMessage(`{"terminalIds":[],"worktreeId":"/w/x"}`), tctx)
	if res.Ok {
		t.Fatalf("an empty cohort must be rejected: %+v", res)
	}
	if confirmations != 0 {
		t.Errorf("invalid args must be rejected BEFORE the human is prompted, prompted %d time(s)", confirmations)
	}
	if len(mcp.calls) != 0 {
		t.Errorf("invalid args must never reach the MCP, called %v", mcp.calls)
	}
}
