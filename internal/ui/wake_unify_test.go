package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// wake_unify_test.go locks the cockpit↔host wake convergence: the cockpit now uses
// the shared agent.BuildWakePrompt (cross-burst dedup + the "NOT typed by the user"
// framing) and records summarized terminals on a REAL reply only, mirroring the host
// so the same terminal isn't re-summarized across lifecycle bursts.

func wakeEvent(terminalID, title string) domain.QueueEvent {
	// Source matters since the async split: only WATCHER events feed the
	// summarized-terminals memory (an async completion must not poison it).
	return domain.QueueEvent{
		Source: domain.SourceTerminalWatcher,
		Title:  title,
		Target: &domain.EventTarget{TerminalID: terminalID},
	}
}

// A successful wake records its terminals as summarized; the NEXT burst's prompt
// then downgrades that terminal to a one-line ack — exactly the cross-burst memory
// the host has and the old local wakePrompt lacked.
func TestWake_SummarizedTerminalsRecordedOnSuccess(t *testing.T) {
	m := liveModel(80)
	m.activeWake = []domain.QueueEvent{wakeEvent("term_1", "agent waiting")}

	next, _ := m.onWakeComplete(WakeCompleteMsg{Reply: "I looked; the agent is waiting for input.", Failed: false})
	nm := next.(Model)

	if _, ok := nm.summarizedTerminals["term_1"]; !ok {
		t.Fatal("a successful wake must record its terminal as summarized")
	}
	// The next burst over the SAME terminal is downgraded to an ack (already reported).
	prompt := agent.BuildWakePrompt([]domain.QueueEvent{wakeEvent("term_1", "agent exited")}, nm.summarizedTerminals)
	if !strings.Contains(prompt, "already reported") {
		t.Fatalf("a follow-up burst on an already-summarized terminal must be downgraded: %q", prompt)
	}
}

// A FAILED wake must NOT record terminals — else a transient model outage would
// permanently downgrade the terminal's later lifecycle events to one-line acks and
// swallow the real summary.
func TestWake_FailedWakeDoesNotRecordSummarized(t *testing.T) {
	m := liveModel(80)
	m.activeWake = []domain.QueueEvent{wakeEvent("term_2", "agent waiting")}

	next, _ := m.onWakeComplete(WakeCompleteMsg{Reply: "Model unavailable: offline", Failed: true})
	nm := next.(Model)

	if _, ok := nm.summarizedTerminals["term_2"]; ok {
		t.Fatal("a failed wake must NOT record the terminal as summarized")
	}
	// A fresh prompt over that terminal is still a full summary (not an ack).
	prompt := agent.BuildWakePrompt([]domain.QueueEvent{wakeEvent("term_2", "agent waiting")}, nm.summarizedTerminals)
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("a terminal from a FAILED wake must not be pre-marked summarized: %q", prompt)
	}
}

// The cockpit's isFailureReply does NOT match the wake-failure sentinels
// ("Model unavailable:", etc.), so Send returning one yields Failed=false. The wake
// must STILL treat it as a failure (via agent.IsWakeFailureReply) and NOT record the
// terminal — else a transient model outage permanently downgrades later events.
func TestWake_ModelFailureSentinelNotRecorded(t *testing.T) {
	for _, sentinel := range []string{
		"Model unavailable: offline",
		"Model error: 500",
	} {
		m := liveModel(80)
		m.activeTurn = "wake_x"
		cell := &TurnCell{ID: "wake_x", State: TurnActive}
		m.transcript = []TranscriptCell{{Turn: cell}}
		m.activeWake = []domain.QueueEvent{wakeEvent("term_3", "agent waiting")}

		// Failed:false mimics the controller (isFailureReply misses the wake sentinel).
		next, _ := m.onWakeComplete(WakeCompleteMsg{RunID: "wake_x", Reply: sentinel, Failed: false})
		nm := next.(Model)

		if _, ok := nm.summarizedTerminals["term_3"]; ok {
			t.Fatalf("a model-failure sentinel (%q) must NOT record the terminal as summarized", sentinel)
		}
		// The failure path consumed the one-shot retry budget (drainPending immediately
		// re-drains the requeued burst into a fresh wake, so wakeRetried is the stable
		// signal that the burst was treated as a failure rather than a clean result).
		if !nm.wakeRetried {
			t.Fatalf("a model-failure wake (%q) should requeue the burst once (wakeRetried)", sentinel)
		}
		if cell.State != TurnFailed {
			t.Fatalf("a model-failure wake (%q) should seal the turn as failed, got %v", sentinel, cell.State)
		}
	}
}

// An ASYNC completion wake must NOT mark its terminal as summarized: that set
// means "got a full watcher summary", and poisoning it would downgrade a later
// genuine watcher event (an agent waiting on a question) to a one-line ack.
func TestWake_AsyncCompletionDoesNotRecordSummarized(t *testing.T) {
	m := liveModel(80)
	m.activeWake = []domain.QueueEvent{{
		Source: domain.SourceAsyncTool,
		Title:  "Async finished: npm test",
		Target: &domain.EventTarget{TerminalID: "term_9", AsyncInvocationID: "asy_1"},
	}}

	next, _ := m.onWakeComplete(WakeCompleteMsg{Reply: "Tests passed; continuing.", Failed: false})
	nm := next.(Model)

	if _, ok := nm.summarizedTerminals["term_9"]; ok {
		t.Fatal("an async completion must not poison the watcher summarized-terminals memory")
	}
}
