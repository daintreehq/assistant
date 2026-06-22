package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// --- helpers (port of tests/wake.test.ts factories) ---

func makeWakeEvent(over func(*domain.QueueEvent)) domain.QueueEvent {
	e := domain.QueueEvent{
		ID:        "evt-1",
		Source:    domain.SourceTerminalWatcher,
		Severity:  domain.SeverityAttention,
		Title:     "supervised waiting: Terminal waiting for input",
		Summary:   "agent paused for input",
		CreatedAt: 1000,
		Count:     1,
	}
	if over != nil {
		over(&e)
	}
	return e
}

func termWakeEvent(terminalID string, over func(*domain.QueueEvent)) domain.QueueEvent {
	return makeWakeEvent(func(e *domain.QueueEvent) {
		e.Target = &domain.EventTarget{TerminalID: terminalID}
		if over != nil {
			over(e)
		}
	})
}

func setOf(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// --- IsActionableWake ---

func TestIsActionableWake(t *testing.T) {
	tests := []struct {
		name string
		ev   domain.QueueEvent
		want bool
	}{
		{"terminal_watcher with terminalId", termWakeEvent("t1", nil), true},
		{"missing target", makeWakeEvent(func(e *domain.QueueEvent) { e.Target = nil }), false},
		{"empty terminalId", makeWakeEvent(func(e *domain.QueueEvent) { e.Target = &domain.EventTarget{TerminalID: ""} }), false},
		{"non-watcher source user", termWakeEvent("t1", func(e *domain.QueueEvent) { e.Source = domain.SourceUser }), false},
		{"non-watcher source system", termWakeEvent("t1", func(e *domain.QueueEvent) { e.Source = domain.SourceSystem }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsActionableWake(tc.ev); got != tc.want {
				t.Fatalf("IsActionableWake = %v want %v", got, tc.want)
			}
		})
	}
}

// --- BuildWakePrompt ---

func TestBuildWakePromptFirstTimeRequestsFullSummary(t *testing.T) {
	// No opts: a first-time terminal earns the positive "give a concise update"
	// guidance and no follow-up ack marker.
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatalf("missing full-summary guidance:\n%s", prompt)
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("unexpected ack marker on first-time terminal:\n%s", prompt)
	}
}

func TestBuildWakePromptEmptySummarizedSameAsNoOpts(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, setOf())
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatalf("missing full-summary guidance with empty set:\n%s", prompt)
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("unexpected ack marker with empty set:\n%s", prompt)
	}
}

func TestBuildWakePromptDowngradesFollowUp(t *testing.T) {
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" })},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "already reported") {
		t.Fatal("expected ack marker for already-summarized terminal")
	}
	if !strings.Contains(prompt, "do NOT call terminal.read/terminal.summarize/terminal.extract again") {
		t.Fatal("missing do-NOT-call directive")
	}
	if !strings.Contains(prompt, "[terminal t1]") {
		t.Fatal("per-event line must name the terminal")
	}
}

func TestBuildWakePromptAllFollowUpsSwapGuidance(t *testing.T) {
	// Every event is a follow-up: the positive "summarize and report" header must be
	// absent and acknowledge-only guidance present.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", nil), termWakeEvent("t1", nil)},
		setOf("t1"),
	)
	if strings.Contains(prompt, "give the user a concise update") {
		t.Fatal("all-follow-up batch must not include the positive update guidance")
	}
	if !strings.Contains(prompt, "Acknowledge each in one short line") {
		t.Fatal("all-follow-up batch must include acknowledge-only guidance")
	}
}

func TestBuildWakePromptFirstTimeEventLineFreeOfAckMarker(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatal("missing full-summary guidance")
	}
	var eventLine string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "[terminal t1]") {
			eventLine = l
		}
	}
	if eventLine == "" {
		t.Fatal("expected a per-event line naming terminal t1")
	}
	if strings.Contains(eventLine, "already reported") {
		t.Fatalf("first-time per-event line must not carry the ack marker: %q", eventLine)
	}
}

func TestBuildWakePromptIssue39Lifecycle(t *testing.T) {
	// A terminal summarized in one burst is a follow-up in the next — the caller
	// threads its summarizedTerminals set across bursts.
	summarized := setOf()
	first := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised waiting: Terminal waiting" })},
		summarized,
	)
	if !strings.Contains(first, "give the user a concise update") || strings.Contains(first, "already reported") {
		t.Fatal("first burst should be a full summary")
	}
	summarized["t1"] = struct{}{}
	second := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" })},
		summarized,
	)
	if !strings.Contains(second, "already reported") {
		t.Fatal("second burst should be a follow-up ack")
	}
	if !strings.Contains(second, "do NOT call terminal.read/terminal.summarize/terminal.extract again") {
		t.Fatal("second burst missing do-NOT-call directive")
	}
}

func TestBuildWakePromptPerTerminalGranularity(t *testing.T) {
	// t1 is a follow-up, t2 is brand new and still earns a full summary.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", nil), termWakeEvent("t2", nil)},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "already reported") {
		t.Fatal("t1 should be a follow-up")
	}
	var t2Line string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.Contains(l, "[terminal t2]") {
			t2Line = l
		}
	}
	if t2Line == "" {
		t.Fatal("expected a per-event line for t2")
	}
	if strings.Contains(t2Line, "already reported") {
		t.Fatalf("brand-new t2 line must not carry ack marker: %q", t2Line)
	}
}

func TestBuildWakePromptFirstOccurrenceOnlyWhenSameTerminalTwice(t *testing.T) {
	// Same terminal appears twice in one batch: only the SECOND per-event line is
	// downgraded to an ack.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{
			termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised waiting: Terminal waiting" }),
			termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" }),
		},
		setOf(),
	)
	var followUps []string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "already reported") {
			followUps = append(followUps, l)
		}
	}
	if len(followUps) != 1 {
		t.Fatalf("expected exactly one downgraded per-event line, got %d:\n%s", len(followUps), prompt)
	}
	if !strings.Contains(followUps[0], "Terminal exited") {
		t.Fatalf("the downgraded line should be the second (exited) event: %q", followUps[0])
	}
}

func TestBuildWakePromptEventWithoutTerminalIDNeutral(t *testing.T) {
	// An event with no terminalId renders neutrally and never crashes; with no new
	// terminal-scoped summary needed, no ack marker leaks in.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{makeWakeEvent(func(e *domain.QueueEvent) { e.Target = nil })},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "New events:") {
		t.Fatal("missing the 'New events:' section")
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("terminal-less event must not carry an ack marker:\n%s", prompt)
	}
}

// --- IsWakeFailureReply ---

func TestIsWakeFailureReplyRecognizesSentinels(t *testing.T) {
	failures := []string{
		"Model unavailable: 503",
		"Model error: boom",
		"Tool projection failed: dup name",
		"Reached the tool-iteration limit without a final answer.",
		"Turn cancelled",
		"Stopped: called watcher.terminal.create 3 times this turn with identical arguments, each failing the same way (INVALID_ARGS: ...).",
	}
	for _, reply := range failures {
		if !IsWakeFailureReply(reply) {
			t.Fatalf("expected wake-failure sentinel for %q", reply)
		}
	}
}

func TestIsWakeFailureReplyRealReplyIsSuccess(t *testing.T) {
	if IsWakeFailureReply("Terminal t1 finished cleanly; tests passed.") {
		t.Fatal("a real reply must not be a wake failure")
	}
	if IsWakeFailureReply("") {
		t.Fatal("an empty reply must not be a wake failure")
	}
}
