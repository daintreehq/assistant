package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// cancel_activity_test.go locks the cancel-resolve fix: a turn the user aborts must not
// leave its announced-but-unrun tool rows frozen as ◦ queued / ◌ active in scrollback.
// The agent stubs CANCELLED results only into the model's message history (no UI
// ToolResult), so the UI must re-stamp the dangling rows itself at seal time.

func TestTurnComplete_CancelResolvesPendingActivities(t *testing.T) {
	m := harnessModel()
	cell := &TurnCell{ID: "turn_c", State: TurnActive, Phase: domain.PhaseCancelling, PhaseStartedAt: domain.NowMS()}
	// A batch with one already-done call, one mid-flight (active), one never-started (queued).
	cell.Steps = []TurnStep{
		{Kind: StepTool, Activity: &Activity{ID: "done", Name: "fs.read", State: ActDone, Detail: "a.go", StartedAt: 1, EndedAt: 2}},
		{Kind: StepTool, Activity: &Activity{ID: "live", Name: "fs.read", State: ActActive, Detail: "b.go", StartedAt: 1}},
		{Kind: StepTool, Activity: &Activity{ID: "wait", Name: "fs.read", State: ActQueued, Args: `{"path":"c.go"}`}},
	}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: cell.ID, Reply: domain.CancelledReply})
	_ = asModel(t, next)

	if cell.State != TurnCancelled {
		t.Fatalf("turn did not seal as cancelled; state=%v", cell.State)
	}
	// The already-completed call keeps its terminal result untouched.
	if got := cell.findActivity("done").State; got != ActDone {
		t.Fatalf("a completed call must stay ActDone, got %v", got)
	}
	// The mid-flight and never-started calls resolve to terminal ActCancelled.
	for _, id := range []string{"live", "wait"} {
		if got := cell.findActivity(id).State; got != ActCancelled {
			t.Fatalf("pending call %q must resolve to ActCancelled, got %v", id, got)
		}
	}

	th := darkTheme()
	// The aborted-in-flight row reads "cancelled" and shows neither a live spinner nor a
	// misleading duration.
	live := stripAnsi(renderActivityRow(th, *cell.findActivity("live"), false, false, 0, domain.NowMS(), 72))
	if !strings.Contains(live, "cancelled") {
		t.Errorf("aborted-in-flight row must say 'cancelled': %q", live)
	}
	// The never-started row reads "not run", keeps its target, and is NOT the queued glyph.
	wait := stripAnsi(renderActivityRow(th, *cell.findActivity("wait"), true, false, 0, domain.NowMS(), 72))
	if !strings.Contains(wait, "not run") {
		t.Errorf("never-started row must say 'not run': %q", wait)
	}
	if !strings.Contains(wait, "c.go") {
		t.Errorf("a cancelled row should still show its resolved target: %q", wait)
	}
	if strings.Contains(wait, th.Glyphs.Queued) {
		t.Errorf("a cancelled row must not render the ◦ queued glyph: %q", wait)
	}
}

// A normal (non-cancelled) completion must NEVER re-stamp activities — cancelPending is
// scoped to the abort path so a clean turn keeps its real done/failed outcomes.
func TestTurnComplete_CleanCompletionDoesNotCancelActivities(t *testing.T) {
	m := harnessModel()
	cell := &TurnCell{ID: "turn_ok", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	cell.Steps = []TurnStep{
		{Kind: StepTool, Activity: &Activity{ID: "d", Name: "fs.read", State: ActDone, Detail: "a.go", StartedAt: 1, EndedAt: 2}},
	}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: cell.ID, Reply: "all good"})
	_ = asModel(t, next)

	if cell.State == TurnCancelled {
		t.Fatalf("a clean reply must not seal as cancelled")
	}
	if got := cell.findActivity("d").State; got != ActDone {
		t.Fatalf("a clean completion must leave ActDone intact, got %v", got)
	}
}
