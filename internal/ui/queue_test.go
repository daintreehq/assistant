package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
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

func TestEscWhileBusy_RetractsNewestPendingInjection(t *testing.T) {
	m := harnessModel()
	// An active turn plus two messages typed mid-turn (buffered for injection, LIFO).
	active := &TurnCell{ID: "turn_a", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true
	fi := m.controller.inject.(*fakeInjector)
	fi.buf = []string{"first queued", "second queued"}
	m.pendingInject = 2

	next, _ := m.onEscWhileBusy()
	mm := asModel(t, next)

	// The newest pending message is pulled back into the composer; the cue decrements.
	if mm.pendingInject != 1 {
		t.Fatalf("pendingInject = %d after retract, want 1", mm.pendingInject)
	}
	if len(fi.buf) != 1 || fi.buf[0] != "first queued" {
		t.Fatalf("retract did not pop the newest buffered message; buf=%+v", fi.buf)
	}
	if mm.composer.Value() != "second queued" {
		t.Fatalf("retracted text not pulled into the composer; got %q", mm.composer.Value())
	}
	// The active turn is untouched (retract never cancels).
	if at := mm.activeTurnCell(); at == nil || at.Phase == domain.PhaseCancelling {
		t.Fatal("retract wrongly cancelled the active turn")
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

// A message folded into the running turn renders INLINE as an interjection step (its
// text appears in chronological place within the active turn's live view), not as a
// separate queued "YOU" card.
func TestLiveCellsView_InterjectionRendersInlineInTurn(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{
		ID: "turn_a", UserText: "kick off the work", State: TurnActive,
		Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS(),
		Steps: []TurnStep{
			{Kind: StepProse, Text: "Working on it now."},
			{Kind: StepInterject, Text: "actually, stop after round 2"},
		},
	}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true

	out := stripAnsi(m.liveCellsView(80))
	if !strings.Contains(out, "actually, stop after round 2") {
		t.Fatalf("interjection text not rendered inline in the turn:\n%s", out)
	}
	// It is part of the active turn — there is no separate second "YOU" card for it.
	if strings.Count(out, "YOU") > 1 {
		t.Fatalf("interjection wrongly rendered as a separate YOU card:\n%s", out)
	}
}
