package commands

import (
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

// TestFormatRunListCapsAndOneLinesLabel verifies a long multi-line prompt is
// collapsed to a single line and truncated with an ellipsis.
func TestFormatRunListCapsAndOneLinesLabel(t *testing.T) {
	long := "first line\n" + strings.Repeat("x", 300)
	out := FormatRunList([]domain.RunSummaryRecord{{RunID: "run_a", Label: long}})
	if strings.Contains(out, "\n") {
		t.Fatalf("multi-line label must be collapsed to one line: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("over-long label must be truncated with …: %q", out)
	}
	if len([]rune(out)) > 200 { // run-id + ts + count + ~140 label
		t.Fatalf("label not capped: %d runes", len([]rune(out)))
	}
}

// TestFormatRunTimelineAgentSaid verifies that an allowlisted tool's structured
// result surfaces an indented "agent said:" block from ResultJson (terminal.read →
// content), while a non-allowlisted tool stays summary-only.
func TestFormatRunTimelineAgentSaid(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "run_a", Seq: 0, Type: "tool:result", Payload: strPtr(
			`{"id":"c1","name":"terminal.read","ok":true,"summary":"read terminal t1","auditId":"aud_1"}`)},
		{RunID: "run_a", Seq: 1, Type: "tool:result", Payload: strPtr(
			`{"id":"c2","name":"fs.read","ok":true,"summary":"read a.go","auditId":"aud_2"}`)},
	}
	audit := []domain.AuditRecord{
		{ID: "aud_1", ToolName: "terminal.read", Outcome: "ok", DurationMs: 5,
			ResultJson: strPtr(`{"terminalId":"t1","content":"build failed: missing import","lineCount":1}`)},
		{ID: "aud_2", ToolName: "fs.read", Outcome: "ok", DurationMs: 2,
			ResultJson: strPtr(`{"content":"package main"}`)},
	}
	out := FormatRunTimeline(events, audit)
	if !strings.Contains(out, "agent said: build failed: missing import") {
		t.Fatalf("terminal.read reply not surfaced: %q", out)
	}
	// fs.read is off the allowlist — its content must NOT appear as agent said.
	if strings.Contains(out, "agent said: package main") {
		t.Fatalf("non-allowlisted tool leaked agent said: %q", out)
	}
}

// TestFormatRunTimelineAgentSaidDaintreeCall verifies daintree.call surfaces its
// "text" field, and that an empty reply renders nothing.
func TestFormatRunTimelineAgentSaidDaintreeCall(t *testing.T) {
	events := []domain.RunEventRecord{
		{RunID: "r", Seq: 0, Type: "tool:result", Payload: strPtr(
			`{"id":"c1","name":"daintree.call","ok":true,"summary":"called","auditId":"a1"}`)},
		{RunID: "r", Seq: 1, Type: "tool:result", Payload: strPtr(
			`{"id":"c2","name":"daintree.call","ok":true,"summary":"called","auditId":"a2"}`)},
	}
	audit := []domain.AuditRecord{
		{ID: "a1", ToolName: "daintree.call", Outcome: "ok", ResultJson: strPtr(`{"text":"worktree ready"}`)},
		{ID: "a2", ToolName: "daintree.call", Outcome: "ok", ResultJson: strPtr(`{"text":""}`)},
	}
	out := FormatRunTimeline(events, audit)
	if !strings.Contains(out, "agent said: worktree ready") {
		t.Fatalf("daintree.call reply not surfaced: %q", out)
	}
	if strings.Count(out, "agent said:") != 1 {
		t.Fatalf("empty reply must render nothing: %q", out)
	}
}

// TestAgentReplyCapsLongReply verifies the 600-rune cap with an ellipsis.
func TestAgentReplyCapsLongReply(t *testing.T) {
	long := strings.Repeat("y", 800)
	rj := `{"content":"` + long + `"}`
	got := agentReply("terminal.read", &rj)
	if r := []rune(got); len(r) != 600 || !strings.HasSuffix(got, "…") {
		t.Fatalf("reply not capped at 600 with …: %d runes", len([]rune(got)))
	}
}
