package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// okResult builds the passthrough envelope shape shapeForgeChecks reads.
func okResult(ciStatus any) tools.ToolResult {
	return tools.Ok("Called forge.getCIStatus.", map[string]any{
		"text":              "",
		"structuredContent": map[string]any{"ciStatus": ciStatus},
	})
}

// A null ciStatus means "we could not read the check state". It does NOT mean "there are
// no checks", and the raw payload gives those two answers the same representation — which
// is why every consumer was re-learning the distinction from prose. Getting it wrong
// turns "I could not tell" into "nothing gates this merge".
func TestForgeChecksNullIsNotNoChecks(t *testing.T) {
	res := shapeForgeChecks(42, okResult(nil))
	if !res.Ok {
		t.Fatalf("a null status is a valid answer, not a failure: %+v", res)
	}
	out, _ := res.Result.(map[string]any)
	if out["reported"] != false {
		t.Errorf("reported = %v, want false", out["reported"])
	}
	if out["state"] != "unknown" {
		t.Errorf("state = %v, want \"unknown\"", out["state"])
	}
	// No counts at all — a zero here would read as "0 checks failed", which is precisely
	// the false reassurance this case must not give.
	for _, k := range []string{"total", "passed", "failed", "pending"} {
		if _, present := out[k]; present {
			t.Errorf("%q is present on an unread status — a count implies a reading was taken", k)
		}
	}
	if out["conclusive"] != false {
		t.Errorf("conclusive = %v, want false", out["conclusive"])
	}
	if !strings.Contains(res.Summary, "not the same as having no checks") {
		t.Errorf("summary does not distinguish unread from empty: %q", res.Summary)
	}
}

// `total: 0` is the single most misread value in this payload: it also appears when the
// required-check list could not be read in full, so it is never evidence that nothing
// gates the merge. The summary has to say so, because the number alone cannot.
func TestForgeChecksZeroTotalIsNotAGreenVerdict(t *testing.T) {
	res := shapeForgeChecks(7, okResult(map[string]any{
		"state": "success", "total": float64(0), "passed": float64(0),
		"failed": float64(0), "pending": float64(0),
	}))
	out, _ := res.Result.(map[string]any)
	// The whole point: a machine-readable field, not a caveat buried in prose. An agent
	// gating a fix-and-verify loop reads this, and a parenthetical cannot outweigh a
	// sibling field that says "success".
	if out["conclusive"] != false {
		t.Fatalf("state=success with 0 required checks was reported as conclusive — a loop would stop here:\n%+v", out)
	}
	if !strings.Contains(res.Summary, "INCONCLUSIVE") {
		t.Errorf("the summary leads with a green verdict: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "could not be read") {
		t.Errorf("the summary does not explain why zero is not reassuring: %q", res.Summary)
	}
	// The forge DID answer — that is a different fact from the answer being usable.
	if out["reported"] != true {
		t.Errorf("reported = %v, want true", out["reported"])
	}
}

// An unrecognised state must not be echoed as a verdict. A future provider spelling
// ("green") would otherwise ride through with conclusive=true attached to a value this
// CLI has never agreed to interpret.
func TestForgeChecksUnknownStateIsInconclusive(t *testing.T) {
	res := shapeForgeChecks(8, okResult(map[string]any{
		"state": "green", "total": float64(5), "passed": float64(5),
	}))
	out, _ := res.Result.(map[string]any)
	if out["state"] != "unknown" {
		t.Errorf("state = %v, want \"unknown\" — an unrecognised value must be downgraded", out["state"])
	}
	if out["conclusive"] != false {
		t.Errorf("conclusive = %v, want false", out["conclusive"])
	}
}

// A count that is absent or wrongly typed must be OMITTED, never defaulted to zero. A
// fabricated "0 failed" is the same class of false reassurance as a green zero-total.
func TestForgeChecksDoesNotFabricateMissingCounts(t *testing.T) {
	res := shapeForgeChecks(9, okResult(map[string]any{
		"state": "success", "total": float64(5), "passed": float64(5),
		"failed": "not a number",
	}))
	out, _ := res.Result.(map[string]any)
	for _, k := range []string{"failed", "pending"} {
		if _, present := out[k]; present {
			t.Errorf("%q was fabricated from a missing/invalid field: %v", k, out[k])
		}
	}
	if strings.Contains(res.Summary, "0 failed") {
		t.Errorf("the summary invented a zero failure count: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "5/5 required passed") {
		t.Errorf("the counts that DID decode were lost: %q", res.Summary)
	}
}

// The happy path: the state is the answer, the counts ride along, and the fact that they
// cover required checks ONLY is a field rather than a footnote somebody has to remember.
func TestForgeChecksReportsStateAndCounts(t *testing.T) {
	res := shapeForgeChecks(12, okResult(map[string]any{
		"state": "failure", "total": float64(5), "passed": float64(3),
		"failed": float64(1), "pending": float64(1), "requiredChecksPassing": false,
	}))
	out, _ := res.Result.(map[string]any)
	if out["state"] != "failure" {
		t.Errorf("state = %v, want \"failure\"", out["state"])
	}
	if out["countsCoverRequiredChecksOnly"] != true {
		t.Error("the required-only scope of the counts is not stated in the result")
	}
	if out["requiredChecksPassing"] != false {
		t.Errorf("requiredChecksPassing = %v, want false", out["requiredChecksPassing"])
	}
	// Staleness is a property of every reading, so it rides every result: one success is
	// a snapshot of ~a minute ago, not a settled verdict.
	if out["mayLagByMs"] != forgeChecksLagMs {
		t.Errorf("mayLagByMs = %v, want %d", out["mayLagByMs"], forgeChecksLagMs)
	}
	if out["conclusive"] != true {
		t.Errorf("conclusive = %v, want true — 5 required checks were actually read", out["conclusive"])
	}
	if !strings.Contains(res.Summary, "3/5 required passed") {
		t.Errorf("summary does not carry the counts: %q", res.Summary)
	}
}

// The forge rolls every check up to one state and dispatch strips rawData, so "which
// check failed?" is unanswerable here. Saying that in the result is better than leaving
// the model to hunt for a field that does not exist — the failure mode is a wasted turn
// re-calling the tool with different arguments.
func TestForgeChecksAdvertisesNoPerCheckDetail(t *testing.T) {
	for _, status := range []any{nil, map[string]any{"state": "success", "total": float64(2)}} {
		out, _ := shapeForgeChecks(1, okResult(status)).Result.(map[string]any)
		if out["perCheckDetail"] != false {
			t.Errorf("perCheckDetail = %v for status %v, want false", out["perCheckDetail"], status)
		}
	}
}

// An unrecognised payload degrades to "could not read" rather than to a fabricated
// verdict. Inventing "unknown, and here are some zeroes" from a shape we do not
// understand would recreate the exact confusion this wrapper removes.
func TestForgeChecksUnparseablePayloadIsUnread(t *testing.T) {
	for _, res := range []tools.ToolResult{
		tools.Ok("x", "not a map"),
		tools.Ok("x", map[string]any{"structuredContent": "not a map"}),
		tools.Ok("x", map[string]any{"structuredContent": map[string]any{}}),
		tools.Ok("x", map[string]any{"structuredContent": map[string]any{"ciStatus": "wrong type"}}),
	} {
		out, _ := shapeForgeChecks(3, res).Result.(map[string]any)
		if out["reported"] != false {
			t.Errorf("an unparseable payload reported %v, want reported=false", out["reported"])
		}
	}
}

// A failed passthrough (MCP down, forge refusal) must surface as the failure it is, not
// be laundered into a "state: unknown" that reads like a real answer.
func TestForgeChecksDescriptionCarriesTheTraps(t *testing.T) {
	tool := newForgeGetChecksTool()
	if tool.Name != "forge.getChecks" {
		t.Fatalf("tool name = %q", tool.Name)
	}
	// The description must carry the traps: this is the surface the model reads, and the
	// whole point of the wrapper is that they stop living in runbook prose.
	for _, want := range []string{"conclusive", "REQUIRED checks only", "per-check"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description does not mention %q:\n%s", want, tool.Description)
		}
	}
}

// The critical test: drive the whole tool with a payload decoded from REAL JSON the way
// the MCP client decodes it (`StructuredContent any` via encoding/json), rather than a
// hand-built Go map.
//
// The failure this catches is silent and total. If the type assertion in
// forgeCIStatusFrom cannot match what the live client actually produces — a different
// concrete type, numbers arriving as json.Number, a nesting level off by one — then every
// real call degrades to reported:false while every hand-built-envelope test above still
// passes. The tool would report "could not read the check state" for a perfectly healthy
// PR, forever, and nothing would say so.
func TestForgeChecksDecodesTheRealWireShape(t *testing.T) {
	// Verbatim from the Daintree forge action's result schema.
	const wire = `{"ciStatus":{"state":"failure","total":5,"passed":3,"failed":1,"pending":1,"requiredChecksPassing":false}}`
	var structured any
	if err := json.Unmarshal([]byte(wire), &structured); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	m := &fakeMCP{connected: true, result: tools.MCPCallResult{StructuredContent: structured}}
	tool := findTool(Tools(Deps{}), "forge.getChecks")
	if tool == nil {
		t.Fatal("forge.getChecks is not registered")
	}
	res := tool.Handle(context.Background(), json.RawMessage(`{"prNumber":5}`), ctxWith(m))
	if !res.Ok {
		t.Fatalf("handler failed: %+v", res)
	}
	// It must forward to the MCP action, not to a tool named after the wrapper.
	if m.lastName != "forge.getCIStatus" {
		t.Errorf("forwarded to %q, want forge.getCIStatus", m.lastName)
	}
	if m.lastArgs["prNumber"] != 5 {
		t.Errorf("forwarded prNumber = %v, want 5", m.lastArgs["prNumber"])
	}

	out, _ := res.Result.(map[string]any)
	if out["reported"] != true {
		t.Fatalf("a well-formed live payload decoded as unread — the type assertion does not match the real client:\n%+v", out)
	}
	if out["state"] != "failure" {
		t.Errorf("state = %v, want \"failure\"", out["state"])
	}
	// json.Unmarshal into `any` yields float64 (the SDK does not enable UseNumber); the
	// counts must survive that decoding.
	if !strings.Contains(res.Summary, "3/5 required passed") {
		t.Errorf("counts lost in decoding: %q", res.Summary)
	}
	if out["requiredChecksPassing"] != false {
		t.Errorf("requiredChecksPassing = %v, want false", out["requiredChecksPassing"])
	}
}

// The same, for the null case as it arrives off the wire.
func TestForgeChecksDecodesTheRealNullShape(t *testing.T) {
	var structured any
	if err := json.Unmarshal([]byte(`{"ciStatus":null}`), &structured); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{StructuredContent: structured}}
	tool := findTool(Tools(Deps{}), "forge.getChecks")
	res := tool.Handle(context.Background(), json.RawMessage(`{"prNumber":9}`), ctxWith(m))
	if !res.Ok {
		t.Fatalf("a null status is an answer, not a failure: %+v", res)
	}
	out, _ := res.Result.(map[string]any)
	if out["reported"] != false {
		t.Errorf("reported = %v, want false", out["reported"])
	}
}

// An MCP failure (disconnected, forge refusal) must surface AS a failure. Laundering it
// into "state: unknown" would dress an outage up as a verdict.
func TestForgeChecksSurfacesMCPFailure(t *testing.T) {
	tool := findTool(Tools(Deps{}), "forge.getChecks")
	res := tool.Handle(context.Background(), json.RawMessage(`{"prNumber":9}`), ctxWith(&fakeMCP{connected: false}))
	if res.Ok {
		t.Fatalf("a disconnected MCP produced a successful verdict: %+v", res)
	}
}

// Both by-number forge reads are on the daintree.call denylist, so a locator spelling the
// wrapper cannot express is a spelling with NO path at all. The underlying actions accept
// worktreeId, worktreePath and the legacy cwd; the wrappers must forward all three, or
// blocking the escape hatch removes capability instead of just redirecting it.
func TestForgeByNumberReadsForwardEveryLocator(t *testing.T) {
	for _, tc := range []struct{ tool, action string }{
		{"forge.getChecks", "forge.getCIStatus"},
		{"forge.getPR", "forge.getPR"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{StructuredContent: map[string]any{}}}
			tool := findTool(Tools(Deps{}), tc.tool)
			if tool == nil {
				t.Fatalf("%s is not registered", tc.tool)
			}
			args := `{"prNumber":4,"worktreeId":"wt_abc","worktreePath":"/repo/.worktrees/x","cwd":"/repo"}`
			if res := tool.Handle(context.Background(), json.RawMessage(args), ctxWith(m)); !res.Ok && tc.tool == "forge.getPR" {
				t.Fatalf("handler failed: %+v", res)
			}
			if m.lastName != tc.action {
				t.Errorf("forwarded to %q, want %q", m.lastName, tc.action)
			}
			for k, want := range map[string]any{
				"worktreeId":   "wt_abc",
				"worktreePath": "/repo/.worktrees/x",
				"cwd":          "/repo",
			} {
				if m.lastArgs[k] != want {
					t.Errorf("%s not forwarded: got %v, want %v", k, m.lastArgs[k], want)
				}
			}
		})
	}
}

// An omitted locator must not be forwarded as an empty string — the action would read
// that as "this worktree, spelled as nothing" rather than as absent.
func TestForgeByNumberReadsOmitAbsentLocators(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{StructuredContent: map[string]any{}}}
	tool := findTool(Tools(Deps{}), "forge.getChecks")
	tool.Handle(context.Background(), json.RawMessage(`{"prNumber":4}`), ctxWith(m))
	for _, k := range []string{"cwd", "worktreeId", "worktreePath"} {
		if _, present := m.lastArgs[k]; present {
			t.Errorf("absent %q was forwarded as %v", k, m.lastArgs[k])
		}
	}
}
