package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// ptrOf returns a pointer to v — the nullable WorkflowRunRecord fields (issue, branch,
// the JSON blobs) are pointers, so the footer tests need terse literal pointers.
func ptrOf[T any](v T) *T { return &v }

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

// Session-level: the footer is appended as the LAST message of the model request,
// carries the goal, and NEVER leaks into durable history (it must stay ephemeral).
func TestComposeTurnFooter_AppendedToStreamTail(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	before := len(s.Messages())
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(r.seen) == 0 {
		t.Fatal("router observed no rounds")
	}
	round := r.seen[0]
	last := round[len(round)-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "# Current goal") {
		t.Fatalf("last message is not the goal footer: %+v", last)
	}
	if !strings.Contains(last.StringContent, "do the thing") {
		t.Errorf("footer missing the goal text; got %q", last.StringContent)
	}

	// Ephemeral: durable history grows only by user + assistant (+2), and no stored
	// message carries the goal anchor.
	if after := len(s.Messages()); after-before != 2 {
		t.Errorf("history grew by %d, want 2 (footer must not be persisted)", after-before)
	}
	for _, m := range s.Messages() {
		if m.Role == "system" && strings.Contains(m.StringContent, "# Current goal") {
			t.Fatal("goal footer leaked into durable history; it must stay ephemeral")
		}
	}
}

// Session-level: the footer is rebuilt on EVERY round of a multi-round turn, always
// seeded from the same originating ask (not a mid-turn injection).
func TestComposeTurnFooter_RebuiltEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
		{Content: "final"}, // round 1: final answer
	}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "investigate the bug", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	for i, round := range r.seen[:2] {
		last := round[len(round)-1]
		if last.Role != "system" || !strings.Contains(last.StringContent, "investigate the bug") {
			t.Errorf("round %d does not end with the goal footer: %+v", i, last)
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

// Session-level: two sequential turns each anchor their OWN originating ask — the
// footer is never stale from a prior turn.
func TestComposeTurnFooter_DistinctGoalsAcrossSends(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "a"}, {Content: "b"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "goal-one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "goal-two", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	last0 := r.seen[0][len(r.seen[0])-1]
	last1 := r.seen[1][len(r.seen[1])-1]
	if !strings.Contains(last0.StringContent, "goal-one") {
		t.Errorf("send 1 footer should carry goal-one; got %q", last0.StringContent)
	}
	if !strings.Contains(last1.StringContent, "goal-two") || strings.Contains(last1.StringContent, "goal-one") {
		t.Errorf("send 2 footer should carry goal-two only; got %q", last1.StringContent)
	}
}

// Session-level: a mid-turn redirect folds into history as a user message, but the
// footer stays anchored to the ORIGINAL goal (it never chases the injection).
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
	s = NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "original goal", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	round1 := r.seen[1]
	if !userTextSeen(round1, "stop, explain only") {
		t.Error("round 1 should see the folded-in injection in history")
	}
	last := round1[len(round1)-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "original goal") {
		t.Errorf("footer should still carry the original goal; got %+v", last)
	}
	if strings.Contains(last.StringContent, "stop, explain only") {
		t.Error("footer must not adopt the mid-turn injection as the goal")
	}
}

// Session-level: a blank send appends NO tail system message — the request is
// byte-identical to the pre-footer behaviour.
func TestComposeTurnFooter_BlankSendAppendsNoFooter(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) == 0 {
		t.Fatal("router observed no rounds")
	}
	last := r.seen[0][len(r.seen[0])-1]
	if last.Role == "system" && strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("blank goal must not append a goal footer; got %+v", last)
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

// The footer reads the ledger and renders the open runs into the stream tail.
func TestComposeTurnFooter_WorkflowRunsAppearInStreamTail(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{ID: "wfr_live", Status: domain.WorkflowActive, IssueNumber: ptrOf(42)},
	}}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "# Active workflow runs") {
		t.Fatalf("stream tail should carry the workflow section; got %+v", last)
	}
	if !strings.Contains(last.StringContent, "wfr_live") || !strings.Contains(last.StringContent, "#42") {
		t.Errorf("workflow row missing id/issue; got %q", last.StringContent)
	}
}

// A ledger read error is swallowed: the turn completes normally and the footer simply
// omits the workflow section (the goal anchor still renders).
func TestComposeTurnFooter_WorkflowRunsListerError_DoesNotFailSend(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{err: errors.New("db down")}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatalf("a ledger read error must not fail the send: %v", err)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if !strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("goal anchor should still render; got %q", last.StringContent)
	}
	if strings.Contains(last.StringContent, "# Active workflow runs") {
		t.Errorf("a failed read must omit the workflow section; got %q", last.StringContent)
	}
}

// The footer re-reads the ledger every round of a multi-round turn (state changes as
// tools run), and always with the activeWorkflowRunsLimit bound.
func TestComposeTurnFooter_WorkflowRunsReadEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	lister := &fakeWorkflowLister{}
	deps.WorkflowRunLister = lister
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "investigate", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if lister.calls != len(r.seen) {
		t.Errorf("lister should be read once per round: calls=%d rounds=%d", lister.calls, len(r.seen))
	}
	if lister.limit != activeWorkflowRunsLimit {
		t.Errorf("footer read should be bounded by activeWorkflowRunsLimit=%d, got %d", activeWorkflowRunsLimit, lister.limit)
	}
}

// Session-level: a blank send with OPEN runs still appends the workflow section (the
// goal anchor is omitted, but the workflow block depends only on the runs). Closes the
// gap in TestComposeTurnFooter_BlankSendAppendsNoFooter, which has no lister wired.
func TestComposeTurnFooter_BlankSendWithActiveRunsAppendsWorkflowOnly(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowRunLister = &fakeWorkflowLister{runs: []domain.WorkflowRunRecord{
		{ID: "wfr_b", Status: domain.WorkflowActive},
	}}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "# Active workflow runs") {
		t.Fatalf("blank send with open runs should still append the workflow section; got %+v", last)
	}
	if strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("blank goal must not emit the goal anchor; got %q", last.StringContent)
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
	want := "# Pinned and relevant memories\n## Pinned\n- pinned fact"
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
	want := "# Pinned and relevant memories\n## Pinned\n- the pin\n## Relevant (recalled for this turn)\n- the recall"
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
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
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
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	for i, round := range r.seen[:2] {
		last := round[len(round)-1]
		if last.Role != "system" || !strings.Contains(last.StringContent, "# Pinned and relevant memories") {
			t.Errorf("round %d footer missing the recalled-memories block: %+v", i, last)
		}
		if !strings.Contains(last.StringContent, "the deploy key lives in vault") {
			t.Errorf("round %d footer missing the recalled fact", i)
		}
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "# Pinned and relevant memories") {
			t.Fatal("recalled-memories footer leaked into durable history; it must stay ephemeral")
		}
	}
}

// Session-level: a recall error is swallowed — the turn still runs and the footer
// carries the goal anchor but NO `# Pinned and relevant memories` block.
func TestComposeTurnFooter_RecallErrorSwallowed(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	rec := &fakeMemoryRecaller{err: errors.New("fts boom")}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 1 {
		t.Errorf("recaller called %d times, want 1", rec.calls)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if !strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("footer should still carry the goal anchor; got %q", last.StringContent)
	}
	if strings.Contains(last.StringContent, "# Pinned and relevant memories") {
		t.Error("a recall error must omit the memories block")
	}
}

// Session-level: a nil MemoryRecaller (the test default) appends NO memories block —
// the recall path is fully optional.
func TestComposeTurnFooter_NilRecallerOmitsMemories(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if strings.Contains(last.StringContent, "# Pinned and relevant memories") {
		t.Errorf("nil recaller must not append a memories block; got %q", last.StringContent)
	}
}

// Session-level: a blank/whitespace-only send short-circuits recall entirely — the
// recaller is NEVER called (no wasted query) and no memories block appears.
func TestComposeTurnFooter_BlankSendSkipsRecall(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	rec := &fakeMemoryRecaller{rows: []domain.MemoryRecord{{Content: "should not surface"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.MemoryRecaller = rec
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 0 {
		t.Errorf("recaller called %d times on a blank send, want 0", rec.calls)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if strings.Contains(last.StringContent, "# Pinned and relevant memories") {
		t.Errorf("a blank send must not append a memories block; got %q", last.StringContent)
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
	body, ok := sessionNoteSection(footerContext{SessionEndedWatchers: []string{"watch deploy"}})
	if !ok || !strings.HasPrefix(body, "# Session note\n") {
		t.Fatalf("section should render with the header; got (%q, %v)", body, ok)
	}
	if !strings.Contains(body, "1 watcher was stopped") || !strings.Contains(body, `"watch deploy"`) {
		t.Errorf("singular note missing; got %q", body)
	}
}

// Many watchers render a plural note with the title list capped and a "+N more" tail.
func TestSessionNoteSection_PluralAndCap(t *testing.T) {
	var titles []string
	for i := 0; i < sessionEndedWatchersMaxTitles+2; i++ {
		titles = append(titles, "w"+strconv.Itoa(i))
	}
	body, ok := sessionNoteSection(footerContext{SessionEndedWatchers: titles})
	if !ok {
		t.Fatal("section should render")
	}
	if !strings.Contains(body, strconv.Itoa(len(titles))+" watchers were stopped") {
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
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
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
	for i, round := range r.seen[:2] {
		last := round[len(round)-1]
		if !strings.Contains(last.StringContent, "# Pinned and relevant memories") || !strings.Contains(last.StringContent, "## Pinned") {
			t.Errorf("round %d footer missing the pinned block: %q", i, last.StringContent)
		}
		if !strings.Contains(last.StringContent, "always use rtk proxy") {
			t.Errorf("round %d footer missing the pinned fact", i)
		}
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "# Pinned and relevant memories") {
			t.Fatal("pinned footer leaked into durable history; it must stay ephemeral")
		}
	}
}

// Session-level: a pinned-lister error is swallowed — the turn still runs, the goal anchor
// survives, and the pinned subblock is omitted.
func TestComposeTurnFooter_PinnedListerErrorSwallowed(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	pinned := &fakePinnedLister{err: errors.New("db boom")}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.PinnedMemoryLister = pinned
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	last := r.seen[0][len(r.seen[0])-1]
	if !strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("footer should still carry the goal anchor; got %q", last.StringContent)
	}
	if strings.Contains(last.StringContent, "## Pinned") {
		t.Error("a pinned-lister error must omit the pinned subblock")
	}
}

// Session-level: the active-worktree provider is called EVERY round and its label appears
// in the footer's `# Active worktree` section.
func TestComposeTurnFooter_ActiveWorktreeEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
	}}
	calls := 0
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.ActiveWorktreeFunc = func() string { calls++; return "feature/issue-263" }
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Errorf("worktree func called %d times, want once per round (>= 2)", calls)
	}
	for i, round := range r.seen[:2] {
		last := round[len(round)-1]
		if !strings.Contains(last.StringContent, "# Active worktree\nfeature/issue-263") {
			t.Errorf("round %d footer missing the worktree section: %q", i, last.StringContent)
		}
	}
}

// Session-level: the one-time session note surfaces on the FIRST turn only — the provider
// is consulted exactly once and the note is gone from the second turn's footer.
func TestComposeTurnFooter_SessionNoteFirstTurnOnly(t *testing.T) {
	r := &injectRouter{} // empty results → each Send is a single final round
	called := 0
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.SessionEndedWatchers = func() []string { called++; return []string{"watch the deploy"} }
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "first", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "second", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds across two sends, got %d", len(r.seen))
	}
	turn1 := r.seen[0][len(r.seen[0])-1].StringContent
	turn2 := r.seen[1][len(r.seen[1])-1].StringContent
	if !strings.Contains(turn1, "# Session note") || !strings.Contains(turn1, "watch the deploy") {
		t.Errorf("first-turn footer should carry the session note; got %q", turn1)
	}
	if strings.Contains(turn2, "# Session note") {
		t.Errorf("second-turn footer must NOT repeat the session note; got %q", turn2)
	}
	if called != 1 {
		t.Errorf("session-ended provider called %d times, want exactly 1 (first turn only)", called)
	}
}
