package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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
	m.pendingInjects = []string{"first queued", "second queued"}

	next, _ := m.onEscWhileBusy()
	mm := asModel(t, next)

	// The newest pending message is pulled back into the composer; the queued card drops
	// that entry and keeps the older one — LIFO, mirroring the Session.
	if len(mm.pendingInjects) != 1 || mm.pendingInjects[0] != "first queued" {
		t.Fatalf("pendingInjects = %+v after retract, want [first queued]", mm.pendingInjects)
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

// The Session folds buffered follow-ups in on its own schedule, and the interjection
// event that zeroes our count arrives later — so pendingInject can outlive the thing it
// counts. A retract in that window fails, and leaving the count up would keep the
// composer advertising "Esc edit follow-up" for a retract that can never succeed.
func TestEscWhileBusy_FailedRetractClearsTheStaleCount(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{ID: "turn_a", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true
	// The Session already folded it in: nothing buffered, but our count says otherwise.
	m.controller.inject.(*fakeInjector).buf = nil
	m.pendingInjects = []string{"already folded in"}

	mm := asModel(t, mustModel(m.onEscWhileBusy()))

	if len(mm.pendingInjects) != 0 {
		t.Fatalf("pendingInjects = %+v after a failed retract, want empty", mm.pendingInjects)
	}
	if mm.composer.Value() != "" {
		t.Fatalf("a failed retract must not put text in the composer; got %q", mm.composer.Value())
	}
	// A failed retract still is NOT a cancel — Ctrl+C owns that.
	if at := mm.activeTurnCell(); at == nil || at.Phase == domain.PhaseCancelling {
		t.Fatal("a failed retract wrongly cancelled the active turn")
	}
	// And the cue it was driving is gone.
	if out := ansi.Strip(mm.footer()); strings.Contains(out, "follow-up queued") {
		t.Errorf("the phantom queue cue survived:\n%s", out)
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
	// It is part of the active turn, not a second exchange: the mid-turn message renders
	// as its own card (interjectLabel) INSIDE the running turn, so exactly one bare "YOU"
	// anchor — the turn-opening card's — and exactly one "◆ DAINTREE" marker may appear.
	var anchors int
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == "YOU" {
			anchors++
		}
	}
	if anchors != 1 {
		t.Fatalf("want exactly 1 turn-opening YOU anchor, got %d — interjection rendered as a separate turn:\n%s", anchors, out)
	}
	// Exact counts, not presence: rendering the same mid-turn card twice inside one turn
	// (or dropping its anchor so it reads as the model's own prose) must both fail here.
	for _, want := range []struct {
		token string
		n     int
	}{
		{"DAINTREE", 1},
		{interjectLabel, 1},
		{"actually, stop after round 2", 1},
	} {
		if got := strings.Count(out, want.token); got != want.n {
			t.Fatalf("%q appears %d times, want %d:\n%s", want.token, got, want.n, out)
		}
	}
}
