package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
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
