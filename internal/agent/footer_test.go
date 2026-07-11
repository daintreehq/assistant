package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
)

// ptrOf returns a pointer to v — the nullable WorkflowRunRecord fields (issue, branch,
// the JSON blobs) are pointers, so the footer tests need terse literal pointers.
func ptrOf[T any](v T) *T { return &v }

// anyContains reports whether any string in ss contains sub. The per-turn footer data now
// travels as structured slices (req.Turn.Memories.*, req.Turn.WorkflowRuns,
// req.Turn.ResumedWatchers); session-level tests assert a fact surfaced by scanning
// those rendered rows rather than a single prose footer string.
func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// fakeWorkflowLister is the WorkflowRunLister seam under test: it returns a fixed set
// of runs (or an error) and counts calls, so a session-level test can assert the
// footer reads the ledger every round and degrades gracefully on a read error.
type fakeWorkflowLister struct {
	runs  []domain.WorkflowRunRecord
	err   error
	calls int
	limit int // the limit arg of the most recent call
}

func (f *fakeWorkflowLister) ListNonTerminalWorkflowRuns(limit int) ([]domain.WorkflowRunRecord, error) {
	f.calls++
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.runs, nil
}

// withFooterSections swaps the package-local registry for the duration of one test
// and restores it afterwards, so registry-shape tests never bleed into each other.
// The swap is unsynchronized, so tests that call this MUST NOT use t.Parallel() —
// footerSections is a process-global with no mutex (it is write-once at init in
// production and never mutated at runtime).
func withFooterSections(t *testing.T, sections ...footerSection) {
	t.Helper()
	prev := footerSections
	t.Cleanup(func() { footerSections = prev })
	footerSections = sections
}

// footerBody asserts composeTurnFooter produced exactly one system message and
// returns its body — the common shape for the goal-anchor assertions.
func footerBody(t *testing.T, goal string) string {
	t.Helper()
	msgs := composeTurnFooter(footerContext{Goal: goal})
	if len(msgs) != 1 {
		t.Fatalf("composeTurnFooter(%q) returned %d messages, want 1", goal, len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("footer role = %q, want system", msgs[0].Role)
	}
	return msgs[0].StringContent
}

// An empty or whitespace-only goal yields NO footer at all, so the append is a
// no-op and the request is byte-identical to the pre-footer behaviour.
func TestComposeTurnFooter_BlankGoalEmitsNothing(t *testing.T) {
	for _, goal := range []string{"", "   ", "\n\t  \n"} {
		if got := composeTurnFooter(footerContext{Goal: goal}); got != nil {
			t.Errorf("composeTurnFooter(%q) = %v, want nil", goal, got)
		}
	}
}

// A normal goal produces a single system message carrying the `# Current goal`
// header, the verbatim ask, and an output-discipline line.
func TestComposeTurnFooter_GoalAnchorShape(t *testing.T) {
	body := footerBody(t, "fix the failing login test")
	if !strings.HasPrefix(body, "# Current goal\n") {
		t.Errorf("body does not start with the header; got %q", body)
	}
	if !strings.Contains(body, "fix the failing login test") {
		t.Errorf("body missing the goal text; got %q", body)
	}
	if !strings.Contains(body, "Stay focused on this goal") {
		t.Errorf("body missing the output-discipline line; got %q", body)
	}
}

// The goal is trimmed before it is embedded, so leading/trailing whitespace in the
// originating ask never bloats the anchor.
func TestComposeTurnFooter_TrimsGoal(t *testing.T) {
	body := footerBody(t, "   ship it   ")
	if !strings.Contains(body, "# Current goal\nship it\n") {
		t.Errorf("goal was not trimmed; got %q", body)
	}
}

// A goal at exactly the rune cap is preserved in full.
func TestComposeTurnFooter_AtCapNotTruncated(t *testing.T) {
	goal := strings.Repeat("a", goalAnchorMaxRunes)
	body := footerBody(t, goal)
	if !strings.Contains(body, goal) {
		t.Errorf("a goal of exactly %d runes was truncated", goalAnchorMaxRunes)
	}
}

// A goal past the rune cap is truncated to the cap — never beyond.
func TestComposeTurnFooter_TruncatesOverCap(t *testing.T) {
	goal := strings.Repeat("a", goalAnchorMaxRunes+1)
	body := footerBody(t, goal)
	if !strings.Contains(body, strings.Repeat("a", goalAnchorMaxRunes)) {
		t.Errorf("body should contain the first %d runes", goalAnchorMaxRunes)
	}
	if strings.Contains(body, strings.Repeat("a", goalAnchorMaxRunes+1)) {
		t.Errorf("body should NOT contain more than %d goal runes", goalAnchorMaxRunes)
	}
}

// Truncation is rune-safe: a multibyte ask past the cap is cut on a rune boundary,
// never mid-character, so the body stays valid UTF-8.
func TestComposeTurnFooter_TruncationIsRuneSafe(t *testing.T) {
	goal := strings.Repeat("世", goalAnchorMaxRunes+1)
	body := footerBody(t, goal)
	if !utf8.ValidString(body) {
		t.Error("truncated body is not valid UTF-8 (a rune was split)")
	}
	if !strings.Contains(body, strings.Repeat("世", goalAnchorMaxRunes)) {
		t.Errorf("body should contain the first %d runes", goalAnchorMaxRunes)
	}
	if strings.Contains(body, strings.Repeat("世", goalAnchorMaxRunes+1)) {
		t.Errorf("body should NOT contain more than %d goal runes", goalAnchorMaxRunes)
	}
}

// Multiple enabled sections coalesce into ONE system message, joined by a blank
// line, in registry order. This is the forward-compat path for later waves.
func TestComposeTurnFooter_JoinsMultipleSections(t *testing.T) {
	withFooterSections(t,
		func(footerContext) (string, bool) { return "SECTION-ONE", true },
		func(footerContext) (string, bool) { return "SECTION-TWO", true },
	)
	body := footerBody(t, "goal")
	if body != "SECTION-ONE\n\nSECTION-TWO" {
		t.Errorf("sections not joined by a blank line in order; got %q", body)
	}
}

// A disabled section (ok=false) and a blank-bodied section are both skipped, so a
// surviving section never carries a stray leading/trailing separator.
func TestComposeTurnFooter_SkipsDisabledAndBlankSections(t *testing.T) {
	withFooterSections(t,
		func(footerContext) (string, bool) { return "DROP-ME", false }, // disabled
		func(footerContext) (string, bool) { return "   ", true },      // blank body
		func(footerContext) (string, bool) { return "KEEP-ME", true },
	)
	body := footerBody(t, "goal")
	if body != "KEEP-ME" {
		t.Errorf("disabled/blank sections not cleanly skipped; got %q", body)
	}
}

// All sections skipped → no message at all.
func TestComposeTurnFooter_AllSectionsSkippedEmitsNothing(t *testing.T) {
	withFooterSections(t, func(footerContext) (string, bool) { return "x", false })
	if got := composeTurnFooter(footerContext{Goal: "goal"}); got != nil {
		t.Errorf("composeTurnFooter = %v, want nil when every section is skipped", got)
	}
}

// Session-level: the originating ask travels to the backend as STRUCTURED turn context
// (req.Turn.Goal — the backend renders the prose footer), never as a system message in the
// visible conversation, and never leaks into durable history.
func TestComposeTurnFooter_AppendedToStreamTail(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	before := len(s.Messages())
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(be.requests()) == 0 {
		t.Fatal("backend observed no rounds")
	}
	if got := be.turnAt(0).Goal; got != "do the thing" {
		t.Fatalf("request did not carry the goal as structured turn context; got %q", got)
	}

	// Ephemeral: durable history grows only by user + assistant (+2), and the visible
	// conversation carries only user/assistant/tool roles (no system footer message).
	if after := len(s.Messages()); after-before != 2 {
		t.Errorf("history grew by %d, want 2 (the footer is structured context, not history)", after-before)
	}
	for _, m := range s.Messages() {
		if m.Role == "system" {
			t.Fatalf("a system message leaked into durable visible history: %+v", m)
		}
	}
}

// Session-level: the structured goal is sent on EVERY round of a multi-round turn, always
// the same originating ask (not a mid-turn injection).
func TestComposeTurnFooter_RebuiltEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
		{Content: "final"}, // round 1: final answer
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "investigate the bug", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(be.requests()))
	}
	for i := 0; i < 2; i++ {
		if got := be.turnAt(i).Goal; got != "investigate the bug" {
			t.Errorf("round %d goal = %q, want the originating ask", i, got)
		}
	}
}

// Truncation keeps the PREFIX (first N runes), not an arbitrary slice or the suffix.
func TestComposeTurnFooter_TruncationKeepsPrefix(t *testing.T) {
	// 498 'a' + "XY" = exactly the first 500 runes; the trailing 'b's must be dropped.
	goal := strings.Repeat("a", goalAnchorMaxRunes-2) + "XY" + strings.Repeat("b", 10)
	body := footerBody(t, goal)
	want := strings.Repeat("a", goalAnchorMaxRunes-2) + "XY"
	if !strings.Contains(body, want) {
		t.Errorf("body should contain the first %d runes ending in XY", goalAnchorMaxRunes)
	}
	if strings.Contains(body, "XYb") {
		t.Error("truncation kept runes past the cap; it must keep the PREFIX, not the suffix")
	}
}

// Session-level: two sequential turns each carry their OWN originating ask in the
// structured turn context — never stale from a prior turn.
func TestComposeTurnFooter_DistinctGoalsAcrossSends(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "a"}, {Content: "b"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "goal-one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "goal-two", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(be.requests()))
	}
	if got := be.turnAt(0).Goal; got != "goal-one" {
		t.Errorf("send 1 goal = %q, want goal-one", got)
	}
	if got := be.turnAt(1).Goal; got != "goal-two" {
		t.Errorf("send 2 goal = %q, want goal-two", got)
	}
}

// Session-level: a mid-turn redirect folds into history as a user message, but the
// structured goal stays anchored to the ORIGINAL ask (it never chases the injection).
func TestComposeTurnFooter_StableAcrossMidTurnInjection(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
			{Content: "final"}, // round 1: final answer
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("stop, explain only")
			}
		},
	}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s = NewSession(deps)

	if _, err := s.Send(context.Background(), "original goal", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	if !userTextSeen(r.seen[1], "stop, explain only") {
		t.Error("round 1 should see the folded-in injection in history")
	}
	if got := be.turnAt(1).Goal; got != "original goal" {
		t.Errorf("round 1 goal = %q, want the original goal (never the injection)", got)
	}
}

// Session-level: a blank send carries an EMPTY structured goal (the backend then renders
// no goal anchor) — no goal data is fabricated from whitespace.
func TestComposeTurnFooter_BlankSendAppendsNoFooter(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) == 0 {
		t.Fatal("backend observed no rounds")
	}
	if got := be.turnAt(0).Goal; got != "" {
		t.Errorf("a blank send must carry an empty goal (trimmed), got %q", got)
	}
}

// ---- active-workflow-runs section: pure formatter ----

// No open runs ⇒ the section is omitted (false), so the common no-open-work case
// adds nothing to the request. Both nil and empty slices omit.
func TestActiveWorkflowRunsSection_EmptyRunsOmitsSection(t *testing.T) {
	for _, runs := range [][]domain.WorkflowRunRecord{nil, {}} {
		if body, ok := activeWorkflowRunsSection(footerContext{WorkflowRuns: runs}); ok || body != "" {
			t.Errorf("empty runs should omit the section; got (%q, %v)", body, ok)
		}
	}
}

// A fully-populated run renders the header plus a single line carrying every handle:
// status, id, issue, branch, the next-action label+tool, and the terminal/watcher ids.
func TestActiveWorkflowRunsSection_RendersExpectedShape(t *testing.T) {
	run := domain.WorkflowRunRecord{
		ID:              "wfr_ab12cd34",
		Status:          domain.WorkflowActive,
		IssueNumber:     ptrOf(255),
		Branch:          ptrOf("feature/issue-255-render"),
		NextActionJson:  ptrOf(`{"label":"Render workflow footer","toolName":"workflow.update"}`),
		TerminalIdsJson: ptrOf(`["term_a1b2","term_c3d4"]`),
		WatcherIdsJson:  ptrOf(`["wch_e5f6"]`),
	}
	body, ok := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if !ok {
		t.Fatal("a populated run must emit the section")
	}
	for _, want := range []string{
		"# Active workflow runs",
		"- [active] wfr_ab12cd34",
		"#255",
		"feature/issue-255-render",
		"Render workflow footer (workflow.update)",
		"terms: term_a1b2 term_c3d4",
		"watchers: wch_e5f6",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
	// Exactly one run ⇒ exactly one row line under the header.
	if got := strings.Count(body, "\n- ["); got != 1 {
		t.Errorf("want 1 run row, got %d; body:\n%s", got, body)
	}
}

// Malformed or absent optional blobs degrade to "none" and never panic — a footer
// must tolerate any ledger data, not assume a well-formed record.
func TestActiveWorkflowRunsSection_MalformedJsonDegrades(t *testing.T) {
	run := domain.WorkflowRunRecord{
		ID:              "wfr_bad",
		Status:          domain.WorkflowBlocked,
		NextActionJson:  ptrOf(`{not valid json`),
		TerminalIdsJson: ptrOf(`nope`),
		WatcherIdsJson:  nil,
	}
	body, ok := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if !ok {
		t.Fatal("section should still emit for a run with bad blobs")
	}
	if !strings.Contains(body, "→  none") {
		t.Errorf("malformed next-action should render 'none'; got:\n%s", body)
	}
	if !strings.Contains(body, "terms: none") {
		t.Errorf("malformed terminal ids should render 'none'; got:\n%s", body)
	}
	if !strings.Contains(body, "watchers: none") {
		t.Errorf("nil watcher ids should render 'none'; got:\n%s", body)
	}
}

// More runs than the cap render exactly activeWorkflowRunsLimit rows (defense in depth:
// the store already bounds the query, but the section never trusts an oversized slice).
func TestActiveWorkflowRunsSection_CapsAtLimit(t *testing.T) {
	runs := make([]domain.WorkflowRunRecord, activeWorkflowRunsLimit+1)
	for i := range runs {
		runs[i] = domain.WorkflowRunRecord{ID: "wfr_" + strconv.Itoa(i), Status: domain.WorkflowActive}
	}
	body, ok := activeWorkflowRunsSection(footerContext{WorkflowRuns: runs})
	if !ok {
		t.Fatal("section should emit")
	}
	if got := strings.Count(body, "\n- ["); got != activeWorkflowRunsLimit {
		t.Errorf("want %d rows (capped), got %d", activeWorkflowRunsLimit, got)
	}
}

// A run with no issue and no branch drops those fragments cleanly — the row still
// carries status, id, the next-action arrow, and the id lists.
func TestActiveWorkflowRunsSection_MissingIssueAndBranch(t *testing.T) {
	run := domain.WorkflowRunRecord{ID: "wfr_bare", Status: domain.WorkflowPending}
	body, ok := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if !ok {
		t.Fatal("section should emit for a bare run")
	}
	// The header carries a '#', so scope the no-issue check to the run row (after the
	// first newline) — a bare run must render no '#NNN' issue fragment there.
	if _, row, _ := strings.Cut(body, "\n"); strings.Contains(row, "#") {
		t.Errorf("bare run must not render an issue number; got row:\n%s", row)
	}
	for _, want := range []string{"- [pending] wfr_bare", "→  none", "terms: none", "watchers: none"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// An id list longer than the inline preview shows the first few ids then a "(+N)"
// count, so a run owning many terminals stays on one scannable line.
func TestActiveWorkflowRunsSection_IDListTruncatesWithCount(t *testing.T) {
	run := domain.WorkflowRunRecord{
		ID:              "wfr_many",
		Status:          domain.WorkflowActive,
		TerminalIdsJson: ptrOf(`["term_1","term_2","term_3","term_4","term_5"]`),
	}
	body, _ := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if !strings.Contains(body, "terms: term_1 term_2 term_3 (+2)") {
		t.Errorf("id list should preview 3 then (+2); got:\n%s", body)
	}
}

// A blank-entry / empty id array renders "none" rather than a stray separator.
func TestActiveWorkflowRunsSection_BlankIDsRenderNone(t *testing.T) {
	run := domain.WorkflowRunRecord{
		ID:              "wfr_blank",
		Status:          domain.WorkflowActive,
		TerminalIdsJson: ptrOf(`["","  "]`),
		WatcherIdsJson:  ptrOf(`[]`),
	}
	body, _ := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if !strings.Contains(body, "terms: none") || !strings.Contains(body, "watchers: none") {
		t.Errorf("blank/empty id arrays should render 'none'; got:\n%s", body)
	}
}

// A run whose ledger blobs carry embedded newlines (model-emitted tool args) must
// still render as exactly ONE row — a newline in toolName or a terminal id must not
// inject a second "- [..." line the model could mistake for a real run.
func TestActiveWorkflowRunsSection_SanitizesNewlinesToOneRow(t *testing.T) {
	run := domain.WorkflowRunRecord{
		ID:              "wfr_inj",
		Status:          domain.WorkflowActive,
		NextActionJson:  ptrOf("{\"label\":\"do it\",\"toolName\":\"workflow.update\\n- [active] wfr_fake\"}"),
		TerminalIdsJson: ptrOf("[\"term_ok\\n- [active] wfr_fake2\"]"),
	}
	body, _ := activeWorkflowRunsSection(footerContext{WorkflowRuns: []domain.WorkflowRunRecord{run}})
	if got := strings.Count(body, "\n- ["); got != 1 {
		t.Errorf("embedded newlines must not inject extra rows: want 1 row, got %d; body:\n%s", got, body)
	}
}

// Even with a blank goal (no goal anchor), a non-empty run set still emits the
// workflow section — the section's omit guard depends only on the runs, not the goal.
func TestComposeTurnFooter_BlankGoalWithActiveRunsEmitsWorkflowSection(t *testing.T) {
	runs := []domain.WorkflowRunRecord{{ID: "wfr_x", Status: domain.WorkflowActive}}
	msgs := composeTurnFooter(footerContext{Goal: "", WorkflowRuns: runs})
	if len(msgs) != 1 {
		t.Fatalf("want exactly one system message, got %d", len(msgs))
	}
	body := msgs[0].StringContent
	if strings.Contains(body, "# Current goal") {
		t.Errorf("blank goal must not emit the goal anchor; got:\n%s", body)
	}
	if !strings.Contains(body, "# Active workflow runs") || !strings.Contains(body, "wfr_x") {
		t.Errorf("workflow section should be present; got:\n%s", body)
	}
}

// ---- active-workflow-runs section: session-level wiring ----

// The footer reads the ledger and ships the open runs as structured rows.
func TestComposeTurnFooter_WorkflowRunsAppearInStreamTail(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{ID: "wfr_live", Status: domain.WorkflowActive, IssueNumber: ptrOf(42)},
	}}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	rows := strings.Join(be.turnAt(0).WorkflowRuns, "\n")
	if !strings.Contains(rows, "wfr_live") || !strings.Contains(rows, "#42") {
		t.Errorf("structured workflow rows missing id/issue; got %q", rows)
	}
}

// A ledger read error is swallowed: the turn completes normally and the request simply
// carries no workflow rows (the goal still travels).
func TestComposeTurnFooter_WorkflowRunsListerError_DoesNotFailSend(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{err: errors.New("db down")}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatalf("a ledger read error must not fail the send: %v", err)
	}
	turn := be.turnAt(0)
	if turn.Goal != "carry on" {
		t.Errorf("goal should still travel; got %q", turn.Goal)
	}
	if len(turn.WorkflowRuns) != 0 {
		t.Errorf("a failed read must carry no workflow rows; got %v", turn.WorkflowRuns)
	}
}

// The footer re-reads the ledger every round of a multi-round turn (state changes as
// tools run), and always with the activeWorkflowRunsLimit bound.
func TestComposeTurnFooter_WorkflowRunsReadEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	lister := &fakeWorkflowLister{}
	deps.WorkflowRunLister = lister
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "investigate", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if lister.calls != len(be.requests()) {
		t.Errorf("lister should be read once per round: calls=%d rounds=%d", lister.calls, len(be.requests()))
	}
	if lister.limit != activeWorkflowRunsLimit {
		t.Errorf("footer read should be bounded by activeWorkflowRunsLimit=%d, got %d", activeWorkflowRunsLimit, lister.limit)
	}
}

// Session-level: a blank send with OPEN runs still carries the workflow rows (the goal is
// blank, but the workflow data depends only on the runs). Closes the gap in
// TestComposeTurnFooter_BlankSendAppendsNoFooter, which has no lister wired.
func TestComposeTurnFooter_BlankSendWithActiveRunsAppendsWorkflowOnly(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{ID: "wfr_b", Status: domain.WorkflowActive},
	}}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	turn := be.turnAt(0)
	if !anyContains(turn.WorkflowRuns, "wfr_b") {
		t.Fatalf("blank send with open runs should still carry the workflow rows; got %v", turn.WorkflowRuns)
	}
	if turn.Goal != "" {
		t.Errorf("a blank goal must travel empty; got %q", turn.Goal)
	}
}

// ---- pinned + relevant memories section ----

// memRec builds a MemoryRecord carrying only Content — all the recall section reads.
func memRec(content string) domain.MemoryRecord {
	return domain.MemoryRecord{Content: content}
}

// fakeMemoryRecaller satisfies agent.MemoryRecaller: it records every recall call
// (so a test can assert "recalled exactly once per turn") and returns a canned
// result or error.
type fakeMemoryRecaller struct {
	rows    []domain.MemoryRecord
	err     error
	calls   int
	queries []string
	limits  []int
}

func (f *fakeMemoryRecaller) RecallMemories(query string, limit int) ([]domain.MemoryRecord, error) {
	f.calls++
	f.queries = append(f.queries, query)
	f.limits = append(f.limits, limit)
	return f.rows, f.err
}

// No pinned and no recalled rows ⇒ the merged section is omitted entirely.
func TestPinnedAndRelevantMemoriesSection_BothEmpty(t *testing.T) {
	if body, ok := pinnedAndRelevantMemoriesSection(footerContext{Goal: "g"}); ok || body != "" {
		t.Errorf("no memories should omit the section; got (%q, %v)", body, ok)
	}
	if body, ok := pinnedAndRelevantMemoriesSection(footerContext{
		PinnedMemories: []domain.MemoryRecord{}, RelevantMemories: []domain.MemoryRecord{},
	}); ok || body != "" {
		t.Errorf("empty slices should omit the section; got (%q, %v)", body, ok)
	}
}

// Recalled-only input renders just the `## Relevant` subblock under the merged header.
func TestPinnedAndRelevantMemoriesSection_RecalledOnly(t *testing.T) {
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{RelevantMemories: []domain.MemoryRecord{
		memRec("fact one"), memRec("  fact two  "),
	}})
	if !ok {
		t.Fatal("section should render when recalled rows are present")
	}
	want := "# Pinned and relevant memories\n## Relevant (recalled for this turn)\n- fact one\n- fact two"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Pinned-only input renders just the `## Pinned` subblock under the merged header.
func TestPinnedAndRelevantMemoriesSection_PinnedOnly(t *testing.T) {
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{PinnedMemories: []domain.MemoryRecord{
		memRec("pinned fact"),
	}})
	if !ok {
		t.Fatal("section should render when pinned rows are present")
	}
	want := "# Pinned and relevant memories\n## Pinned\n" + pinnedMemoriesFraming + "\n- pinned fact"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Both present ⇒ the `## Pinned` subblock (curated facts) comes FIRST, then `## Relevant`,
// each under its own subhead so the model can weight curated vs speculative differently.
func TestPinnedAndRelevantMemoriesSection_BothPresent(t *testing.T) {
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{
		PinnedMemories:   []domain.MemoryRecord{memRec("the pin")},
		RelevantMemories: []domain.MemoryRecord{memRec("the recall")},
	})
	if !ok {
		t.Fatal("section should render")
	}
	want := "# Pinned and relevant memories\n## Pinned\n" + pinnedMemoriesFraming + "\n- the pin\n## Relevant (recalled for this turn)\n- the recall"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Embedded newlines (\n, \r\n, \r) in a memory are flattened to spaces so one memory is
// exactly one list line — a raw newline must never split a fact or inject a heading.
func TestPinnedAndRelevantMemoriesSection_FlattensNewlines(t *testing.T) {
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{RelevantMemories: []domain.MemoryRecord{
		memRec("line one\nline two\r\nline three\rline four"),
	}})
	if !ok {
		t.Fatal("section should render")
	}
	want := "# Pinned and relevant memories\n## Relevant (recalled for this turn)\n- line one line two line three line four"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// Whitespace-only memories are skipped; if every row in a subblock is blank that subblock
// (and its subhead) is omitted, and a blank row never suppresses a real fact after it.
func TestPinnedAndRelevantMemoriesSection_SkipsBlankRows(t *testing.T) {
	if body, ok := pinnedAndRelevantMemoriesSection(footerContext{RelevantMemories: []domain.MemoryRecord{
		memRec("   "), memRec("\n\t"),
	}}); ok || body != "" {
		t.Errorf("all-blank rows should omit the section; got (%q, %v)", body, ok)
	}
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{RelevantMemories: []domain.MemoryRecord{
		memRec("  "), memRec("real fact"),
	}})
	if !ok || body != "# Pinned and relevant memories\n## Relevant (recalled for this turn)\n- real fact" {
		t.Errorf("blank row should be skipped but real fact kept; got (%q, %v)", body, ok)
	}
}

// renderMemoryBullets keeps at most maxRows rows (the FIRST, highest-rank) and drops the
// overflow — shared bound for both the pinned and recalled subblocks.
func TestRenderMemoryBullets_RowCap(t *testing.T) {
	var rows []domain.MemoryRecord
	for i := 0; i < relevantMemoriesMaxRows+3; i++ {
		rows = append(rows, memRec("fact "+strconv.Itoa(i)))
	}
	body := renderMemoryBullets(rows, relevantMemoriesMaxRows, relevantMemoriesBlockMaxBytes)
	if got := strings.Count(body, "- "); got != relevantMemoriesMaxRows {
		t.Errorf("rendered %d rows, want the cap of %d", got, relevantMemoriesMaxRows)
	}
	if !strings.Contains(body, "- fact "+strconv.Itoa(relevantMemoriesMaxRows-1)) {
		t.Error("the last in-cap row should be present")
	}
	if strings.Contains(body, "- fact "+strconv.Itoa(relevantMemoriesMaxRows)) {
		t.Error("a row past the cap must not be rendered")
	}
}

// renderMemoryBullets: when EVERY row exceeds the byte cap the result is empty — there is
// no lower-ranked fallback (storage returns exactly the top-N).
func TestRenderMemoryBullets_AllOversizedEmpty(t *testing.T) {
	huge := strings.Repeat("y", relevantMemoriesBlockMaxBytes+1)
	var rows []domain.MemoryRecord
	for i := 0; i < relevantMemoriesMaxRows; i++ {
		rows = append(rows, memRec(huge))
	}
	if body := renderMemoryBullets(rows, relevantMemoriesMaxRows, relevantMemoriesBlockMaxBytes); body != "" {
		t.Errorf("all-oversized rows should render empty; got %d bytes", len(body))
	}
}

// renderMemoryBullets: an oversized row is SKIPPED (continue, not break), so a shorter
// row AFTER the oversized one still renders, and the payload stays within the byte cap.
func TestRenderMemoryBullets_ByteCapSkipsOverflow(t *testing.T) {
	huge := strings.Repeat("x", relevantMemoriesBlockMaxBytes+100)
	body := renderMemoryBullets([]domain.MemoryRecord{
		memRec("small before"), memRec(huge), memRec("small after"),
	}, relevantMemoriesMaxRows, relevantMemoriesBlockMaxBytes)
	if strings.Contains(body, huge) {
		t.Error("an oversized memory must be skipped, not rendered")
	}
	if !strings.Contains(body, "- small before") || !strings.Contains(body, "- small after") {
		t.Errorf("a row after the oversized one must still render; got %q", body)
	}
	if len(body) > relevantMemoriesBlockMaxBytes {
		t.Errorf("payload is %d bytes, exceeds the cap of %d", len(body), relevantMemoriesBlockMaxBytes)
	}
}

// The pinned subblock uses the larger pinned byte cap, so a curated fact that would be
// dropped by the (smaller) recalled cap still renders when pinned — proving the two
// subblocks are bounded independently.
func TestPinnedAndRelevantMemoriesSection_PinnedUsesLargerCap(t *testing.T) {
	mid := strings.Repeat("p", relevantMemoriesBlockMaxBytes+200) // > recalled cap, < pinned cap
	body, ok := pinnedAndRelevantMemoriesSection(footerContext{
		PinnedMemories: []domain.MemoryRecord{memRec(mid)},
	})
	if !ok || !strings.Contains(body, mid) {
		t.Errorf("a pin within the pinned cap should render; got ok=%v len=%d", ok, len(body))
	}
	// The same content as a recalled row would be dropped by the smaller recalled cap.
	if got := renderMemoryBullets([]domain.MemoryRecord{memRec(mid)}, relevantMemoriesMaxRows, relevantMemoriesBlockMaxBytes); got != "" {
		t.Errorf("content over the recalled cap should be dropped there; got %d bytes", len(got))
	}
}

// Session-level: the recaller runs EXACTLY ONCE per turn (not per round), seeded by
// the originating ask, and the recalled facts appear in the footer of EVERY round of
// a multi-round turn without ever leaking into durable history.
func TestComposeTurnFooter_RecalledMemoriesInEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	rec := &fakeMemoryRecaller{rows: []domain.MemoryRecord{{Content: "the deploy key lives in vault"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "where is the deploy key", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Errorf("recaller called %d times, want exactly 1 per turn", rec.calls)
	}
	if len(rec.queries) > 0 && rec.queries[0] != "where is the deploy key" {
		t.Errorf("recall seeded with %q, want the originating ask", rec.queries[0])
	}
	if len(rec.limits) > 0 && rec.limits[0] != relevantMemoriesMaxRows {
		t.Errorf("recall limit = %d, want %d", rec.limits[0], relevantMemoriesMaxRows)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(be.requests()))
	}
	for i := 0; i < 2; i++ {
		mem := be.turnAt(i).Memories
		if mem == nil || !anyContains(mem.Relevant, "the deploy key lives in vault") {
			t.Errorf("round %d structured turn missing the recalled fact: %+v", i, mem)
		}
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "the deploy key lives in vault") {
			t.Fatal("recalled memory leaked into durable history; it must stay structured-only")
		}
	}
}

// Session-level: a recall error is swallowed — the turn still runs and the request
// carries the goal but NO recalled memories.
func TestComposeTurnFooter_RecallErrorSwallowed(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	rec := &fakeMemoryRecaller{err: errors.New("fts boom")}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Errorf("recaller called %d times, want 1", rec.calls)
	}
	turn := be.turnAt(0)
	if turn.Goal != "do the thing" {
		t.Errorf("goal should still travel; got %q", turn.Goal)
	}
	if turn.Memories != nil && len(turn.Memories.Relevant) != 0 {
		t.Errorf("a recall error must carry no relevant memories; got %v", turn.Memories.Relevant)
	}
}

// Session-level: a nil MemoryRecaller (the test default) carries NO relevant memories —
// the recall path is fully optional.
func TestComposeTurnFooter_NilRecallerOmitsMemories(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if mem := be.turnAt(0).Memories; mem != nil && len(mem.Relevant) != 0 {
		t.Errorf("nil recaller must carry no relevant memories; got %+v", mem)
	}
}

// Session-level: a blank/whitespace-only send short-circuits recall entirely — the
// recaller is NEVER called (no wasted query) and no recalled memories travel.
func TestComposeTurnFooter_BlankSendSkipsRecall(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	rec := &fakeMemoryRecaller{rows: []domain.MemoryRecord{{Content: "should not surface"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 0 {
		t.Errorf("recaller called %d times on a blank send, want 0", rec.calls)
	}
	if mem := be.turnAt(0).Memories; mem != nil && len(mem.Relevant) != 0 {
		t.Errorf("a blank send must carry no relevant memories; got %+v", mem)
	}
}

// Session-level: a mid-turn injection (which adds a round) does NOT trigger a second
// recall — recall is once-per-turn, seeded by the originating ask, and deliberately
// does not chase injections (mirroring the goal anchor's non-chasing).
func TestComposeTurnFooter_RecallNotRepeatedOnInjection(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
			{Content: "final"}, // round 1
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("also check the logs")
			}
		},
	}
	rec := &fakeMemoryRecaller{rows: []domain.MemoryRecord{{Content: "recalled fact"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s = NewSession(deps)

	if _, err := s.Send(context.Background(), "original ask", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Errorf("recaller called %d times across an injected multi-round turn, want exactly 1", rec.calls)
	}
	if len(rec.queries) > 0 && rec.queries[0] != "original ask" {
		t.Errorf("recall seeded with %q, want the originating ask (never the injection)", rec.queries[0])
	}
}

// ---- active worktree section ----

// An empty worktree label omits the section; a known label renders the `# Active worktree`
// line verbatim.
func TestActiveWorktreeSection(t *testing.T) {
	if body, ok := activeWorktreeSection(footerContext{}); ok || body != "" {
		t.Errorf("empty worktree should omit the section; got (%q, %v)", body, ok)
	}
	body, ok := activeWorktreeSection(footerContext{ActiveWorktree: "feature/issue-263"})
	if !ok || body != "# Active worktree\nfeature/issue-263" {
		t.Errorf("body = %q, ok = %v", body, ok)
	}
}

// A multi-line worktree label is flattened to one line so it can't inject a stray heading.
func TestActiveWorktreeSection_FlattensMultiline(t *testing.T) {
	body, ok := activeWorktreeSection(footerContext{ActiveWorktree: "feature/x\nrogue heading"})
	if !ok || body != "# Active worktree\nfeature/x rogue heading" {
		t.Errorf("multiline label not flattened; got (%q, %v)", body, ok)
	}
}

// ---- session note section ----

// No titles omit the section; one watcher renders a singular, quoted note.
func TestSessionNoteSection(t *testing.T) {
	if body, ok := sessionNoteSection(footerContext{}); ok || body != "" {
		t.Errorf("no titles should omit the section; got (%q, %v)", body, ok)
	}
	body, ok := sessionNoteSection(footerContext{ResumedWatchers: []string{"watch deploy"}})
	if !ok || !strings.HasPrefix(body, "# Session note\n") {
		t.Fatalf("section should render with the header; got (%q, %v)", body, ok)
	}
	if !strings.Contains(body, "1 watcher from a previous session is still running") || !strings.Contains(body, `"watch deploy"`) {
		t.Errorf("singular note missing; got %q", body)
	}
}

// Many watchers render a plural note with the title list capped and a "+N more" tail.
func TestSessionNoteSection_PluralAndCap(t *testing.T) {
	var titles []string
	for i := 0; i < resumedWatchersMaxTitles+2; i++ {
		titles = append(titles, "w"+strconv.Itoa(i))
	}
	body, ok := sessionNoteSection(footerContext{ResumedWatchers: titles})
	if !ok {
		t.Fatal("section should render")
	}
	if !strings.Contains(body, strconv.Itoa(len(titles))+" watchers from a previous session are still running") {
		t.Errorf("plural count missing; got %q", body)
	}
	if !strings.Contains(body, "+2 more") {
		t.Errorf("expected the '+2 more' tail; got %q", body)
	}
}

// ---- goal-anchor wake fallback ----

// On a wake turn the anchor substitutes the active-workflow objective (the first open
// run's next-action label) and does NOT echo the verbose wake blob.
func TestGoalAnchorSection_WakeUsesWorkflowObjective(t *testing.T) {
	body, ok := goalAnchorSection(footerContext{
		Goal:   wakePromptPrefix + " a watcher fired while you were idle",
		IsWake: true,
		WorkflowRuns: []domain.WorkflowRunRecord{
			{NextActionJson: ptrOf(`{"label":"finish the migration","toolName":"agentTask.spawnForEdits"}`)},
		},
	})
	if !ok {
		t.Fatal("a wake turn with an open run should render the objective anchor")
	}
	if !strings.Contains(body, "# Current objective") || !strings.Contains(body, "finish the migration") {
		t.Errorf("expected the objective anchor; got %q", body)
	}
	if strings.Contains(body, "# Current goal") || strings.Contains(body, "a watcher fired") {
		t.Errorf("a wake turn must NOT echo the verbose wake blob; got %q", body)
	}
}

// A wake turn with no open run (or a run whose next-action has no label) omits the anchor
// entirely — the wake blob is already in history as the user message.
func TestGoalAnchorSection_WakeWithoutObjectiveOmitted(t *testing.T) {
	if body, ok := goalAnchorSection(footerContext{Goal: wakePromptPrefix + " x", IsWake: true}); ok || body != "" {
		t.Errorf("a wake turn with no open run should omit the anchor; got (%q, %v)", body, ok)
	}
	if body, ok := goalAnchorSection(footerContext{
		Goal: wakePromptPrefix + " x", IsWake: true,
		WorkflowRuns: []domain.WorkflowRunRecord{{NextActionJson: ptrOf(`{"toolName":"x"}`)}},
	}); ok || body != "" {
		t.Errorf("a wake run with no objective label should omit the anchor; got (%q, %v)", body, ok)
	}
}

// A normal (non-wake) turn renders the standard `# Current goal` anchor from the ask.
func TestGoalAnchorSection_NonWakeUnchanged(t *testing.T) {
	body, ok := goalAnchorSection(footerContext{Goal: "do the thing"})
	if !ok || !strings.Contains(body, "# Current goal") || !strings.Contains(body, "do the thing") {
		t.Errorf("a normal turn should render the goal anchor; got (%q, %v)", body, ok)
	}
}

// ---- global footer budget ----

// When the joined footer exceeds footerMaxBytes, whole sections are dropped from the
// FRONT (lowest salience) until it fits; the last (highest-salience) section is kept.
func TestComposeTurnFooter_GlobalBudgetDropsFrontSections(t *testing.T) {
	big := strings.Repeat("a", 7000) // two of these alone exceed footerMaxBytes (12288)
	withFooterSections(t,
		func(footerContext) (string, bool) { return "FRONT-" + big, true },
		func(footerContext) (string, bool) { return "MIDDLE-" + big, true },
		func(footerContext) (string, bool) { return "TAIL-KEEP", true },
	)
	body := footerBody(t, "goal")
	if strings.Contains(body, "FRONT-") {
		t.Error("the front (lowest-salience) section should be dropped under the global budget")
	}
	if !strings.Contains(body, "TAIL-KEEP") {
		t.Error("the highest-salience (last) section must be kept")
	}
	if len(body) > footerMaxBytes {
		t.Errorf("trimmed footer is %d bytes, exceeds the budget of %d", len(body), footerMaxBytes)
	}
}

// A single section larger than the budget is kept rather than producing an empty footer
// (the trim never drops the final, highest-salience section).
func TestComposeTurnFooter_GlobalBudgetKeepsLastSection(t *testing.T) {
	withFooterSections(t, func(footerContext) (string, bool) {
		return "ONLY-" + strings.Repeat("z", footerMaxBytes), true
	})
	body := footerBody(t, "goal")
	if !strings.Contains(body, "ONLY-") {
		t.Errorf("the sole section must be kept even when it alone exceeds the budget; got %d bytes", len(body))
	}
}

// ---- session-level wiring of the new footer inputs ----

// fakePinnedLister satisfies agent.PinnedMemoryLister: it counts calls (so a test can
// assert the footer reads pins every ROUND, not once per turn) and returns canned rows.
type fakePinnedLister struct {
	rows  []domain.MemoryRecord
	err   error
	calls int
	limit int
}

func (f *fakePinnedLister) ListPinnedMemories(limit int) ([]domain.MemoryRecord, error) {
	f.calls++
	f.limit = limit
	return f.rows, f.err
}

// Session-level: pinned memories are re-read EVERY round (so a mid-turn pin surfaces next
// round), appear under the merged section's `## Pinned` subhead, and never leak into
// durable history.
func TestComposeTurnFooter_PinnedMemoriesEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	pinned := &fakePinnedLister{rows: []domain.MemoryRecord{{Content: "always use rtk proxy"}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.PinnedMemoryLister = pinned
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if pinned.calls < 2 {
		t.Errorf("pinned lister called %d times, want once per round (>= 2)", pinned.calls)
	}
	if pinned.limit != pinnedMemoriesMaxRows {
		t.Errorf("pinned limit = %d, want %d", pinned.limit, pinnedMemoriesMaxRows)
	}
	for i := 0; i < 2; i++ {
		mem := be.turnAt(i).Memories
		if mem == nil || !anyContains(mem.Pinned, "always use rtk proxy") {
			t.Errorf("round %d structured turn missing the pinned fact: %+v", i, mem)
		}
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "always use rtk proxy") {
			t.Fatal("pinned memory leaked into durable history; it must stay structured-only")
		}
	}
}

// Session-level: a pinned-lister error is swallowed — the turn still runs, the goal
// survives, and the pinned memories are omitted.
func TestComposeTurnFooter_PinnedListerErrorSwallowed(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	pinned := &fakePinnedLister{err: errors.New("db boom")}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.PinnedMemoryLister = pinned
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	turn := be.turnAt(0)
	if turn.Goal != "do the thing" {
		t.Errorf("goal should still travel; got %q", turn.Goal)
	}
	if turn.Memories != nil && len(turn.Memories.Pinned) != 0 {
		t.Error("a pinned-lister error must carry no pinned memories")
	}
}

// Session-level: the typed worktree travels in the structured runtime context on
// EVERY round — served from the cross-turn cache (fetched ONCE, detached, at turn
// start), never re-read inline per round.
func TestComposeTurnFooter_TypedWorktreeEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
	}}
	var calls atomic.Int32
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		calls.Add(1)
		return &prompts.WorktreeContext{Present: true, Branch: "feature/issue-263"}
	}
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork() // join the detached refresh before reading its call count
	if got := calls.Load(); got != 1 {
		t.Errorf("worktree fetcher entered %d times, want exactly 1 (cached, never per round)", got)
	}
	for i := 0; i < 2; i++ {
		got := be.runtimeAt(i).Worktree
		if got == nil || got.Current == nil || got.Current.Branch != "feature/issue-263" {
			t.Errorf("round %d runtime worktree = %+v, want feature/issue-263", i, got)
		}
	}
}

// Session-level: the one-time session note surfaces on the FIRST turn only — the provider
// is consulted exactly once and the note is gone from the second turn's request.
func TestComposeTurnFooter_SessionNoteFirstTurnOnly(t *testing.T) {
	r := &injectRouter{} // empty results → each Send is a single final round
	called := 0
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.ResumedWatchers = func() []string { called++; return []string{"watch the deploy"} }
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "first", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "second", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want >= 2 rounds across two sends, got %d", len(be.requests()))
	}
	if !anyContains(be.turnAt(0).ResumedWatchers, "watch the deploy") {
		t.Errorf("first-turn request should carry the session note; got %v", be.turnAt(0).ResumedWatchers)
	}
	if len(be.turnAt(1).ResumedWatchers) != 0 {
		t.Errorf("second-turn request must NOT repeat the session note; got %v", be.turnAt(1).ResumedWatchers)
	}
	if called != 1 {
		t.Errorf("resumed-watchers provider called %d times, want exactly 1 (first turn only)", called)
	}
}

// Session-level: the one-time session note rides EVERY round of the first turn (the
// per-turn context is rebuilt per round) — the multi-round complement to the
// first-turn-only test.
func TestComposeTurnFooter_SessionNoteRidesEveryRoundOfFirstTurn(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.ResumedWatchers = func() []string { return []string{"watch the deploy"} }
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "first", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want a 2-round first turn, got %d", len(be.requests()))
	}
	for i := 0; i < 2; i++ {
		if !anyContains(be.turnAt(i).ResumedWatchers, "watch the deploy") {
			t.Errorf("round %d of the first turn should carry the session note: %v", i, be.turnAt(i).ResumedWatchers)
		}
	}
}

// Session-level (issue #263 core claim): volatile state (the active worktree + pins)
// travels as PER-TURN structured context, never baked into the cached conversation — so a
// mid-session worktree/pin change is reflected on the next request without rewriting any
// durable history. (The old message[1] runtime-context block is gone; the backend owns the
// system prefix and the CLI ships these as data on every request.)
func TestComposeTurnFooter_RuntimeContextStableAcrossVolatileChanges(t *testing.T) {
	r := &injectRouter{} // empty results → each Send is a single final round
	wt := "feature/one"
	pins := []domain.MemoryRecord{{Content: "pin A"}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		return &prompts.WorktreeContext{Present: true, Branch: wt}
	}
	deps.PinnedMemoryLister = &fakePinnedLister{rows: pins}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "first", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// Mutate the volatile inputs (same backing array / captured var the fakes read), then
	// run a second turn. The worktree snapshot is TTL-cached across turns now, so make
	// the change observable deterministically: wait for turn 1's detached refresh to
	// land, then expire the cache — turn 2's warm re-fetches and picks up feature/two
	// (in production the same happens ≤ worktreeSnapshotTTL later).
	waitForWorktreeIdle(t, s)
	wt = "feature/two"
	expireWorktreeCache(s)
	pins[0] = domain.MemoryRecord{Content: "pin B totally different"}
	if _, err := s.Send(context.Background(), "second", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// The durable conversation never carried the volatile state, so nothing in history was
	// rewritten by the change.
	for _, m := range s.Messages() {
		for _, vol := range []string{"feature/one", "feature/two", "pin A", "pin B totally different"} {
			if strings.Contains(m.StringContent, vol) {
				t.Fatalf("volatile state %q leaked into durable history: %+v", vol, m)
			}
		}
	}
	// The change DID land — in the second turn's structured request.
	gotWorktree := be.runtimeAt(1).Worktree
	if gotWorktree == nil || gotWorktree.Current == nil || gotWorktree.Current.Branch != "feature/two" {
		t.Fatalf("turn-2 runtime should reflect the changed worktree; got %+v", gotWorktree)
	}
	mem := be.turnAt(1).Memories
	if mem == nil || !anyContains(mem.Pinned, "pin B totally different") {
		t.Fatalf("turn-2 turn context should reflect the changed pin; got %+v", mem)
	}
}

func TestStableStartupContextRidesEveryRoundWhileMessagesStayVisibleOnly(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
	}}
	contextCalls := 0
	launchable := true
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.PromptContextFunc = func() prompts.MainPromptContext {
		contextCalls++
		tier := domain.TierOperator
		if contextCalls > 1 {
			tier = domain.TierSupervisor
		}
		return prompts.MainPromptContext{
			Tier: tier,
			Project: &prompts.ProjectContext{
				ID: "project-1", Name: "Demo", Path: "/repo",
			},
			AgentRoster: &prompts.AgentRosterContext{Agents: []prompts.AgentContext{
				{ID: "codex", Source: "built-in", Availability: "ready", Launchable: &launchable},
			}},
			ProjectInstructions: "Run the linter.",
		}
	}
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want two rounds, got %d", len(be.requests()))
	}
	for i := 0; i < 2; i++ {
		req := be.requests()[i]
		if req.Startup.Project == nil || req.Startup.Project.Name != "Demo" || req.Startup.AgentRoster == nil || len(req.Startup.AgentRoster.Agents) != 1 || req.Startup.AgentRoster.Agents[0].ID != "codex" {
			t.Fatalf("round %d startup context missing project/agent facts: %+v", i, req.Startup)
		}
		if req.Startup.ProjectInstructions != "Run the linter." {
			t.Fatalf("round %d project instructions = %q", i, req.Startup.ProjectInstructions)
		}
		if i > 0 {
			first, _ := json.Marshal(be.requests()[0].Startup)
			current, _ := json.Marshal(req.Startup)
			if string(first) != string(current) {
				t.Fatalf("round %d changed the stable startup bytes", i)
			}
		}
		runtime := be.runtimeAt(i)
		wantTier := string(domain.TierOperator)
		if i > 0 {
			wantTier = string(domain.TierSupervisor)
		}
		if runtime.PermissionTier != wantTier {
			t.Fatalf("round %d runtime tier = %q", i, runtime.PermissionTier)
		}
	}
	wantRoles := [][]string{{"user"}, {"user", "assistant", "tool"}}
	for round, want := range wantRoles {
		messages := be.requests()[round].Input.Messages
		if len(messages) != len(want) {
			t.Fatalf("round %d message count = %d, want roles %v", round, len(messages), want)
		}
		for i, role := range want {
			if messages[i].Role != role {
				t.Fatalf("round %d message %d role = %q, want %q", round, i, messages[i].Role, role)
			}
		}
	}
}

func TestEnsureStartupContextRunsBeforeFirstBackendSnapshot(t *testing.T) {
	ready := false
	deps, be := recordingDeps(&injectRouter{results: []models.ChatResult{{Content: "done"}}}, &fakeTools{})
	deps.EnsureStartupContext = func(context.Context) { ready = true }
	deps.PromptContextFunc = func() prompts.MainPromptContext {
		if !ready {
			return prompts.MainPromptContext{}
		}
		return prompts.MainPromptContext{Project: &prompts.ProjectContext{Name: "Warmed project"}}
	}
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hello", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := be.requests()
	if len(reqs) != 1 || len(reqs[0].Input.Messages) != 1 {
		t.Fatalf("first request did not wait for startup context: %+v", reqs)
	}
	if reqs[0].Startup.Project == nil || reqs[0].Startup.Project.Name != "Warmed project" {
		t.Fatalf("first startup context = %+v", reqs[0].Startup)
	}
}

func TestStartupContextStaysOutOfMessagesAcrossPersistenceAndResume(t *testing.T) {
	store := &recordingStore{}
	promptContext := prompts.MainPromptContext{Project: &prompts.ProjectContext{ID: "p1", Name: "Persistent Demo"}}
	firstDeps, _ := recordingDeps(&injectRouter{results: []models.ChatResult{{Content: "first answer"}}}, &fakeTools{})
	firstDeps.Store = store
	firstDeps.SessionID = "ses_startup_resume"
	firstDeps.PromptContext = promptContext
	first := NewSession(firstDeps)
	if _, err := first.Send(context.Background(), "first ask", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range store.msgs {
		if strings.Contains(row.Content, "Persistent Demo") {
			t.Fatalf("startup project persisted at seq %d", row.Seq)
		}
	}
	restore, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("persisted visible history did not rehydrate")
	}
	secondDeps, secondBackend := recordingDeps(&injectRouter{results: []models.ChatResult{{Content: "second answer"}}}, &fakeTools{})
	secondDeps.Store = store
	secondDeps.SessionID = "ses_startup_resume"
	secondDeps.PromptContext = promptContext
	secondDeps.RestoredMessages = restore.RestoredMessages
	secondDeps.InitialSeq = restore.InitialSeq
	resumed := NewSession(secondDeps)
	if _, err := resumed.Send(context.Background(), "second ask", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	requests := secondBackend.requests()
	if len(requests) != 1 {
		t.Fatalf("resumed turn requests = %d, want 1", len(requests))
	}
	if requests[0].Startup.Project == nil || requests[0].Startup.Project.Name != "Persistent Demo" {
		t.Fatalf("resumed request startup = %+v", requests[0].Startup)
	}
	wantRoles := []string{"user", "assistant", "user"}
	if len(requests[0].Input.Messages) != len(wantRoles) {
		t.Fatalf("resumed visible message count = %d, want %d", len(requests[0].Input.Messages), len(wantRoles))
	}
	for i, want := range wantRoles {
		if got := requests[0].Input.Messages[i].Role; got != want {
			t.Fatalf("resumed visible message %d role = %q, want %q", i, got, want)
		}
	}
	for _, row := range store.msgs {
		if strings.Contains(row.Content, "Persistent Demo") {
			t.Fatalf("resumed turn persisted startup project at seq %d", row.Seq)
		}
	}
}

// The inverse of the old per-round refresh contract: one detached fetch serves BOTH
// rounds of a multi-round turn (the runtime block carries the same cached snapshot),
// and the fetcher is never re-entered per round. A worktree switch instead reaches the
// model via TTL expiry (see TestWorktree_TTLExpiryKicksDetachedRefresh).
func TestCurrentWorktreeCachedSnapshotServesEveryBackendRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	var calls atomic.Int32
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		n := int(calls.Add(1))
		return &prompts.WorktreeContext{Present: true, Branch: "feature/" + strconv.Itoa(n)}
	}
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "work", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork()
	if got := calls.Load(); got != 1 {
		t.Fatalf("worktree fetch calls = %d, want exactly one for the whole turn", got)
	}
	for i := 0; i < 2; i++ {
		if got := be.runtimeAt(i).Worktree; got == nil || got.Current == nil || got.Current.Branch != "feature/1" {
			t.Fatalf("round %d worktree = %+v, want the cached feature/1 snapshot", i, got)
		}
	}
}

// A FAILED worktree read (fetcher → nil) is cached as "unknown" and injected as nil —
// never papered over with the stale splash/reconnect selection the prompt context
// still carries. (The old companion assertion — that the runtime tail re-snapshots
// MCP state after the read — is gone WITH its cause: the fetch is detached now, so an
// inline read can no longer degrade the shared MCP transport mid-round.)
func TestFailedCurrentWorktreeReadDoesNotMasqueradeCachedValueAsCurrent(t *testing.T) {
	deps, be := recordingDeps(&injectRouter{results: []models.ChatResult{{Content: "done"}}}, &fakeTools{})
	deps.PromptContextFunc = func() prompts.MainPromptContext {
		return prompts.MainPromptContext{
			Worktree: &prompts.WorktreeContext{Present: true, Branch: "stale/from-splash"},
		}
	}
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext { return nil }
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "work", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := be.runtimeAt(0).Worktree; got != nil {
		t.Fatalf("failed live read reused stale worktree %+v", got)
	}
	s.DrainBackgroundWork()
}

// Send-level: an autonomous wake turn (SendOptions.IsWake) ships the IsWake flag plus the
// open workflow runs as structured turn context — the backend substitutes the active
// workflow objective for the verbose wake blob. The CLI's job is to channel the signal,
// not render the anchor; this is the channel-driven path through runTurn.
func TestSend_WakeOptionDrivesObjectiveAnchor(t *testing.T) {
	r := &injectRouter{} // single final round
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{NextActionJson: ptrOf(`{"label":"finish the migration"}`)},
	}}
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), wakePromptPrefix+" a watcher fired", SendOptions{IsWake: true}); err != nil {
		t.Fatal(err)
	}
	turn := be.turnAt(0)
	if !turn.IsWake {
		t.Error("a wake Send must mark the turn context IsWake")
	}
	if !anyContains(turn.WorkflowRuns, "finish the migration") {
		t.Errorf("a wake Send should carry the open workflow runs (with the objective); got %v", turn.WorkflowRuns)
	}
}

// Send-level: a USER who happens to type the wake prefix — WITHOUT SendOptions.IsWake —
// still gets their own goal in the turn context (IsWake stays false), never the workflow
// objective substitution. This is the whole point of channel-based (not content-based)
// wake detection (issue #263 review).
func TestSend_PrefixWithoutWakeOptionKeepsGoalAnchor(t *testing.T) {
	r := &injectRouter{}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{NextActionJson: ptrOf(`{"label":"finish the migration"}`)},
	}}
	s := NewSession(deps)
	input := wakePromptPrefix + " please summarize the logs"
	if _, err := s.Send(context.Background(), input, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	turn := be.turnAt(0)
	if turn.IsWake {
		t.Error("without SendOptions.IsWake the turn must NOT be marked IsWake")
	}
	if turn.Goal != input {
		t.Errorf("a user-typed prefix (no IsWake) should anchor on the user's goal; got %q", turn.Goal)
	}
}
