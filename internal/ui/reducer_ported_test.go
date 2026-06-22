package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
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
	m.queuedInput = []queuedTurn{{prompt: "x", cellID: "q1"}}
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
	// Queue + wake + active turn are dropped.
	if len(nm.queuedInput) != 0 || len(nm.pendingWake) != 0 || nm.activeTurn != "" {
		t.Error("clear must drop the queue, wake burst, and active turn")
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

// --- work serialization: FIFO queue + cancel + pullback (controllerQueueCancel) ---

func TestQueue_QueuesFollowupWhileBusyAndDrainsFIFO(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	// Two follow-ups typed while busy → both queue (FIFO), neither starts yet.
	next, _ = m.onSubmit("second")
	m = next.(Model)
	next, _ = m.onSubmit("third")
	m = next.(Model)
	if len(m.queuedInput) != 2 {
		t.Fatalf("queueDepth = %d, want 2", len(m.queuedInput))
	}
	if m.queuedInput[0].prompt != "second" || m.queuedInput[1].prompt != "third" {
		t.Errorf("FIFO order wrong: %+v", m.queuedInput)
	}
	// The active turn ends → "second" drains (depth drops to 1, not 2).
	m.activeTurn = ""
	m.inFlight = false
	next, _ = m.onTurnComplete(TurnCompleteMsg{Reply: "done"})
	m = next.(Model)
	if len(m.queuedInput) != 1 || m.queuedInput[0].prompt != "third" {
		t.Errorf("after drain, remaining queue = %+v, want [third]", m.queuedInput)
	}
}

func TestQueue_SlashCommandNotQueuedAsTurn(t *testing.T) {
	// A slash command runs as a command, never queued behind the in-flight turn.
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	depth := len(m.queuedInput)
	next, _ = m.onSubmit("/clear")
	m = next.(Model)
	if len(m.queuedInput) != depth {
		t.Errorf("slash command must not enter the turn queue: depth %d", len(m.queuedInput))
	}
}

func TestQueue_CancelSetsCancellingThenDrains(t *testing.T) {
	// Cancelling the in-flight turn sets Cancelling synchronously; a queued follow-up
	// still drains once the turn seals.
	m := liveModel(80)
	next, _ := m.startTurn("first")
	m = next.(Model)
	next, _ = m.onSubmit("queued")
	m = next.(Model)
	next, _ = m.onCancel()
	m = next.(Model)
	if c := m.activeTurnCell(); c == nil || c.Phase != domain.PhaseCancelling {
		t.Fatalf("cancel must set Cancelling synchronously: %v", c)
	}
	// Seal the cancelled turn → the queued follow-up promotes in place.
	next, _ = m.onTurnComplete(TurnCompleteMsg{Reply: domain.CancelledReply})
	m = next.(Model)
	if len(m.queuedInput) != 0 {
		t.Errorf("queued follow-up should have drained, queue = %+v", m.queuedInput)
	}
	if m.activeTurn == "" {
		t.Error("queued follow-up should have become the active turn")
	}
}

func TestQueue_QueuedTurnVisibleDimmedInFooter(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("first task")
	m = next.(Model)
	next, _ = m.onSubmit("second task")
	m = next.(Model)
	q := m.transcript[len(m.transcript)-1].Turn
	if q == nil || !q.Queued || q.UserText != "second task" {
		t.Fatalf("queued cell wrong: %+v", q)
	}
	// It is visible (rendered) in the live footer.
	if strings.TrimSpace(stripAnsi(m.liveCellsView(m.contentW()))) == "" {
		t.Error("queued follow-up must be visible in the footer")
	}
}
