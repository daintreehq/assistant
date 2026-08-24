package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// TestFormatRunListShowsPromptLabel verifies the run list appends the originating
// prompt (one-lined + capped) after the event count, and omits it when blank.
func TestFormatRunListShowsPromptLabel(t *testing.T) {
	runs := []domain.RunSummaryRecord{
		{RunID: "run_a", FirstTs: 1000, EventCount: 3, Label: "which worktrees are ready?"},
		{RunID: "run_b", FirstTs: 2000, EventCount: 1, Label: ""},
	}
	out := FormatRunList(runs)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "which worktrees are ready?") {
		t.Fatalf("labeled run missing prompt: %q", lines[0])
	}
	// Unlabeled run ends at the noun with no trailing label noise.
	if !strings.HasSuffix(strings.TrimRight(lines[1], " "), "1 event") {
		t.Fatalf("unlabeled run should end at the noun: %q", lines[1])
	}
}

// TestFormatRunListCapsAndOneLinesLabel verifies a long multi-line prompt (CRLF
// included) is collapsed to a single line with no stray carriage returns and
// truncated with an ellipsis.
func TestFormatRunListCapsAndOneLinesLabel(t *testing.T) {
	long := "first line\r\nsecond\r" + strings.Repeat("x", 300)
	out := FormatRunList([]domain.RunSummaryRecord{{RunID: "run_a", Label: long}})
	if strings.ContainsAny(out, "\n\r") {
		t.Fatalf("multi-line/CRLF label must collapse to one line: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("over-long label must be truncated with …: %q", out)
	}
	if len([]rune(out)) > 200 { // run-id + ts + count + ~140 label
		t.Fatalf("label not capped: %d runes", len([]rune(out)))
	}
}

// TestFormatRunTimelineSurfacesHiddenReply verifies that a tool whose summary is
// terse (daintree.call → "Called X.") surfaces its reply text from ResultJson as
// an indented "agent said:" block.
func TestFormatRunTimelineSurfacesHiddenReply(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "tool:result", Payload: strPtr(
			`{"id":"c1","name":"daintree.call","ok":true,"summary":"Called worktree.list.","auditId":"a1"}`)},
	}
	audit := []domain.AuditRecord{
		{ID: "a1", ToolName: "daintree.call", Outcome: "ok", DurationMs: 7,
			ResultJson: strPtr(`{"text":"worktree feature-x is clean and ready","structuredContent":null}`)},
	}
	out := FormatRunTimeline(events, audit)
	if !strings.Contains(out, "agent said: worktree feature-x is clean and ready") {
		t.Fatalf("daintree.call reply not surfaced: %q", out)
	}
}

// TestFormatRunTimelineDedupsVerbatimReply verifies that terminal.read — whose
// summary IS its verbatim scrollback — does NOT also emit an "agent said:" block
// (the content would appear twice).
func TestFormatRunTimelineDedupsVerbatimReply(t *testing.T) {
	content := "build failed: missing import \"fmt\""
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "tool:result", Payload: strPtr(
			`{"id":"c1","name":"terminal.read","ok":true,"summary":` + jsonStr(content) + `,"auditId":"a1"}`)},
	}
	audit := []domain.AuditRecord{
		{ID: "a1", ToolName: "terminal.read", Outcome: "ok", DurationMs: 5,
			ResultJson: strPtr(`{"terminalId":"t1","content":` + jsonStr(content) + `,"lineCount":1}`)},
	}
	out := FormatRunTimeline(events, audit)
	if strings.Contains(out, "agent said:") {
		t.Fatalf("verbatim terminal.read must not duplicate as agent said: %q", out)
	}
	if strings.Count(out, content) != 1 {
		t.Fatalf("scrollback should appear exactly once (as the summary): %q", out)
	}
}

// TestFormatRunTimelineNonAllowlistedToolNoReply verifies a tool off the allowlist
// (fs.read) never emits an agent-said block even with a content field.
func TestFormatRunTimelineNonAllowlistedToolNoReply(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "tool:result", Payload: strPtr(
			`{"id":"c1","name":"fs.read","ok":true,"summary":"read a.go","auditId":"a1"}`)},
	}
	audit := []domain.AuditRecord{
		{ID: "a1", ToolName: "fs.read", Outcome: "ok", ResultJson: strPtr(`{"content":"package main"}`)},
	}
	out := FormatRunTimeline(events, audit)
	if strings.Contains(out, "agent said:") {
		t.Fatalf("non-allowlisted tool leaked agent said: %q", out)
	}
}

// TestFormatRunTimelineSkipsTurnPromptLine verifies the turn:prompt event (shown as
// the run label in FormatRunList) is not rendered as a timeline line.
func TestFormatRunTimelineSkipsTurnPromptLine(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "turn:prompt", Payload: strPtr(`{"prompt":"spawn the fixer"}`)},
		{RunID: "r", Seq: 1, Type: "assistant:start"},
	}
	out := FormatRunTimeline(events, nil)
	if strings.Contains(out, "turn:prompt") || strings.Contains(out, "spawn the fixer") {
		t.Fatalf("turn:prompt must not appear in the timeline: %q", out)
	}
	if !strings.Contains(out, "▸ assistant") {
		t.Fatalf("assistant:start should still render: %q", out)
	}
}

// TestAgentReplyAllowlistAndCap verifies allowlist gating (raw, uncapped) and the
// 600-rune capReply boundary.
func TestAgentReplyAllowlistAndCap(t *testing.T) {
	// off the allowlist → empty.
	if got := agentReply("fs.read", strPtr(`{"content":"x"}`)); got != "" {
		t.Fatalf("non-allowlisted should be empty, got %q", got)
	}
	// nil/blank result → empty.
	if got := agentReply("terminal.read", nil); got != "" {
		t.Fatalf("nil result should be empty, got %q", got)
	}
	// agentReply returns the RAW (uncapped) reply; capReply bounds it.
	long := strings.Repeat("y", 800)
	raw := agentReply("terminal.read", strPtr(`{"content":`+jsonStr(long)+`}`))
	if raw != long {
		t.Fatalf("agentReply should return the raw reply uncapped")
	}
	if capped := capReply(raw); len([]rune(capped)) != 600 || !strings.HasSuffix(capped, "…") {
		t.Fatalf("capReply must bound to 600 runes + …: %d runes", len([]rune(capped)))
	}
}

// jsonStr returns s as a JSON string literal (quoted, escaped) for embedding in a
// raw payload fixture.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// decisionRow builds a runbook:decision run-event row.
func decisionRow(seq int, payload string) domain.RunEventRecord {
	return domain.RunEventRecord{RunID: "r", Seq: seq, Type: "runbook:decision", Payload: strPtr(payload)}
}

const activeMulti = `{"active":[{"id":"multi_agent","title":"Multi-agent orchestration"},` +
	`{"id":"foundation","title":"Daintree orchestration foundation"}],"newlyLoaded":[],` +
	`"selector":{"ran":true,"degraded":false,"taskType":"orchestration","confidence":0.9,"reason":""}}`

// The committed decision is the run's runbook story, and it names the WHOLE active set —
// including the foundation runbook that was retained rather than newly loaded, which the
// delta-only runbook:loaded cue can never mention.
func TestFormatRunTimelineShowsActiveSetFromDecision(t *testing.T) {
	out := FormatRunTimeline([]domain.RunEventRecord{decisionRow(0, activeMulti)}, nil)
	if !strings.Contains(out, "Multi-agent orchestration") ||
		!strings.Contains(out, "Daintree orchestration foundation") {
		t.Fatalf("the active set is not named: %q", out)
	}
}

// Once a run records decisions, the eager runbook:loaded row is SUPERSEDED. It is a
// per-attempt delta: on a retried round it can name a runbook the committed round never
// kept, so showing it above the authoritative line would put the wrong answer first.
func TestFormatRunTimelineDecisionSupersedesEagerRunbookLoaded(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "runbook:loaded", Payload: strPtr(`{"titles":["Attempt-one runbook"]}`)},
		decisionRow(1, activeMulti),
	}
	out := FormatRunTimeline(events, nil)
	if strings.Contains(out, "Attempt-one runbook") {
		t.Fatalf("the non-authoritative delta cue survived alongside the decision: %q", out)
	}
	if !strings.Contains(out, "Multi-agent orchestration") {
		t.Fatalf("the committed active set is missing: %q", out)
	}
}

// A run from before this event existed has ONLY the eager rows, so they must still
// render — dropping them unconditionally would blank the runbook story of every past run.
func TestFormatRunTimelineKeepsRunbookLoadedWhenNoDecisionRecorded(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "runbook:loaded", Payload: strPtr(`{"titles":["Legacy runbook"]}`)},
	}
	out := FormatRunTimeline(events, nil)
	if !strings.Contains(out, "Legacy runbook") {
		t.Fatalf("a pre-decision run lost its runbook row: %q", out)
	}
}

// A decision is persisted every committed round, but an UNCHANGED set adds nothing. A
// turn is many rounds and the set usually holds across all of them, so repeating it
// would bury the tool calls the replay exists to show.
func TestFormatRunTimelineCollapsesUnchangedActiveSet(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, activeMulti),
		decisionRow(1, activeMulti),
		decisionRow(2, activeMulti),
	}
	out := FormatRunTimeline(events, nil)
	if got := strings.Count(out, "runbooks active"); got != 1 {
		t.Fatalf("active line rendered %d times across 3 identical rounds, want 1: %q", got, out)
	}
}

// …but a CHANGE is the whole point: mid-turn the selector swapped a runbook in.
func TestFormatRunTimelineShowsChangedActiveSet(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
		decisionRow(1, `{"active":[{"id":"b","title":"Beta"}],"selector":{"ran":true}}`),
	}
	out := FormatRunTimeline(events, nil)
	if !strings.Contains(out, "Alpha") || !strings.Contains(out, "Beta") {
		t.Fatalf("a mid-turn runbook change was collapsed away: %q", out)
	}
}

// A set that CLEARS must be said out loud. Silence here would let the replay imply the
// last-named set stayed active for the rest of the run.
func TestFormatRunTimelineReportsActiveSetClearing(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
		decisionRow(1, `{"active":[],"selector":{"ran":true}}`),
	}
	out := FormatRunTimeline(events, nil)
	if !strings.Contains(out, "Alpha") {
		t.Fatalf("the initial set is missing: %q", out)
	}
	if !strings.Contains(out, "none") {
		t.Fatalf("a cleared active set rendered nothing, so the replay still implies "+
			"Alpha is active: %q", out)
	}
}

// …and a REACTIVATION after clearing must show too — the bug this guards against is
// state that never recorded the clear, making A → none → A collapse to a single A.
func TestFormatRunTimelineReportsReactivationAfterClearing(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
		decisionRow(1, `{"active":[],"selector":{"ran":true}}`),
		decisionRow(2, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
	}
	out := FormatRunTimeline(events, nil)
	if got := strings.Count(out, "Alpha"); got != 2 {
		t.Fatalf("Alpha rendered %d time(s), want 2 (before and after the clear): %q", got, out)
	}
}

// A run that never has any active runbooks stays silent. "none" is for a set that CLEARED,
// not for the ordinary no-runbooks run, which would otherwise gain a line per turn.
func TestFormatRunTimelineSilentWhenNothingWasEverActive(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[],"newlyLoaded":[],"selector":{"ran":false,"degraded":false}}`),
		decisionRow(1, `{"active":[],"newlyLoaded":[],"selector":{"ran":false,"degraded":false}}`),
	}
	if out := FormatRunTimeline(events, nil); strings.TrimSpace(out) != "" {
		t.Fatalf("a run with no runbooks must render nothing, got %q", out)
	}
}

// A row whose `active` is missing or malformed says "this row cannot tell us" — it must
// NOT be read as "the set cleared", which would blank a perfectly good tracked set.
func TestFormatRunTimelineMalformedActiveDoesNotClearTheTrackedSet(t *testing.T) {
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
		decisionRow(1, `{"selector":{"ran":true}}`),                         // absent
		decisionRow(2, `{"active":"not-an-array","selector":{"ran":true}}`), // malformed
		decisionRow(3, `{"active":[{"id":"a","title":"Alpha"}],"selector":{"ran":true}}`),
	}
	out := FormatRunTimeline(events, nil)
	if strings.Contains(out, "none") {
		t.Fatalf("an unusable active field was misread as a cleared set: %q", out)
	}
	// Alpha is still the tracked set across rows 1-2, so row 3 is not a change.
	if got := strings.Count(out, "Alpha"); got != 1 {
		t.Fatalf("Alpha rendered %d time(s), want 1 — the tracked set was disturbed by an "+
			"unusable row: %q", got, out)
	}
}

// The degraded case is ALWAYS shown, even on a round whose active set is unchanged —
// that is exactly the fail-open shape: the set held because deciding failed, not because
// the selector chose it.
func TestFormatRunTimelineSurfacesDegradedEvenWhenSetUnchanged(t *testing.T) {
	degraded := `{"active":[{"id":"multi_agent","title":"Multi-agent orchestration"},{"id":"bare_id"}],` +
		`"newlyLoaded":[],"selector":{"ran":true,"degraded":true,"taskType":"","confidence":null,` +
		`"reason":"selector timed out"}}`
	events := []domain.RunEventRecord{
		decisionRow(0, `{"active":[{"id":"multi_agent","title":"Multi-agent orchestration"},`+
			`{"id":"bare_id"}],"selector":{"ran":true,"degraded":false}}`),
		decisionRow(1, degraded),
	}
	out := FormatRunTimeline(events, nil)
	if !strings.Contains(out, "degraded") {
		t.Fatalf("degraded selector not surfaced on an unchanged round: %q", out)
	}
	if !strings.Contains(out, "Multi-agent orchestration") {
		t.Fatalf("the reused active set is not named: %q", out)
	}
	// A ref the backend sent without a title still shows as something addressable.
	if !strings.Contains(out, "bare_id") {
		t.Fatalf("a title-less ref must fall back to its id: %q", out)
	}
	if !strings.Contains(out, "selector timed out") {
		t.Fatalf("the selector's reason is the diagnostic payload: %q", out)
	}
}

// These rows are replayed from SQLite, so the payload is arbitrary stored JSON rather
// than something built in-process. Each shape gets its own case with an exact expected
// output, so a malformed row cannot start leaking text while a combined assertion still
// passes on one good line elsewhere.
func TestFormatRunTimelineToleratesMalformedRunbookDecision(t *testing.T) {
	cases := []struct {
		name    string
		payload *string
		want    string // exact rendered output
	}{
		{"nil payload", nil, ""},
		{"json null", strPtr(`null`), ""},
		{"invalid json", strPtr(`{not json`), ""},
		{"empty object", strPtr(`{}`), ""},
		{"selector is a scalar", strPtr(`{"selector":"not-an-object"}`), ""},
		{"active is a scalar", strPtr(`{"active":"not-an-array","selector":{"degraded":false}}`), ""},
		{"degraded is a string", strPtr(`{"selector":{"degraded":"true"}}`), ""},
		{"active entries are not objects", strPtr(
			`{"active":[1,"two",null],"selector":{"degraded":false}}`), ""},
		{"non-string id and title", strPtr(
			`{"active":[{"id":7,"title":false}],"selector":{"degraded":false}}`), ""},
		{"serialization stub", strPtr(`{"error":"unserializable"}`), ""},
		// Degraded still warns even when the active set is unusable — the flag is the
		// diagnostic, and a missing set must not suppress it.
		{"degraded with unusable active", strPtr(
			`{"active":"not-an-array","selector":{"degraded":true}}`),
			"⚠ runbook selector degraded (reused the prior set)"},
		{"degraded with non-string reason", strPtr(
			`{"active":[],"selector":{"degraded":true,"reason":42}}`),
			"⚠ runbook selector degraded (reused the prior set)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := FormatRunTimeline(
				[]domain.RunEventRecord{{RunID: "r", Seq: 0, Type: "runbook:decision", Payload: tc.payload}}, nil)
			if strings.TrimSpace(out) != tc.want {
				t.Fatalf("output = %q, want %q", out, tc.want)
			}
			// Never the bare "· runbook:decision" default, which is noise with none of
			// the information.
			if strings.Contains(out, "· runbook:decision") {
				t.Fatalf("bare event type leaked into the replay: %q", out)
			}
		})
	}
}
