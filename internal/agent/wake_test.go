package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// --- helpers ---

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

func TestBuildWakePromptSurfacesInboxIDForResolve(t *testing.T) {
	// The reactor needs the inbox id to resolve THIS exact item on a wake
	// turn — every per-event line must carry "(inbox <id>)".
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.ID = "evt-77" })},
		nil,
	)
	if !strings.Contains(prompt, "(inbox evt-77)") {
		t.Fatalf("event line must surface the inbox id for queue.resolve:\n%s", prompt)
	}
}

func TestBuildWakePromptInstructsInboxHygiene(t *testing.T) {
	// A finished watch, once reported, should be resolved (not cancelled — its
	// watcher already stopped itself). The hygiene guidance is present on BOTH the
	// summarize branch and the acknowledge-only branch.
	full := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	ackOnly := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, setOf("t1"))
	for name, prompt := range map[string]string{"summarize": full, "ack-only": ackOnly} {
		if !strings.Contains(prompt, "queue.resolve") {
			t.Fatalf("%s branch missing queue.resolve hygiene guidance:\n%s", name, prompt)
		}
		if !strings.Contains(prompt, "nothing left to cancel") {
			t.Fatalf("%s branch missing the already-stopped/no-cancel nuance:\n%s", name, prompt)
		}
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
		"Model rate-limited: provider quota/throughput exceeded",
		"Model error: boom",
		"Tool projection failed: dup name",
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

// --- async wake events ---

func asyncWakeEvent(over func(*domain.QueueEvent)) domain.QueueEvent {
	e := domain.QueueEvent{
		ID:        "evt-async-1",
		Source:    domain.SourceAsyncTool,
		Severity:  domain.SeverityAttention,
		Title:     "Async finished: npm test",
		Summary:   `asy_1a2b "npm test": term-1: finished`,
		Target:    &domain.EventTarget{TerminalID: "term-1", AsyncInvocationID: "asy_1a2b"},
		CreatedAt: 1000,
		Count:     1,
	}
	if over != nil {
		over(&e)
	}
	return e
}

func TestIsActionableWakeAsyncSource(t *testing.T) {
	if !IsActionableWake(asyncWakeEvent(nil)) {
		t.Fatal("an async_tool completion must be actionable")
	}
	// Even without a terminal target (a grouped completion), async completions wake.
	if !IsActionableWake(asyncWakeEvent(func(e *domain.QueueEvent) { e.Target = &domain.EventTarget{AsyncInvocationID: "asy_x"} })) {
		t.Fatal("a grouped async completion (no terminal target) must still be actionable")
	}
}

func TestBuildWakePromptAsyncOnlyBurst(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{asyncWakeEvent(nil)}, nil)
	if !strings.HasPrefix(prompt, "[automatic wake-up]") {
		t.Fatalf("missing wake prefix:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Asynchronous operation(s) you started") {
		t.Fatalf("missing async framing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- Async finished: npm test: asy_1a2b \"npm test\": term-1: finished [terminal term-1] (inbox evt-async-1)") {
		t.Fatalf("missing self-contained completion line:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Do NOT re-run the async operation") {
		t.Fatalf("missing no-rerun guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "queue.resolve") {
		t.Fatalf("missing inbox hygiene guidance:\n%s", prompt)
	}
	// The watcher framing must NOT leak into an async-only burst.
	if strings.Contains(prompt, "background watcher") {
		t.Fatalf("watcher framing leaked into async-only burst:\n%s", prompt)
	}
}

func TestBuildWakePromptMixedBurstKeepsWatcherBodyAndAppendsAsync(t *testing.T) {
	events := []domain.QueueEvent{termWakeEvent("t1", nil), asyncWakeEvent(nil)}
	prompt := BuildWakePrompt(events, nil)
	if !strings.Contains(prompt, "A background watcher surfaced new activity") {
		t.Fatalf("mixed burst lost the watcher framing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "asynchronous operation(s) you started earlier have finished") {
		t.Fatalf("mixed burst lost the async section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "(inbox evt-async-1)") {
		t.Fatalf("async completion line missing from mixed burst:\n%s", prompt)
	}
}

func TestBuildWakePromptWatcherOnlyUnchangedByAsyncBranch(t *testing.T) {
	// The watcher-only output is model-facing contract text: the async partition
	// must be a pure pass-through when no async events are present.
	events := []domain.QueueEvent{termWakeEvent("t1", nil)}
	if got, want := BuildWakePrompt(events, nil), buildWatcherWakePrompt(events, nil); got != want {
		t.Fatalf("watcher-only burst diverged from the watcher prompt:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
