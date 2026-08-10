package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// turn_completion_cue_test.go locks the #317 contract: a turn that COMPLETES with prose
// and no tool call renders as preamble + prose and nothing else (renderLiveStatus goes
// silent the instant a turn seals), so it is indistinguishable from one still streaming.
// The fix is a standalone MUTED note appended AFTER the turn — never a step on the turn
// itself, which would touch the flush/seal byte-exact prefix contract.

// settledProse is a finished prose step — the shape a callless turn seals with.
func settledProse(text string) TurnStep { return proseStep(text, false) }

// doneTool is one settled tool call — the ledger row that makes a turn read as "acted".
func doneTool(id string) TurnStep { return toolStep(id, "terminal.list", "", ActDone) }

// countCue reports how many standalone notes carry the end-of-turn cue.
func countCue(m Model) int {
	n := 0
	for _, txt := range noteTexts(m) {
		if txt == noActionCueText {
			n++
		}
	}
	return n
}

// The gate is exact: ONLY a normally-completed turn that produced prose and attempted no
// tool call earns the cue. Every other shape already carries its own terminal signal (a
// settled ✓/× ledger row, "Turn cancelled.", the failure path) and must not double up.
func TestNeedsNoActionCue_ExactGate(t *testing.T) {
	cases := []struct {
		name string
		cell *TurnCell
		want bool
		why  string
	}{
		{name: "nil turn", cell: nil, want: false, why: "no turn to annotate"},
		{
			name: "complete prose-only",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{settledProse("I'll read the terminals.")}},
			want: true, why: "the exact #317 shape",
		},
		{
			name: "still active",
			cell: &TurnCell{State: TurnActive, Steps: []TurnStep{settledProse("working…")}},
			want: false, why: "the live status still renders for an active turn",
		},
		{
			name: "failed",
			cell: &TurnCell{State: TurnFailed, Steps: []TurnStep{settledProse("boom")}},
			want: false, why: "the failure path surfaces its own note",
		},
		{
			name: "cancelled",
			cell: &TurnCell{State: TurnCancelled, Steps: []TurnStep{settledProse("half a th")}},
			want: false, why: `"Turn cancelled." is already the durable marker`,
		},
		{
			name: "complete, no prose at all",
			cell: &TurnCell{State: TurnComplete},
			want: false, why: "nothing rendered to mark the end of",
		},
		{
			name: "complete, whitespace-only prose",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{{Kind: StepProse, Text: ""}}},
			want: false, why: "an empty prose step renders nothing",
		},
		{
			name: "tool-only",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{doneTool("c1")}},
			want: false, why: "the settled ledger row already reads as done",
		},
		{
			name: "prose then tool",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{settledProse("reading now"), doneTool("c1")}},
			want: false, why: "action was taken",
		},
		{
			name: "tool then prose",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{doneTool("c1"), settledProse("all three are idle")}},
			want: false, why: "action was taken",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsNoActionCue(tc.cell); got != tc.want {
				t.Fatalf("needsNoActionCue = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// The cue lands as a SEPARATE muted transcript cell, after the turn — the turn's own
// steps must be byte-identical to what streamed, or the seal would disagree with the
// rows the incremental flush already committed to scrollback.
func TestTurnComplete_CalllessProseAppendsMutedStandaloneCue(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", UserText: "read the five terminals", State: TurnActive, Phase: domain.PhaseGenerating}
	cell.appendProse("I'm about to read all five terminals.")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true
	stepsBefore := len(cell.Steps)

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "I'm about to read all five terminals."})
	nm := next.(Model)

	last := nm.transcript[len(nm.transcript)-1]
	if last.Note == nil {
		t.Fatalf("a callless completion must append a standalone note cell, got %+v", last)
	}
	if last.Note.Text != noActionCueText {
		t.Fatalf("cue text = %q, want %q", last.Note.Text, noActionCueText)
	}
	if !last.Note.Muted {
		t.Error("the end-of-turn cue must render muted — it fires on most callless turns")
	}
	if last.Note.Level != NoteInfo {
		t.Errorf("cue level = %v, want NoteInfo (neutral — not success green)", last.Note.Level)
	}
	// The cue is a SIBLING cell, never a step: the sealed turn must render exactly what
	// the flush already committed.
	turn := nm.transcript[0].Turn
	if len(turn.Steps) != stepsBefore {
		t.Fatalf("the sealing turn gained %d step(s) — the cue must never mutate the turn", len(turn.Steps)-stepsBefore)
	}
	rendered := stripAnsi(renderTurn(nm.theme, nm.md, turn, 80, 80, false, 0, domain.NowMS()))
	if strings.Contains(rendered, noActionCueText) {
		t.Fatalf("the cue leaked into the turn's own render: %q", rendered)
	}
}

// A turn that called a tool already settles a ✓/× ledger row, so it must not also get
// the cue — otherwise every ordinary turn ends with a redundant line.
func TestTurnComplete_ToolTurnDoesNotAppendCue(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{
		ID: "turn_1", UserText: "list the terminals", State: TurnActive, Phase: domain.PhaseGenerating,
		Steps: []TurnStep{settledProse("Listing them now."), doneTool("call_1")},
	}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "Listing them now."})
	if got := countCue(next.(Model)); got != 0 {
		t.Fatalf("a tool turn must not carry the end-of-turn cue, got %d", got)
	}
}

// A cancelled turn already has its durable "Turn cancelled." marker; adding the cue on
// top would contradict it (the turn did not reach a normal end).
func TestTurnComplete_CancelledGetsOnlyTheCancelNote(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", UserText: "do a thing", State: TurnActive, Phase: domain.PhaseGenerating}
	cell.appendProse("Starting on that…")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: domain.CancelledReply})
	nm := next.(Model)

	if got := countCue(nm); got != 0 {
		t.Fatalf("a cancelled turn must not carry the end-of-turn cue, got %d", got)
	}
	if notes := noteTexts(nm); len(notes) != 1 || notes[0] != "Turn cancelled." {
		t.Fatalf("expected only the cancellation marker, got %q", notes)
	}
}

// An autonomous wake seals into the same prose-then-silence shape, so it earns the same
// boundary marker — and it also separates two consecutive wake turns in scrollback.
func TestWakeComplete_CalllessProseAppendsMutedStandaloneCue(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "wake_1", State: TurnActive, Phase: domain.PhaseGenerating}
	cell.appendProse("term_1 finished cleanly; nothing needs you.")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "wake_1"
	m.inFlight = true
	m.activeWake = []domain.QueueEvent{wakeEvent("term_1", "agent exited")}

	next, _ := m.onWakeComplete(WakeCompleteMsg{RunID: "wake_1", Reply: "term_1 finished cleanly; nothing needs you."})
	nm := next.(Model)

	if got := countCue(nm); got != 1 {
		t.Fatalf("a callless wake must carry exactly one end-of-turn cue, got %d", got)
	}
	last := nm.transcript[len(nm.transcript)-1]
	if last.Note == nil || !last.Note.Muted {
		t.Fatalf("the wake cue must be a muted standalone note, got %+v", last)
	}
	// The wake still settles normally — the cue is purely additive.
	if nm.inFlight || nm.activeTurn != "" {
		t.Error("the cue must not disturb wake settling (inFlight/activeTurn must clear)")
	}
}

// A FAILED wake is already recorded as such; a "no action taken" marker would read as a
// clean end and mask the failure.
func TestWakeComplete_FailedWakeGetsNoCue(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "wake_1", State: TurnActive, Phase: domain.PhaseGenerating}
	cell.appendProse("Model unavailable: offline")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "wake_1"
	m.inFlight = true
	m.activeWake = []domain.QueueEvent{wakeEvent("term_2", "agent waiting")}

	next, _ := m.onWakeComplete(WakeCompleteMsg{RunID: "wake_1", Reply: "Model unavailable: offline"})
	if got := countCue(next.(Model)); got != 0 {
		t.Fatalf("a failed wake must not carry the end-of-turn cue, got %d", got)
	}
}

// The cue renders as ONE plain row: toned spine + info glyph + a DIM body. Dim (an
// attribute-only faint) is what keeps a per-turn marker from competing with the prose
// above it — an ordinary note keeps full body weight.
func TestRenderNoteCell_MutedEndOfTurnCue(t *testing.T) {
	th := darkTheme()
	muted := renderNoteCell(th, &NoteCell{Level: NoteInfo, Text: noActionCueText, Muted: true}, 120)

	plain := stripAnsi(muted)
	if !strings.Contains(plain, noActionCueText) {
		t.Fatalf("cue text missing from render: %q", plain)
	}
	if strings.Contains(plain, "\n") {
		t.Fatalf("the cue must render as a single row, got %q", plain)
	}
	if strings.TrimSpace(plain) == "" {
		t.Fatal("the cue must never render blank — tea.Println drops a whitespace-only line")
	}
	// The muted body is styled differently from the same note at full weight; the toned
	// spine/glyph prefix is identical, so any diff is the body treatment.
	full := renderNoteCell(th, &NoteCell{Level: NoteInfo, Text: noActionCueText}, 120)
	if muted == full {
		t.Fatal("a muted note must render its body differently from a full-weight one")
	}
	if !strings.Contains(muted, th.Dim().Render(noActionCueText)) {
		t.Fatalf("the muted body must use the theme's dim style, got %q", muted)
	}
	// Muted is opt-in: every existing note keeps full body weight.
	if !strings.Contains(full, th.Body().Render(noActionCueText)) {
		t.Fatalf("a non-muted note must keep the body style, got %q", full)
	}
}
