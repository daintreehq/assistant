package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// reducer_ported_test.go exercises the transcript-reducer and controller
// queue/cancel/clear behaviors (transcript reducer, controller clear-terminal,
// controller queue-cancel, scrollback reset) on the Go Update reducers.
// The Bubble Tea cockpit drives these through pump events + the work-serialization
// slice, so we assert the synchronous state mutations directly.

// --- transcript reducer: ordered steps + append-only + clear ---

func TestReducer_SecondProseAfterToolIsDistinctStep(t *testing.T) {
	// A prose step after a tool batch is APPENDED (a new StepProse), not merged into
	// the pre-batch prose — the "integrating" beat reads as its own step.
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"

	m.applyPumpEvent(pumpEvent{kind: pumpTokens, text: "Analyzing."})
	m.applyPumpEvent(pumpEvent{kind: pumpBatch, batch: []agent.BatchedToolCall{{ID: "a", Name: "fs.read"}}})
	m.applyPumpEvent(pumpEvent{kind: pumpTokens, text: "Integrating."})

	var prose []string
	for _, s := range cell.Steps {
		if s.Kind == StepProse {
			prose = append(prose, s.Text)
		}
	}
	if len(prose) != 2 || prose[0] != "Analyzing." || prose[1] != "Integrating." {
		t.Fatalf("post-batch prose must be a distinct step: %v", prose)
	}
}

func TestReducer_AppendOnlyRetention(t *testing.T) {
	// Out-of-band notes append to the transcript; nothing is front-pruned.
	m := liveModel(80)
	for i := 0; i < 5; i++ {
		m.addNote(NoteInfo, "note "+itoa(i))
	}
	if len(m.transcript) != 5 {
		t.Fatalf("append-only retention broke: len = %d, want 5", len(m.transcript))
	}
	if m.transcript[0].Note == nil || m.transcript[0].Note.Text != "note 0" {
		t.Error("oldest note must be retained (no front-pruning)")
	}
}

func TestReducer_ClearWipesThenFreshCard(t *testing.T) {
	m := liveModel(80)
	m.transcript = []TranscriptCell{
		{Turn: &TurnCell{ID: "t1", State: TurnComplete}},
		{Turn: &TurnCell{ID: "t2", State: TurnComplete}},
	}
	m.pendingInject = 1
	m.controller.inject.(*fakeInjector).buf = []string{"x"}
	m.pendingWake = []domain.QueueEvent{{Title: "w"}}

	next, _ := m.onClear("Conversation cleared", "fresh start")
	nm := next.(Model)
	// The transcript is wiped down to the single confirmation card.
	if len(nm.transcript) != 1 {
		t.Fatalf("clear must reset to one card, got %d", len(nm.transcript))
	}
	if nm.transcript[0].Command == nil || nm.transcript[0].Command.Title != "Conversation cleared" {
		t.Errorf("clear card wrong: %+v", nm.transcript[0])
	}
	// Pending injection + wake + active turn are dropped.
	if nm.pendingInject != 0 || len(nm.controller.inject.(*fakeInjector).buf) != 0 {
		t.Error("clear must drop buffered injections")
	}
	if len(nm.pendingWake) != 0 || nm.activeTurn != "" {
		t.Error("clear must drop the wake burst and active turn")
	}
	// The reset key advanced (re-arms the commit queue masthead + card).
	if nm.clearNonce == m.clearNonce {
		t.Error("clear must bump the clear nonce (re-arm scrollback)")
	}
}

func TestReducer_SecondClearReArmsAgain(t *testing.T) {
	// A second /clear is not de-duped — the nonce advances each time so the host wipe
	// + re-commit run again.
	m := liveModel(80)
	next, _ := m.onClear("A", "")
	m1 := next.(Model)
	n1 := m1.clearNonce
	next, _ = m1.onClear("B", "")
	m2 := next.(Model)
	if m2.clearNonce == n1 {
		t.Error("a second clear must bump the nonce again (not de-duped)")
	}
}

func TestReducer_RedrawKeepsTranscript(t *testing.T) {
	// requestRedraw (settled resize) re-commits the masthead + transcript fresh but
	// keeps the conversation — unlike /clear.
	m := liveModel(80)
	m.addNote(NoteInfo, "seed-line")
	lenBefore := len(m.transcript)
	m.resizePending = 7
	next, _ := m.onRedraw(RedrawMsg{Nonce: 7})
	nm := next.(Model)
	if len(nm.transcript) != lenBefore {
		t.Errorf("redraw must keep the transcript: %d != %d", len(nm.transcript), lenBefore)
	}
	if nm.transcript[0].Note == nil || nm.transcript[0].Note.Text != "seed-line" {
		t.Error("redraw must preserve the seed cell")
	}
	if nm.redrawNonce == m.redrawNonce {
		t.Error("redraw must bump the redraw nonce")
	}
}

func TestReducer_StaleRedrawIsNoOp(t *testing.T) {
	// A superseded (older nonce) redraw must be a no-op (debounce coalesces a burst).
	m := liveModel(80)
	m.resizePending = 9
	before := m.redrawNonce
	next, cmd := m.onRedraw(RedrawMsg{Nonce: 3})
	nm := next.(Model)
	if nm.redrawNonce != before || cmd != nil {
		t.Error("a stale redraw nonce must be a no-op")
	}
}

// --- work serialization: mid-turn injection (fold into the running turn) ---

func TestInject_BuffersFollowupsWhileBusyInOrder(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	before := len(m.transcript)
	// Two messages typed while busy → both buffer for the running turn (no new cells).
	next, _ = m.onSubmit("second")
	m = next.(Model)
	next, _ = m.onSubmit("third")
	m = next.(Model)
	if len(m.transcript) != before {
		t.Fatalf("typed-while-busy messages wrongly created new cells: %d → %d", before, len(m.transcript))
	}
	if m.pendingInject != 2 {
		t.Fatalf("pendingInject = %d, want 2", m.pendingInject)
	}
	fi := m.controller.inject.(*fakeInjector)
	if len(fi.buf) != 2 || fi.buf[0] != "second" || fi.buf[1] != "third" {
		t.Errorf("buffered order wrong: %+v", fi.buf)
	}
	// Completing the turn does NOT spawn a deferred user turn — the messages were folded
	// into the turn that just ran (the buffer is the Session's; nothing drains here).
	m.activeTurn = ""
	m.inFlight = false
	next, _ = m.onTurnComplete(TurnCompleteMsg{Reply: "done"})
	m = next.(Model)
	if m.activeTurn != "" {
		t.Errorf("turn completion must not start a new user turn, activeTurn=%q", m.activeTurn)
	}
}

func TestInject_SlashCommandNotBuffered(t *testing.T) {
	// A slash command runs as a command, never buffered as an injection.
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	depth := m.pendingInject
	next, _ = m.onSubmit("/help")
	m = next.(Model)
	if m.pendingInject != depth {
		t.Errorf("slash command must not buffer as an injection: pendingInject %d", m.pendingInject)
	}
}

func TestInject_CancelSetsCancellingAndDropsPending(t *testing.T) {
	// Cancelling the in-flight turn sets Cancelling synchronously AND drops any message
	// typed mid-turn but not yet folded in (it was meant for the abandoned work).
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	next, _ = m.onSubmit("steer")
	m = next.(Model)
	next, _ = m.onCancel()
	m = next.(Model)
	if c := m.activeTurnCell(); c == nil || c.Phase != domain.PhaseCancelling {
		t.Fatalf("cancel must set Cancelling synchronously: %v", c)
	}
	if m.pendingInject != 0 || len(m.controller.inject.(*fakeInjector).buf) != 0 {
		t.Errorf("cancel must drop buffered injections; pendingInject=%d buf=%+v",
			m.pendingInject, m.controller.inject.(*fakeInjector).buf)
	}
}

func TestInject_PendingCueShownWhileBusy(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("first task")
	m = next.(Model)
	next, _ = m.onSubmit("second task")
	m = next.(Model)
	// No separate cell; the pending cue surfaces in the composer.
	if m.pendingInject != 1 {
		t.Fatalf("pendingInject = %d, want 1", m.pendingInject)
	}
	if !strings.Contains(stripAnsi(m.composerView(80)), "queued") {
		t.Error("composer must surface the pending-injection cue while busy")
	}
}
