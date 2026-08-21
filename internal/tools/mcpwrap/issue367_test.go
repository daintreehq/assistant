package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// The issue #367 wrappers: the observation and verification actions that used to be
// reachable only through daintree.call. These tests pin the three things that make a
// wrapper worth having over the escape hatch — its POLICY (risk / parallelizability /
// confirmation), its FORWARDING (every argument spelling the host accepts, and nothing
// the caller did not send), and its RESULT SHAPING (a verdict the model can gate on,
// including the honest failure when the action answers with something else).

/* ------------------------------- helpers ---------------------------------- */

// structured returns an MCP envelope carrying obj as structuredContent — the channel
// Daintree uses for every action declaring mcpOutputSchema.
func structured(obj map[string]any) tools.MCPCallResult {
	return tools.MCPCallResult{StructuredContent: obj}
}

func run(t *testing.T, tool *tools.Tool, m *fakeMCP, args string) tools.ToolResult {
	t.Helper()
	return tool.Handle(context.Background(), json.RawMessage(args), ctxWith(m))
}

// decodeThenRun mirrors what DISPATCH does: the registry's Decode runs first (which is
// what enforces a Validator), then Handle. Wrapper tests that only call Handle would
// miss any rule that lives in Validate, so bound-checking tests go through here.
func decodeThenRun(t *testing.T, tool *tools.Tool, m *fakeMCP, args string) (tools.ToolResult, error) {
	t.Helper()
	canonical, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tool.Handle(context.Background(), canonical, ctxWith(m)), nil
}

func issue367Tools(t *testing.T) []*tools.Tool {
	t.Helper()
	return Tools(Deps{})
}

/* ------------------------------- policy ----------------------------------- */

// TestIssue367WrapperPolicy pins each new wrapper's risk class and concurrency opt-in.
//
// The read set is the point of the issue: those six answer ordinary questions and must
// NOT inherit daintree.call's system-tier confirmation. The two command-shaped ones are
// the deliberate exceptions, and their risk is evidence-based rather than name-based —
// project.runCheck and worktree.resource.status both execute shell on Daintree's side.
func TestIssue367WrapperPolicy(t *testing.T) {
	want := map[string]struct {
		risk     domain.RiskClass
		parallel bool
	}{
		"project.detectRunners":      {domain.RiskRead, true},
		"forge.listIssueComments":    {domain.RiskRead, true},
		"agentSessionHistory.list":   {domain.RiskRead, true},
		"browser.getConsoleMessages": {domain.RiskRead, true},
		"errors.recent":              {domain.RiskRead, true},
		"notifications.recent":       {domain.RiskRead, true},
		// Spawns a real child process (readOnlyHint:false, denyPluginDispatch alongside
		// terminal.sendCommand). Never parallel: concurrent checks contend for the same
		// build directory, and the RiskRead double-gate would refuse the flag anyway.
		"project.runCheck": {domain.RiskProject, false},
		// Executes the worktree's configured status command — arbitrary user shell. The
		// read-shaped NAME is exactly why this needs pinning: it is the case most likely
		// to be "corrected" to RiskRead by someone reading the name and not the host.
		"worktree.resource.status": {domain.RiskProject, false},
	}
	all := issue367Tools(t)
	for name, exp := range want {
		tool := findTool(all, name)
		if tool == nil {
			t.Errorf("%s is not registered by mcpwrap.Tools", name)
			continue
		}
		if tool.Risk != exp.risk {
			t.Errorf("%s risk = %s, want %s", name, tool.Risk, exp.risk)
		}
		if tool.Parallelizable != exp.parallel {
			t.Errorf("%s Parallelizable = %v, want %v", name, tool.Parallelizable, exp.parallel)
		}
		// Parallelizable is double-gated on RiskRead by the runner; a tool that sets it
		// without read risk would be silently demoted, so the pair must agree here.
		if tool.Parallelizable && tool.Risk != domain.RiskRead {
			t.Errorf("%s is Parallelizable but risk=%s — the double-gate would reject it", name, tool.Risk)
		}
		// Every mutating wrapper owes the approval sheet a plain-English effect line.
		if tool.Risk != domain.RiskRead && strings.TrimSpace(tool.Consequence) == "" {
			t.Errorf("%s is %s risk and must carry a Consequence line for the approval sheet", name, tool.Risk)
		}
		// Closed schemas only: an open one would let a typo through to Daintree as a
		// silently ignored key, which is the failure the wrapper exists to prevent.
		if !strings.Contains(string(tool.Schema), `"additionalProperties": false`) {
			t.Errorf("%s schema must set additionalProperties:false", name)
		}
	}
}

/* ----------------------------- project.runCheck --------------------------- */

func TestProjectRunCheckForwardsAndShapesVerdict(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"projectId": "p1", "cwd": "/repo", "runnerId": "r1", "runnerName": "test",
		"command": "go test ./...", "passed": false, "exitCode": float64(1),
		"signalName": nil, "durationMs": float64(4200), "timedOut": false,
		"aborted": false, "output": "FAIL", "outputTruncated": false,
	})}
	res := run(t, findTool(issue367Tools(t), "project.runCheck"), m,
		`{"projectId":"p1","runnerId":"r1","cwd":"/repo","timeoutMs":120000}`)

	// A FAILING check is a SUCCESSFUL call. Collapsing the two would make "the tests
	// failed" indistinguishable from "the check could not be run".
	if !res.Ok {
		t.Fatalf("a failing check must return ok with passed:false, got failure: %+v", res.Error)
	}
	out := res.Result.(map[string]any)
	if out["passed"] != false {
		t.Errorf("passed = %v, want false", out["passed"])
	}
	if !strings.Contains(res.Summary, "FAILED") {
		t.Errorf("summary must name the failure, got %q", res.Summary)
	}
	for _, k := range []string{"runnerName", "command", "exitCode", "durationMs", "timedOut", "aborted"} {
		if _, ok := out[k]; !ok {
			t.Errorf("verdict field %q was dropped", k)
		}
	}
	if m.lastName != "project.runCheck" {
		t.Errorf("forwarded to %q", m.lastName)
	}
	if m.lastArgs["cwd"] != "/repo" || m.lastArgs["projectId"] != "p1" || m.lastArgs["runnerId"] != "r1" {
		t.Errorf("args not forwarded verbatim: %+v", m.lastArgs)
	}
	if m.lastArgs["timeoutMs"] != 120000 {
		t.Errorf("timeoutMs = %v, want the caller's 120000", m.lastArgs["timeoutMs"])
	}
}

// TestProjectRunCheckOmitsUnsentTimeout proves the DEFAULT stays Daintree's. Forwarding
// our idea of it would pin a value the host is free to change, and the drift would be
// invisible — the call would keep succeeding with the wrong ceiling.
func TestProjectRunCheckOmitsUnsentTimeout(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"passed": true})}
	run(t, findTool(issue367Tools(t), "project.runCheck"), m, `{"projectId":"p1","runnerId":"r1"}`)
	if _, present := m.lastArgs["timeoutMs"]; present {
		t.Errorf("timeoutMs must not be forwarded when the caller omitted it, got %v", m.lastArgs["timeoutMs"])
	}
	if _, present := m.lastArgs["cwd"]; present {
		t.Errorf("cwd must not be forwarded when the caller omitted it")
	}
}

// TestProjectRunCheckWireDeadline is the whole reason tools.MCPCallOptions exists: the
// transport's 120s default would abort any real check long before it finished and report
// that abort as a tool error. The deadline must track the check's own budget plus the
// settlement margin Daintree needs to kill the tree and marshal a result.
func TestProjectRunCheckWireDeadline(t *testing.T) {
	tool := findTool(issue367Tools(t), "project.runCheck")
	cases := []struct {
		label string
		args  string
		want  time.Duration
	}{
		{"omitted uses the host default", `{"projectId":"p","runnerId":"r"}`,
			projectCheckDefaultTimeoutMS*time.Millisecond + projectCheckSettleMargin},
		{"explicit budget", `{"projectId":"p","runnerId":"r","timeoutMs":900000}`,
			900000*time.Millisecond + projectCheckSettleMargin},
		{"the host maximum", `{"projectId":"p","runnerId":"r","timeoutMs":3600000}`,
			projectCheckMaxTimeoutMS*time.Millisecond + projectCheckSettleMargin},
	}
	for _, c := range cases {
		m := &fakeMCP{connected: true, result: structured(map[string]any{"passed": true})}
		if _, err := decodeThenRun(t, tool, m, c.args); err != nil {
			t.Fatalf("%s: decode: %v", c.label, err)
		}
		if m.lastOpts.Timeout != c.want {
			t.Errorf("%s: wire timeout = %v, want %v", c.label, m.lastOpts.Timeout, c.want)
		}
		// The margin must be real headroom, never zero or negative: a deadline equal to
		// the check's own budget aborts during the host's kill-and-drain settlement.
		if m.lastOpts.Timeout <= projectCheckSettleMargin {
			t.Errorf("%s: wire timeout %v leaves no settlement headroom", c.label, m.lastOpts.Timeout)
		}
	}
}

// TestOnlyProjectRunCheckAsksForALongDeadline keeps the exception an exception. Every
// other wrapper must ride the transport default, so a future wrapper cannot acquire an
// hour-long deadline by copying a neighbour.
func TestOnlyProjectRunCheckAsksForALongDeadline(t *testing.T) {
	cases := map[string]string{
		"project.detectRunners":      `{}`,
		"forge.listIssueComments":    `{"issueNumber":1}`,
		"agentSessionHistory.list":   `{}`,
		"browser.getConsoleMessages": `{}`,
		"errors.recent":              `{}`,
		"notifications.recent":       `{}`,
		"worktree.resource.status":   `{}`,
	}
	all := issue367Tools(t)
	for name, args := range cases {
		m := &fakeMCP{connected: true, result: structured(map[string]any{
			"passed": true, "configured": false, "status": nil,
		})}
		run(t, findTool(all, name), m, args)
		if m.lastOpts.Timeout != 0 {
			t.Errorf("%s requested a wire timeout of %v; only project.runCheck may set one", name, m.lastOpts.Timeout)
		}
	}
}

func TestProjectRunCheckRejectsBadArgs(t *testing.T) {
	tool := findTool(issue367Tools(t), "project.runCheck")
	for _, args := range []string{
		`{"runnerId":"r"}`,                                     // missing projectId
		`{"projectId":"p"}`,                                    // missing runnerId
		`{"projectId":"  ","runnerId":"r"}`,                    // blank projectId
		`{"projectId":"p","runnerId":"r","timeoutMs":999}`,     // under the host minimum
		`{"projectId":"p","runnerId":"r","timeoutMs":3600001}`, // over the host maximum
		`{"projectId":"p","runnerId":"r","runner":"x"}`,        // unknown field
	} {
		m := &fakeMCP{connected: true, result: structured(map[string]any{"passed": true})}
		if _, err := decodeThenRun(t, tool, m, args); err == nil {
			t.Errorf("args %s were accepted; the declared bounds must be enforced", args)
		}
		if m.lastName != "" {
			t.Errorf("args %s reached Daintree as %q — validation must run BEFORE the call", args, m.lastName)
		}
	}
}

// TestProjectRunCheckTrimsOutputFromTheStart pins the one transformation this wrapper
// makes. The trim is from the FRONT because a runner prints its failure summary last,
// and it is ANNOUNCED, because an unannounced trim reads as the whole output — which is
// how a model concludes a suite passed from a fragment that never reached the errors.
func TestProjectRunCheckTrimsOutputFromTheStart(t *testing.T) {
	huge := strings.Repeat("a", projectCheckOutputBudget+500) + "TAIL-MARKER"
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"passed": false, "output": huge, "runnerName": "test",
	})}
	res := run(t, findTool(issue367Tools(t), "project.runCheck"), m, `{"projectId":"p","runnerId":"r"}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	out := res.Result.(map[string]any)
	got := out["output"].(string)
	if !strings.HasSuffix(got, "TAIL-MARKER") {
		t.Error("the TAIL must survive the trim — it holds the failure summary")
	}
	if len([]rune(got)) > projectCheckOutputBudget {
		t.Errorf("output is %d runes, over the %d budget", len([]rune(got)), projectCheckOutputBudget)
	}
	if out["outputTrimmedByAssistant"] != true {
		t.Error("a trim must be announced in the result, not left for the reader to notice")
	}
	if out["outputTrimmedFrom"] != "start" {
		t.Errorf("outputTrimmedFrom = %v, want \"start\"", out["outputTrimmedFrom"])
	}
	if !strings.Contains(res.Summary, "trimmed") {
		t.Errorf("the summary must mention the trim, got %q", res.Summary)
	}
}

func TestProjectRunCheckKeepsShortOutputWhole(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"passed": true, "output": "ok\n"})}
	res := run(t, findTool(issue367Tools(t), "project.runCheck"), m, `{"projectId":"p","runnerId":"r"}`)
	out := res.Result.(map[string]any)
	if out["output"] != "ok\n" {
		t.Errorf("short output must pass through verbatim, got %q", out["output"])
	}
	if out["outputTrimmedByAssistant"] != false {
		t.Error("outputTrimmedByAssistant must be false when nothing was dropped")
	}
	if _, present := out["outputCharsDropped"]; present {
		t.Error("outputCharsDropped must be absent when nothing was dropped")
	}
}

// TestProjectRunCheckTimedOutIsNotAVerdict: a killed check reports passed:false, which
// on its own reads as "the code is broken". It is not — it is "we never found out".
func TestProjectRunCheckTimedOutIsNotAVerdict(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"passed": false, "timedOut": true, "exitCode": nil, "runnerName": "e2e",
	})}
	res := run(t, findTool(issue367Tools(t), "project.runCheck"), m, `{"projectId":"p","runnerId":"r"}`)
	if !strings.Contains(res.Summary, "TIMED OUT") || !strings.Contains(res.Summary, "not a verdict") {
		t.Errorf("a timed-out check must say so and disclaim the verdict, got %q", res.Summary)
	}
}

// TestProjectRunCheckMalformedResultFailsHonestly: no `passed` means no verdict, and
// defaulting it either way would manufacture one out of a broken payload.
func TestProjectRunCheckMalformedResultFailsHonestly(t *testing.T) {
	for _, bad := range []tools.MCPCallResult{
		structured(map[string]any{"runnerName": "test"}), // no passed
		structured(map[string]any{"passed": "yes"}),      // wrong type
		{Text: "not json"}, // neither channel carries an object
	} {
		m := &fakeMCP{connected: true, result: bad}
		res := run(t, findTool(issue367Tools(t), "project.runCheck"), m, `{"projectId":"p","runnerId":"r"}`)
		if res.Ok {
			t.Errorf("a result with no readable verdict must fail, got ok: %+v", res.Result)
		}
	}
}

// TestStructuredFromFallsBackToTextJSON: actions without mcpOutputSchema answer with a
// JSON text block instead of structuredContent, and a wrapper that only read the
// structured channel would call those malformed.
func TestStructuredFromFallsBackToTextJSON(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: `{"errors":[{"id":"e1"}]}`}}
	res := run(t, findTool(issue367Tools(t), "errors.recent"), m, `{}`)
	if !res.Ok {
		t.Fatalf("a JSON text payload must be read, got failure: %+v", res.Error)
	}
	if got := len(res.Result.(map[string]any)["errors"].([]any)); got != 1 {
		t.Errorf("errors = %d, want 1", got)
	}
}

/* --------------------------- project.detectRunners ------------------------ */

func TestProjectDetectRunnersOptionalScope(t *testing.T) {
	tool := findTool(issue367Tools(t), "project.detectRunners")
	m := &fakeMCP{connected: true, result: structured(map[string]any{"runners": []any{map[string]any{"id": "r1"}}})}
	res := run(t, tool, m, `{}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	if _, present := m.lastArgs["projectId"]; present {
		t.Error("projectId must not be forwarded when omitted — the active project is Daintree's fallback")
	}
	m2 := &fakeMCP{connected: true, result: structured(map[string]any{"runners": []any{}})}
	run(t, tool, m2, `{"projectId":"p9"}`)
	if m2.lastArgs["projectId"] != "p9" {
		t.Errorf("projectId = %v, want p9", m2.lastArgs["projectId"])
	}
}

/* ------------------------ forge.listIssueComments ------------------------- */

// TestForgeListIssueCommentsForwardsEveryLocator: the wrapper blocks the raw path, so a
// locator spelling it cannot express is a spelling with no path at all. All three must
// cross, and precedence stays the forge's rule rather than being re-decided here.
func TestForgeListIssueCommentsForwardsEveryLocator(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"items": []any{}, "hasMore": false})}
	run(t, findTool(issue367Tools(t), "forge.listIssueComments"), m,
		`{"issueNumber":42,"cwd":"/a","worktreeId":"wt1","worktreePath":"/b","cursor":"","perPage":50}`)
	for k, want := range map[string]any{
		"issueNumber": 42, "cwd": "/a", "worktreeId": "wt1", "worktreePath": "/b", "perPage": 50,
	} {
		if m.lastArgs[k] != want {
			t.Errorf("%s = %v, want %v", k, m.lastArgs[k], want)
		}
	}
	// An EMPTY cursor is a value the host accepts and is not the same as no cursor,
	// which is why Cursor is a pointer rather than a string with omitempty.
	if c, present := m.lastArgs["cursor"]; !present || c != "" {
		t.Errorf("an explicitly empty cursor must be forwarded, got %v (present=%v)", c, present)
	}
}

func TestForgeListIssueCommentsOmitsUnsentPaging(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"items": []any{}})}
	run(t, findTool(issue367Tools(t), "forge.listIssueComments"), m, `{"issueNumber":7}`)
	for _, k := range []string{"cursor", "perPage", "cwd", "worktreeId", "worktreePath"} {
		if _, present := m.lastArgs[k]; present {
			t.Errorf("%s must not be forwarded when omitted", k)
		}
	}
}

// TestForgeListIssueCommentsSurfacesMoreToRead: comments arrive OLDEST first, so a model
// that stops at page one has read the least recent discussion. hasMore must be relayed
// and must never be defaulted — a defaulted false claims the thread is complete.
func TestForgeListIssueCommentsSurfacesMoreToRead(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"items": []any{map[string]any{"body": "one"}}, "hasMore": true, "nextCursor": "c2", "totalCount": float64(9),
	})}
	res := run(t, findTool(issue367Tools(t), "forge.listIssueComments"), m, `{"issueNumber":7}`)
	if !strings.Contains(res.Summary, "MORE") {
		t.Errorf("an unfinished thread must say so in the summary, got %q", res.Summary)
	}
	out := res.Result.(map[string]any)
	if out["hasMore"] != true || out["nextCursor"] != "c2" {
		t.Errorf("paging fields dropped: %+v", out)
	}

	m2 := &fakeMCP{connected: true, result: structured(map[string]any{"items": []any{}})}
	out2 := run(t, findTool(issue367Tools(t), "forge.listIssueComments"), m2, `{"issueNumber":7}`).Result.(map[string]any)
	if _, present := out2["hasMore"]; present {
		t.Error("hasMore must be ABSENT when the forge did not send it — a defaulted false would claim the thread is complete")
	}
}

func TestForgeListIssueCommentsRejectsBadArgs(t *testing.T) {
	tool := findTool(issue367Tools(t), "forge.listIssueComments")
	for _, args := range []string{
		`{}`, `{"issueNumber":0}`, `{"issueNumber":-1}`,
		`{"issueNumber":1,"perPage":0}`, `{"issueNumber":1,"perPage":101}`,
		`{"issueNumber":1,"page":2}`,
	} {
		m := &fakeMCP{connected: true, result: structured(map[string]any{"items": []any{}})}
		if _, err := decodeThenRun(t, tool, m, args); err == nil {
			t.Errorf("args %s were accepted", args)
		}
	}
}

/* ----------------------- agentSessionHistory.list ------------------------- */

func TestAgentSessionHistoryListScopeAndPaging(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"sessions": []any{map[string]any{"sessionId": "s1"}}, "total": float64(30), "hasMore": true,
	})}
	res := run(t, findTool(issue367Tools(t), "agentSessionHistory.list"), m,
		`{"worktreeId":"wt1","projectId":"p1","limit":10,"offset":20}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	for k, want := range map[string]any{"worktreeId": "wt1", "projectId": "p1", "limit": 10, "offset": 20} {
		if m.lastArgs[k] != want {
			t.Errorf("%s = %v, want %v", k, m.lastArgs[k], want)
		}
	}
	if !strings.Contains(res.Summary, "MORE") {
		t.Errorf("hasMore must reach the summary, got %q", res.Summary)
	}
	out := res.Result.(map[string]any)
	if out["total"] == nil || out["hasMore"] != true {
		t.Errorf("paging metadata dropped: %+v", out)
	}
}

func TestAgentSessionHistoryListEmptyObjectIsAllowed(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"sessions": []any{}})}
	if _, err := decodeThenRun(t, findTool(issue367Tools(t), "agentSessionHistory.list"), m, `{}`); err != nil {
		t.Fatalf("{} must be accepted — Daintree derives scope from the session: %v", err)
	}
	if len(m.lastArgs) != 0 {
		t.Errorf("nothing should be forwarded for {}, got %+v", m.lastArgs)
	}
}

// A BLANK id must be rejected, not forwarded. The host declares both ids `.min(1)`
// precisely because a blank one falls through its `if (!worktreeId)` guard and silently
// widens the listing to every project.
func TestAgentSessionHistoryListRejectsBlankScope(t *testing.T) {
	tool := findTool(issue367Tools(t), "agentSessionHistory.list")
	for _, args := range []string{
		`{"worktreeId":"   "}`, `{"projectId":" "}`,
		`{"limit":0}`, `{"limit":101}`, `{"offset":-1}`, `{"scope":"all"}`,
	} {
		m := &fakeMCP{connected: true, result: structured(map[string]any{"sessions": []any{}})}
		if _, err := decodeThenRun(t, tool, m, args); err == nil {
			t.Errorf("args %s were accepted", args)
		}
		if m.lastName != "" {
			t.Errorf("args %s reached Daintree — validation must run first", args)
		}
	}
}

/* ---------------------- browser.getConsoleMessages ------------------------ */

func TestBrowserGetConsoleMessagesForwardsAndCounts(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"paneId":   "pane-1",
		"messages": []any{map[string]any{"level": "error"}},
		"counts":   map[string]any{"errorCount": float64(7), "warnCount": float64(2)},
	})}
	res := run(t, findTool(issue367Tools(t), "browser.getConsoleMessages"), m,
		`{"terminalId":"t1","level":"error","limit":5}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	for k, want := range map[string]any{"terminalId": "t1", "level": "error", "limit": 5} {
		if m.lastArgs[k] != want {
			t.Errorf("%s = %v, want %v", k, m.lastArgs[k], want)
		}
	}
	// counts covers the PANE, not the filtered page — a reader comparing 7 errors to
	// 1 returned row must be told which number means what.
	if !strings.Contains(res.Summary, "pane totals") || !strings.Contains(res.Summary, "7 error") {
		t.Errorf("pane-wide counts must be attributed in the summary, got %q", res.Summary)
	}
}

func TestBrowserGetConsoleMessagesRejectsBadArgs(t *testing.T) {
	tool := findTool(issue367Tools(t), "browser.getConsoleMessages")
	for _, args := range []string{
		`{"level":"debug"}`, `{"level":"ERROR"}`, `{"limit":0}`, `{"limit":501}`, `{"pane":"x"}`,
	} {
		m := &fakeMCP{connected: true, result: structured(map[string]any{"messages": []any{}})}
		if _, err := decodeThenRun(t, tool, m, args); err == nil {
			t.Errorf("args %s were accepted", args)
		}
	}
}

func TestBrowserGetConsoleMessagesOmitsUnsent(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"messages": []any{}})}
	run(t, findTool(issue367Tools(t), "browser.getConsoleMessages"), m, `{}`)
	if len(m.lastArgs) != 0 {
		t.Errorf("nothing should be forwarded for {}, got %+v", m.lastArgs)
	}
}

/* ------------------- errors.recent / notifications.recent ----------------- */

func TestDiagnosticsWrappersForwardAndShape(t *testing.T) {
	errTool := findTool(issue367Tools(t), "errors.recent")
	m := &fakeMCP{connected: true, result: structured(map[string]any{
		"errors": []any{map[string]any{"id": "e1"}, map[string]any{"id": "e2"}},
	})}
	res := run(t, errTool, m, `{"limit":5,"includesDismissed":true}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	if m.lastArgs["limit"] != 5 || m.lastArgs["includesDismissed"] != true {
		t.Errorf("errors.recent args not forwarded: %+v", m.lastArgs)
	}
	if got := len(res.Result.(map[string]any)["errors"].([]any)); got != 2 {
		t.Errorf("errors = %d, want 2", got)
	}

	noteTool := findTool(issue367Tools(t), "notifications.recent")
	m2 := &fakeMCP{connected: true, result: structured(map[string]any{"notifications": []any{}})}
	res2 := run(t, noteTool, m2, `{"limit":50,"type":"error","unreadOnly":false}`)
	if !res2.Ok {
		t.Fatalf("unexpected failure: %+v", res2.Error)
	}
	if m2.lastArgs["type"] != "error" || m2.lastArgs["limit"] != 50 {
		t.Errorf("notifications.recent args not forwarded: %+v", m2.lastArgs)
	}
	// An explicit false must cross: it is distinguishable from omission by design.
	if v, present := m2.lastArgs["unreadOnly"]; !present || v != false {
		t.Errorf("an explicit unreadOnly:false must be forwarded, got %v (present=%v)", v, present)
	}
}

// The two stores answer one question between them, and neither payload hints that the
// other exists — so each result must name its sibling.
func TestDiagnosticsResultsNameTheOtherStore(t *testing.T) {
	all := issue367Tools(t)
	m := &fakeMCP{connected: true, result: structured(map[string]any{"errors": []any{}})}
	if s := run(t, findTool(all, "errors.recent"), m, `{}`).Summary; !strings.Contains(s, "notification") {
		t.Errorf("errors.recent must point at the notification inbox, got %q", s)
	}
	m2 := &fakeMCP{connected: true, result: structured(map[string]any{"notifications": []any{}})}
	if s := run(t, findTool(all, "notifications.recent"), m2, `{}`).Summary; !strings.Contains(s, "diagnostics") {
		t.Errorf("notifications.recent must point at the diagnostics log, got %q", s)
	}
}

func TestDiagnosticsWrappersRejectBadArgs(t *testing.T) {
	all := issue367Tools(t)
	cases := map[string][]string{
		"errors.recent":        {`{"limit":0}`, `{"limit":51}`, `{"dismissed":true}`},
		"notifications.recent": {`{"limit":0}`, `{"limit":51}`, `{"type":"debug"}`, `{"unread":true}`},
	}
	for name, argsList := range cases {
		for _, args := range argsList {
			m := &fakeMCP{connected: true, result: structured(map[string]any{})}
			if _, err := decodeThenRun(t, findTool(all, name), m, args); err == nil {
				t.Errorf("%s accepted %s", name, args)
			}
		}
	}
}

/* ------------------------ worktree.resource.status ------------------------ */

// `configured:false` is NOT a failure and NOT evidence the resource is down — the
// distinction the result has to carry, since both arrive as a successful call.
func TestWorktreeResourceStatusUnconfigured(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"configured": false, "status": nil})}
	res := run(t, findTool(issue367Tools(t), "worktree.resource.status"), m, `{}`)
	if !res.Ok {
		t.Fatalf("unexpected failure: %+v", res.Error)
	}
	if res.Result.(map[string]any)["configured"] != false {
		t.Error("configured must be relayed")
	}
	if !strings.Contains(res.Summary, "not evidence") {
		t.Errorf("an unconfigured resource must not read as a down one, got %q", res.Summary)
	}
	if len(m.lastArgs) != 0 {
		t.Errorf("nothing should be forwarded for {}, got %+v", m.lastArgs)
	}
}

func TestWorktreeResourceStatusReportsStatus(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"configured": true, "status": "running"})}
	res := run(t, findTool(issue367Tools(t), "worktree.resource.status"), m, `{"worktreeId":"wt1"}`)
	if !strings.Contains(res.Summary, "running") {
		t.Errorf("summary must carry the status, got %q", res.Summary)
	}
	if m.lastArgs["worktreeId"] != "wt1" {
		t.Errorf("worktreeId = %v", m.lastArgs["worktreeId"])
	}
}

// A missing `configured` decides how every other field reads, so defaulting it to false
// would claim "no status command is set up" on no evidence at all.
func TestWorktreeResourceStatusMalformedFailsHonestly(t *testing.T) {
	m := &fakeMCP{connected: true, result: structured(map[string]any{"status": "running"})}
	if res := run(t, findTool(issue367Tools(t), "worktree.resource.status"), m, `{}`); res.Ok {
		t.Errorf("a result without `configured` must fail, got ok: %+v", res.Result)
	}
}

/* ------------------------- shared MCP failure paths ----------------------- */

// Every wrapper must map a dead link to MCP_UNAVAILABLE (naming /reconnect) and a
// Daintree refusal to MCP_TOOL_ERROR — never to a shaped result that looks like data.
func TestIssue367WrappersMapMCPFailures(t *testing.T) {
	cases := map[string]string{
		"project.detectRunners":      `{}`,
		"project.runCheck":           `{"projectId":"p","runnerId":"r"}`,
		"forge.listIssueComments":    `{"issueNumber":1}`,
		"agentSessionHistory.list":   `{}`,
		"browser.getConsoleMessages": `{}`,
		"errors.recent":              `{}`,
		"notifications.recent":       `{}`,
		"worktree.resource.status":   `{}`,
	}
	all := issue367Tools(t)
	for name, args := range cases {
		res := run(t, findTool(all, name), &fakeMCP{connected: false}, args)
		if res.Ok || res.Error == nil || res.Error.Code != codeMCPUnavailable {
			t.Errorf("%s disconnected: want %s, got %+v", name, codeMCPUnavailable, res.Error)
		} else if !strings.Contains(res.Error.Message, "/reconnect") {
			t.Errorf("%s disconnected message must name /reconnect, got %q", name, res.Error.Message)
		}

		refused := &fakeMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "nope"}}
		res = run(t, findTool(all, name), refused, args)
		if res.Ok || res.Error == nil || res.Error.Code != codeMCPToolError {
			t.Errorf("%s refusal: want %s, got %+v", name, codeMCPToolError, res.Error)
		}
	}
}
