package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// queue_test.go covers the durable cancelled marker and the queued-turn retract.

func TestTurnComplete_CancelledLeavesDurableMarker(t *testing.T) {
	m := harnessModel()
	cell := &TurnCell{ID: "turn_c", State: TurnActive, Phase: domain.PhaseCancelling, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: cell.ID, Reply: domain.CancelledReply})
	mm := asModel(t, next)

	if cell.State != TurnCancelled {
		t.Fatalf("turn did not seal as cancelled; state=%v", cell.State)
	}
	var foundNote bool
	for _, c := range mm.transcript {
		if c.Note != nil && strings.Contains(c.Note.Text, "cancelled") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatal("a cancelled turn left no durable 'cancelled' marker in the transcript")
	}
}

func TestEscWhileBusy_RetractsNewestQueued(t *testing.T) {
	m := harnessModel()
	// An active turn plus two queued follow-ups.
	active := &TurnCell{ID: "turn_a", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	q1 := &TurnCell{ID: "turn_q1", UserText: "first queued", State: TurnActive, Queued: true}
	q2 := &TurnCell{ID: "turn_q2", UserText: "second queued", State: TurnActive, Queued: true}
	m.transcript = append(m.transcript,
		TranscriptCell{Turn: active}, TranscriptCell{Turn: q1}, TranscriptCell{Turn: q2})
	m.activeTurn = active.ID
	m.inFlight = true
	m.queuedInput = []queuedTurn{{prompt: "first queued", cellID: q1.ID}, {prompt: "second queued", cellID: q2.ID}}

	next, _ := m.onEscWhileBusy()
	mm := asModel(t, next)

	if len(mm.queuedInput) != 1 || mm.queuedInput[0].cellID != q1.ID {
		t.Fatalf("retract did not pop the newest queued entry; queue=%+v", mm.queuedInput)
	}
	if mm.composer.Value() != "second queued" {
		t.Fatalf("retracted text not pulled into the composer; got %q", mm.composer.Value())
	}
	// The retracted cell is gone; the active turn and the still-queued cell remain.
	var hasQ2, hasQ1, hasActive bool
	for _, c := range mm.transcript {
		if c.Turn == nil {
			continue
		}
		switch c.Turn.ID {
		case q2.ID:
			hasQ2 = true
		case q1.ID:
			hasQ1 = true
		case active.ID:
			hasActive = true
		}
	}
	if hasQ2 {
		t.Fatal("retracted queued cell still present in the transcript")
	}
	if !hasQ1 || !hasActive {
		t.Fatal("retract wrongly removed a cell it should have kept")
	}
}

func TestEscWhileBusy_NoQueueCancels(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{ID: "turn_a", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true

	next, _ := m.onEscWhileBusy()
	mm := asModel(t, next)
	if at := mm.activeTurnCell(); at == nil || at.Phase != domain.PhaseCancelling {
		t.Fatalf("Esc with no queue did not cancel the active turn; phase=%v", at)
	}
}

// A message submitted MID-TURN (queued behind the in-flight turn) renders in the live
// footer, NOT via the sealed-block path — so it must get the SAME marginTop. Regression
// for the missing blank line above a mid-turn "YOU" card (it was rendering flush against
// the in-flight assistant prose).
func TestLiveCellsView_MidTurnUserMessageHasBlankLineAbove(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{
		ID: "turn_a", UserText: "kick off the work", State: TurnActive,
		Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS(),
		Steps: []TurnStep{{Kind: StepProse, Text: "Working on it now."}},
	}
	queued := &TurnCell{ID: "turn_q", UserText: "actually, stop after round 2", State: TurnActive, Queued: true}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active}, TranscriptCell{Turn: queued})
	m.activeTurn = active.ID
	m.inFlight = true

	lines := strings.Split(stripAnsi(m.liveCellsView(80)), "\n")
	// The queued cell's "YOU" card is the LAST YOU in the live region; the line directly
	// above it must be blank (the margin), never the active turn's prose line.
	lastYou := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "YOU" {
			lastYou = i
		}
	}
	if lastYou <= 0 {
		t.Fatalf("queued user message did not render a YOU card: %q", lines)
	}
	if strings.TrimSpace(lines[lastYou-1]) != "" {
		t.Fatalf("mid-turn YOU card must have a blank line above it, got %q", lines[lastYou-1])
	}
}
