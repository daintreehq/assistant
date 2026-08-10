package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// async_activity_test.go locks the async-pending tool state: a tool result that
// carries an accepted AsyncHandle renders as the distinct yellow ● "running
// asynchronously" row — never a green ✓ (which would read "finished") — survives
// a user cancel untouched (the runtime is still watching the work), and is
// terminal for the incremental flush (the row never mutates in this turn).

func asyncPendingTurn() (*TurnCell, string) {
	cell := &TurnCell{ID: "turn_a", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	cell.Steps = []TurnStep{
		{Kind: StepTool, Activity: &Activity{ID: "async", Name: "terminal.run.async", State: ActQueued}},
	}
	return cell, "async"
}

func TestToolResultWithAsyncHandleRendersPending(t *testing.T) {
	m := harnessModel()
	cell, callID := asyncPendingTurn()
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID

	res := domain.Ok("Started asynchronously: \"npm test\" is running.", nil)
	res.Async = &domain.AsyncHandle{ID: "asy_1", ToolName: "terminal.run.async", Title: "npm test"}
	m.applyPumpEvent(pumpEvent{kind: pumpToolResult, result: agent.ToolResultEvent{ID: callID, Result: res, EndedAt: 2}})

	a := cell.findActivity(callID)
	if a.State != ActAsyncPending {
		t.Fatalf("state = %v, want ActAsyncPending", a.State)
	}

	th := darkTheme()
	row := stripAnsi(renderActivityRow(th, *a, true, false, 0, domain.NowMS(), 90))
	if !strings.Contains(row, th.Glyphs.Async) {
		t.Errorf("row missing the async glyph %q: %q", th.Glyphs.Async, row)
	}
	if !strings.Contains(row, "running asynchronously") {
		t.Errorf("row must read as still-running: %q", row)
	}
	if strings.Contains(row, th.Glyphs.Done) {
		t.Errorf("an async-pending row must not show the done ✓: %q", row)
	}
}

func TestAsyncPendingSurvivesCancelPending(t *testing.T) {
	cell, callID := asyncPendingTurn()
	cell.findActivity(callID).State = ActAsyncPending
	cell.cancelPending()
	if got := cell.findActivity(callID).State; got != ActAsyncPending {
		t.Fatalf("cancelPending re-stamped an async-pending row to %v", got)
	}
}

func TestAsyncPendingIsTerminalForFlush(t *testing.T) {
	// A closed tool run whose only member is async-pending must be flushable —
	// the row never mutates again in this turn (its completion is a separate
	// wake turn), so holding it in the mutable footer would be pure churn.
	cell := &TurnCell{ID: "turn_f", State: TurnActive}
	cell.Steps = []TurnStep{
		{Kind: StepProse, Text: "starting work"},
		{Kind: StepTool, Activity: &Activity{ID: "a", Name: "terminal.run.async", State: ActAsyncPending}},
		{Kind: StepProse, Text: "reported to the user"},
		{Kind: StepProse, Text: "live tail"},
	}
	if got := finalizedStepCount(cell); got != 3 {
		t.Fatalf("finalizedStepCount = %d, want 3 (prose + closed async run + settled prose)", got)
	}
}
