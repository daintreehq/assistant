package ui

import (
	"strings"
	"testing"
)

// command_progress_test.go pins the slash-command liveness cue: a running command (the
// model-backed /compact runs two serial backend calls — tens of seconds) must show a
// live spinner line in the footer instead of leaving the cockpit looking idle, update
// that line from pump CommandProgress events, and retire it on completion.

// submitCommand drives onSubmit with a slash line and returns the resulting model.
func submitCommand(t *testing.T, m Model, line string) Model {
	t.Helper()
	next, _ := m.onSubmit(line)
	return asModel(t, next)
}

func TestCommandLiveCue_ShowsWhileRunning(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	if mm.commandsRunning != 1 {
		t.Fatalf("commandsRunning = %d, want 1", mm.commandsRunning)
	}
	band := mm.bottomBand(80)
	if !strings.Contains(band, "Running /compact…") {
		t.Fatalf("footer should show the command liveness cue, got:\n%s", band)
	}
}

func TestCommandLiveCue_ProgressUpdatesStage(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	mm.applyPumpEvent(pumpEvent{kind: pumpCommandProgress, msg: "Checkpointing conversation…"})
	if !strings.Contains(mm.bottomBand(80), "Checkpointing conversation…") {
		t.Fatalf("footer should show the reported stage, got:\n%s", mm.bottomBand(80))
	}
	mm.applyPumpEvent(pumpEvent{kind: pumpCommandProgress, msg: "Distilling memories…"})
	if !strings.Contains(mm.bottomBand(80), "Distilling memories…") {
		t.Fatalf("footer should show the newest stage, got:\n%s", mm.bottomBand(80))
	}
}

func TestCommandLiveCue_ClearsOnComplete(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	next, _ := mm.onCommandComplete(CommandCompleteMsg{Tracked: true, Title: "Compact", Text: "done"})
	fin := asModel(t, next)
	if fin.commandsRunning != 0 || fin.commandStage != "" {
		t.Fatalf("cue not retired: running=%d stage=%q", fin.commandsRunning, fin.commandStage)
	}
	if strings.Contains(fin.bottomBand(80), "Running /compact…") {
		t.Fatal("footer still shows the liveness cue after completion")
	}
}

// TestCommandLiveCue_StaleProgressIgnored proves a progress event that lands AFTER the
// command completed (goroutine race) cannot resurrect the cue.
func TestCommandLiveCue_StaleProgressIgnored(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	next, _ := mm.onCommandComplete(CommandCompleteMsg{Tracked: true, Title: "Compact", Text: "done"})
	fin := asModel(t, next)
	fin.applyPumpEvent(pumpEvent{kind: pumpCommandProgress, msg: "Distilling memories…"})
	if fin.commandStage != "" {
		t.Fatalf("stale progress must be ignored, got stage %q", fin.commandStage)
	}
}

// TestCommandLiveCue_UntrackedCompleteDoesNotRetire pins the /approvals hazard: a
// SYNCHRONOUS completion (which never incremented the counter) must not decrement a
// still-running command's count and kill its cue mid-run.
func TestCommandLiveCue_UntrackedCompleteDoesNotRetire(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	// The /approvals shortcut completes synchronously, untracked.
	next, _ := mm.onCommandComplete(CommandCompleteMsg{Title: "Approvals", Text: "none"})
	fin := asModel(t, next)
	if fin.commandsRunning != 1 {
		t.Fatalf("untracked completion retired a running command: running=%d", fin.commandsRunning)
	}
	if !strings.Contains(fin.bottomBand(80), "Running /compact…") {
		t.Fatal("the running command's cue must survive an untracked completion")
	}
}

// TestCommandLiveCue_UnhandledCompletionRetiresSilently pins the bare-"/" hazard: a
// submission the commands package rejects must STILL retire the counter (else the
// spinner ticks forever) without printing a result card.
func TestCommandLiveCue_UnhandledCompletionRetiresSilently(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/")
	if mm.commandsRunning != 1 {
		t.Fatalf("submit should track the command, running=%d", mm.commandsRunning)
	}
	cards := len(mm.transcript)
	next, _ := mm.onCommandComplete(CommandCompleteMsg{Tracked: true, Unhandled: true})
	fin := asModel(t, next)
	if fin.commandsRunning != 0 || fin.commandStage != "" {
		t.Fatalf("unhandled completion must retire the cue: running=%d stage=%q",
			fin.commandsRunning, fin.commandStage)
	}
	if len(fin.transcript) != cards {
		t.Fatal("an unhandled completion must not print a result card")
	}
}

// TestCommandLiveCue_WidthBounded: the liveness row must truncate to the band width —
// footer height accounting counts newline rows, so a soft-wrapping over-wide row would
// corrupt the budget (#1613 class).
func TestCommandLiveCue_WidthBounded(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	mm.applyPumpEvent(pumpEvent{kind: pumpCommandProgress,
		msg: strings.Repeat("Checkpointing a very long conversation ", 10)})
	if got := cellWidth(stripAnsi(mm.commandLiveView(40))); got > 40 {
		t.Fatalf("command live row is %d cells wide, want ≤ 40", got)
	}
}

// TestCommandLiveCue_ProgressDoesNotTouchTurnHeartbeat: a command's stage update must
// not refresh the ACTIVE TURN's LastActivityAt (it would postpone the turn's
// "still working" stall cue).
func TestCommandLiveCue_ProgressDoesNotTouchTurnHeartbeat(t *testing.T) {
	mm := harnessModel()
	// An active turn with a sentinel heartbeat: a refresh would overwrite it.
	cell := &TurnCell{ID: "turn_hb", State: TurnActive, LastActivityAt: 12345}
	mm.transcript = append(mm.transcript, TranscriptCell{Turn: cell})
	mm.activeTurn = cell.ID
	mm.inFlight = true
	mm.commandsRunning = 1
	mm.applyPumpEvent(pumpEvent{kind: pumpCommandProgress, msg: "Checkpointing conversation…"})
	if cell.LastActivityAt != 12345 {
		t.Fatalf("command progress refreshed the turn heartbeat: %d", cell.LastActivityAt)
	}
}

// TestCommandLiveCue_TurnTakesPrecedence: with BOTH a turn and a command in flight the
// footer shows the turn's own live status, not a second concurrent spinner line.
func TestCommandLiveCue_TurnTakesPrecedence(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/status")
	mm.inFlight = true
	if strings.Contains(mm.bottomBand(80), "Running /status…") {
		t.Fatalf("turn liveness should take precedence over the command cue, got:\n%s", mm.bottomBand(80))
	}
}

// TestCommandLiveCue_ArmsSpinner: submitting a command starts the spinner tick even
// with no turn in flight, so the cue actually animates.
func TestCommandLiveCue_ArmsSpinner(t *testing.T) {
	m := harnessModel()
	mm := submitCommand(t, m, "/compact")
	if !mm.spinnerRunning {
		t.Fatal("spinner tick should arm while a command runs")
	}
	// The tick keeps re-arming while the command runs, then lapses once it settles.
	next, _ := mm.Update(spinnerTickMsg{})
	mid := asModel(t, next)
	if !mid.spinnerRunning {
		t.Fatal("spinner should keep running mid-command")
	}
	done, _ := mid.onCommandComplete(CommandCompleteMsg{Tracked: true, Title: "Compact", Text: "done"})
	fin := asModel(t, done)
	after, _ := fin.Update(spinnerTickMsg{})
	if asModel(t, after).spinnerRunning {
		t.Fatal("spinner should lapse once the command settles")
	}
}
