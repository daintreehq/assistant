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

// cueIndex is the transcript position of the end-of-turn cue, or -1.
func cueIndex(m Model) int {
	for i, c := range m.transcript {
		if c.Note != nil && c.Note.Text == noActionCueText {
			return i
		}
	}
	return -1
}

// callessTurn is a live turn that has streamed prose and called nothing — the #317 shape.
func callessTurn(id, user, prose string) *TurnCell {
	t := &TurnCell{ID: id, UserText: user, State: TurnActive, Phase: domain.PhaseGenerating}
	t.appendProse(prose)
	return t
}

// The gate is exact: ONLY a normally-completed turn that produced prose and attempted no
// tool call earns the cue. Every other shape already carries its own terminal signal (a
// settled ✓/× ledger row, "Turn cancelled.", the failure path) and must not double up.
func TestNeedsNoActionCue_ExactGate(t *testing.T) {
	cases := []struct {
		name  string
		cell  *TurnCell
		phase domain.RunPhase
		want  bool
		why   string
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
			name: "failed by state",
			cell: &TurnCell{State: TurnFailed, Steps: []TurnStep{settledProse("boom")}},
			want: false, why: "the failure path surfaces its own note",
		},
		{
			// The regression guard for the double-up: a mid-stream failure raises
			// PhaseFailed and pumpError adds an error note, but the completion can carry
			// plain reply text that seals the turn as TurnComplete. The ORDERED phase is
			// what makes it recognisable as a failure.
			name:  "complete by state but failed by ordered phase",
			cell:  &TurnCell{State: TurnComplete, Steps: []TurnStep{settledProse("Model error: upstream 500")}},
			phase: domain.PhaseFailed,
			want:  false, why: "pumpError already wrote an error note — the cue would contradict it",
		},
		{
			name: "cancelled",
			cell: &TurnCell{State: TurnCancelled, Steps: []TurnStep{settledProse("half a th")}},
			want: false, why: `"Turn cancelled." is already the durable marker`,
		},
		{
			name: "complete, no steps at all",
			cell: &TurnCell{State: TurnComplete},
			want: false, why: "nothing rendered to mark the end of",
		},
		{
			name: "complete, empty prose step",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{{Kind: StepProse, Text: ""}}},
			want: false, why: "an empty prose step is not prose",
		},
		{
			// Whitespace-only prose IS prose to the gate. The markdown renderer drops it,
			// so the turn renders as a bare marker — which is precisely the ambiguity the
			// cue exists to resolve, so firing is correct.
			name: "complete, whitespace-only prose",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{settledProse("  \n\t")}},
			want: true, why: "renders as a bare marker — the most ambiguous shape of all",
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
		{
			// A StepTool is appended when the batch is ANNOUNCED, so an unsettled or
			// failed call still counts as action attempted — its ledger row is visible.
			name: "prose then a FAILED tool",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{
				settledProse("reading now"), toolStep("c1", "terminal.read", "", ActFailed)}},
			want: false, why: "an announced call is action, however it settled",
		},
		{
			name: "prose then a still-QUEUED tool",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{
				settledProse("reading now"), toolStep("c1", "terminal.read", "", ActQueued)}},
			want: false, why: "an announced call is action, however it settled",
		},
		{
			// Scoping decision, pinned deliberately: a skill card or an interjection is
			// not the callless-prose shape, and "no action taken" would misdescribe it.
			name: "skill card only, no prose",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{{Kind: StepSkill, Text: "Terminal supervision"}}},
			want: false, why: "loading a skill is not 'no action'",
		},
		{
			name: "interjection only, no prose",
			cell: &TurnCell{State: TurnComplete, Steps: []TurnStep{{Kind: StepInterject, Text: "also check term_2"}}},
			want: false, why: "not the callless-prose shape",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsNoActionCue(tc.cell, tc.phase); got != tc.want {
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
	cell := callessTurn("turn_1", "read the five terminals", "I'm about to read all five terminals.")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true
	stepsBefore := len(cell.Steps)
	cellsBefore := len(m.transcript)

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "I'm about to read all five terminals."})
	nm := next.(Model)

	// EXACTLY one new cell, and it is the cue.
	if grew := len(nm.transcript) - cellsBefore; grew != 1 {
		t.Fatalf("transcript grew by %d cells, want exactly 1", grew)
	}
	if got := countCue(nm); got != 1 {
		t.Fatalf("expected exactly one end-of-turn cue, got %d", got)
	}
	note := nm.transcript[len(nm.transcript)-1].Note
	if note == nil {
		t.Fatalf("the new cell must be a standalone note, got %+v", nm.transcript[len(nm.transcript)-1])
	}
	if note.Text != noActionCueText {
		t.Fatalf("cue text = %q, want %q", note.Text, noActionCueText)
	}
	if !note.Muted {
		t.Error("the end-of-turn cue must render muted — it fires on most callless turns")
	}
	if note.Level != NoteInfo {
		t.Errorf("cue level = %v, want NoteInfo (neutral — not success green)", note.Level)
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

	// A REDELIVERED completion for the same turn is stale (activeTurn is cleared) and
	// must not append a second cue.
	again, _ := nm.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "I'm about to read all five terminals."})
	if got := countCue(again.(Model)); got != 1 {
		t.Fatalf("a redelivered completion must not duplicate the cue, got %d", got)
	}
}

// The cue must sit BETWEEN the turn it annotates and whatever drainPending starts next —
// otherwise it reads as belonging to the following wake.
func TestTurnComplete_CueOrdersBeforeADrainedWake(t *testing.T) {
	m := liveModel(80)
	cell := callessTurn("turn_1", "anything happening?", "Nothing needs you right now.")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true
	// A wake queued mid-turn drains the moment this turn settles.
	m.pendingWake = []domain.QueueEvent{wakeEvent("term_1", "agent exited")}

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "Nothing needs you right now."})
	nm := next.(Model)

	ci := cueIndex(nm)
	if ci < 0 {
		t.Fatal("the callless turn must still get its cue when a wake is pending")
	}
	// The drained wake appended a fresh active turn AFTER the cue.
	if nm.activeTurn == "" {
		t.Fatal("the pending wake should have drained into a new active turn")
	}
	wakeIdx := -1
	for i, c := range nm.transcript {
		if c.Turn != nil && c.Turn.ID == nm.activeTurn {
			wakeIdx = i
		}
	}
	if wakeIdx < 0 {
		t.Fatal("the drained wake turn is missing from the transcript")
	}
	if !(0 < ci && ci < wakeIdx) {
		t.Fatalf("order must be completed turn (0) < cue (%d) < drained wake (%d)", ci, wakeIdx)
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
	cell := callessTurn("turn_1", "do a thing", "Starting on that…")
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

// A mid-stream failure raises PhaseFailed and pumpError writes an error note; the
// completion that follows can still carry plain reply text that seals the turn
// TurnComplete. The cue must NOT land under that error note claiming a clean end.
func TestTurnComplete_PhaseFailedGetsNoCueUnderTheErrorNote(t *testing.T) {
	m := liveModel(80)
	cell := callessTurn("turn_1", "read the terminals", "Model error: upstream returned 500")
	// Every production events.Error writer emits PhaseFailed first, so reproduce BOTH
	// halves: the ordered phase (the authoritative failure signal) and the error note
	// pumpError already put in the transcript. Neither isFailureReply nor
	// terminalTurnState recognises this plain reply, so the turn seals TurnComplete —
	// the exact shape that would otherwise print the cue directly under the error.
	cell.Phase = domain.PhaseFailed
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true
	m.addNote(NoteError, "Model error: upstream returned 500")

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "Model error: upstream returned 500"})
	nm := next.(Model)

	if nm.transcript[0].Turn.State != TurnComplete {
		t.Fatalf("precondition: this shape must seal TurnComplete (got %v) — otherwise the "+
			"phase guard isn't what's doing the work", nm.transcript[0].Turn.State)
	}
	if got := countCue(nm); got != 0 {
		t.Fatalf("a phase-failed turn must not carry the end-of-turn cue, got %d", got)
	}
	if notes := noteTexts(nm); len(notes) != 1 {
		t.Fatalf("expected only the pre-existing error note, got %q", notes)
	}
}

// The REAL surfaced-failure payload: App.Send passes Session.Send through, whose only
// non-nil error is ErrTurnInProgress — returned with an EMPTY reply. isFailureReply("")
// is true, so the "Turn could not start" branch is skipped and terminalTurnState seals
// TurnFailed. Either way there must be no cue.
func TestTurnComplete_SurfacedFailureGetsNoCue(t *testing.T) {
	m := liveModel(80)
	cell := callessTurn("turn_1", "do a thing", "partial output")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "", Failed: true})
	nm := next.(Model)

	if nm.transcript[0].Turn.State != TurnFailed {
		t.Errorf("an empty-reply failure must seal TurnFailed, got %v", nm.transcript[0].Turn.State)
	}
	if got := countCue(nm); got != 0 {
		t.Fatalf("a surfaced failure must not carry the end-of-turn cue, got %d", got)
	}
}

// Defensive coverage of the handler's other failure branch (Failed with a NON-sentinel
// reply). No production caller produces this payload today — Send's only error carries
// an empty reply — but the branch exists, and if it ever becomes reachable it must add
// its note and still refuse the cue.
func TestTurnComplete_FailedWithNonSentinelReplyGetsNoteNotCue(t *testing.T) {
	m := liveModel(80)
	cell := callessTurn("turn_1", "do a thing", "partial output")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	m.inFlight = true

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_1", Reply: "partial output", Failed: true})
	nm := next.(Model)

	if got := countCue(nm); got != 0 {
		t.Fatalf("a surfaced failure must not carry the end-of-turn cue, got %d", got)
	}
	notes := noteTexts(nm)
	if len(notes) != 1 || !strings.Contains(notes[0], "Turn could not start") {
		t.Fatalf("expected only the start-failure note, got %q", notes)
	}
}

// A completion tagged with a stale run id is dropped at the ordering barrier — it must
// not annotate the turn that is genuinely active.
func TestTurnComplete_StaleCompletionAppendsNoCue(t *testing.T) {
	m := liveModel(80)
	live := callessTurn("turn_2", "the real one", "still working on it")
	m.transcript = []TranscriptCell{{Turn: live}}
	m.activeTurn = "turn_2"
	m.inFlight = true
	cellsBefore := len(m.transcript)

	next, _ := m.onTurnComplete(TurnCompleteMsg{RunID: "turn_OLD", Reply: "stale"})
	nm := next.(Model)

	if got := countCue(nm); got != 0 {
		t.Fatalf("a stale completion must not append a cue, got %d", got)
	}
	if len(nm.transcript) != cellsBefore {
		t.Fatalf("a stale completion must not grow the transcript (%d → %d)", cellsBefore, len(nm.transcript))
	}
	if nm.transcript[0].Turn.State != TurnActive {
		t.Error("a stale completion must leave the live turn active")
	}
}

// An autonomous wake gets NO cue, deliberately: a watcher wake that reports without
// calling a tool is doing exactly its job, so "no action taken" would read as neglect on
// the majority of wakes — and the human isn't holding a turn on a wake.
func TestWakeComplete_CalllessWakeGetsNoCue(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "wake_1", State: TurnActive, Phase: domain.PhaseGenerating}
	cell.appendProse("term_1 finished cleanly; nothing needs you.")
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "wake_1"
	m.inFlight = true
	m.activeWake = []domain.QueueEvent{wakeEvent("term_1", "agent exited")}

	next, _ := m.onWakeComplete(WakeCompleteMsg{RunID: "wake_1", Reply: "term_1 finished cleanly; nothing needs you."})
	nm := next.(Model)

	if got := countCue(nm); got != 0 {
		t.Fatalf("a wake must not carry the end-of-turn cue, got %d", got)
	}
	// The wake still settles normally.
	if nm.inFlight || nm.activeTurn != "" {
		t.Error("the wake must settle (inFlight/activeTurn must clear)")
	}
	if nm.transcript[0].Turn.State != TurnComplete {
		t.Errorf("wake turn state = %v, want TurnComplete", nm.transcript[0].Turn.State)
	}
}

// The cue renders as ONE plain row: toned spine + info glyph + a DIM body. Dim is what
// keeps a per-turn marker from competing with the prose above it — an ordinary note
// keeps full body weight.
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

// Under the ASCII glyph set (DAINTREE_ASCII / non-UTF locale) the cue must still be a
// legible, non-blank single row — a whitespace-only line would be silently dropped by
// tea.Println and the marker would vanish exactly where it matters most.
func TestRenderNoteCell_MutedCueAsciiSafe(t *testing.T) {
	th := asciiTheme(t)
	out := stripAnsi(renderNoteCell(th, &NoteCell{Level: NoteInfo, Text: noActionCueText, Muted: true}, 120))

	if strings.TrimSpace(out) == "" {
		t.Fatal("the ASCII cue must never render blank")
	}
	if !strings.Contains(out, noActionCueText) {
		t.Fatalf("ASCII cue text missing: %q", out)
	}
	if !strings.HasPrefix(out, th.Glyphs.Continuation) {
		t.Errorf("the ASCII cue must keep the note spine %q, got %q", th.Glyphs.Continuation, out)
	}
	if !strings.Contains(out, th.Glyphs.Bullet) {
		t.Errorf("the ASCII cue must keep the info bullet %q, got %q", th.Glyphs.Bullet, out)
	}
}

// Every standalone note now funnels through appendNote, which stamps the shared identity
// fields. Pin that contract: the constructors differ ONLY in the fields they set, and no
// two notes ever share an id or a backing pointer.
func TestAppendNote_ConstructorContract(t *testing.T) {
	m := testModel(80)
	m.addNote(NoteInfo, "plain")
	m.addSeverityNote(NoteWarn, domain.SeverityAttention, "routed")
	m.addMutedNote(NoteInfo, "quiet")

	var notes []*NoteCell
	for _, c := range m.transcript {
		if c.Note != nil {
			notes = append(notes, c.Note)
		}
	}
	if len(notes) != 3 {
		t.Fatalf("expected 3 notes, got %d", len(notes))
	}

	seen := map[string]bool{}
	for i, n := range notes {
		if !strings.HasPrefix(n.ID, "note_") {
			t.Errorf("note %d id = %q, want a note_ prefix", i, n.ID)
		}
		if seen[n.ID] {
			t.Errorf("note %d reuses id %q — ids must be unique", i, n.ID)
		}
		seen[n.ID] = true
		if n.Ts == 0 {
			t.Errorf("note %d has a zero timestamp", i)
		}
		// Distinct backing pointers: appendNote takes the cell BY VALUE, so each call
		// must allocate its own — a shared pointer would let one note overwrite another.
		for j := 0; j < i; j++ {
			if notes[j] == n {
				t.Errorf("notes %d and %d share a pointer", j, i)
			}
		}
	}

	// addNote: no severity, not muted.
	if notes[0].Severity != "" || notes[0].Muted || notes[0].Level != NoteInfo || notes[0].Text != "plain" {
		t.Errorf("addNote produced %+v", *notes[0])
	}
	// addSeverityNote: severity preserved (the renderer draws THAT glyph), not muted.
	if notes[1].Severity != domain.SeverityAttention || notes[1].Muted || notes[1].Level != NoteWarn || notes[1].Text != "routed" {
		t.Errorf("addSeverityNote produced %+v", *notes[1])
	}
	// addMutedNote: muted, no severity.
	if !notes[2].Muted || notes[2].Severity != "" || notes[2].Level != NoteInfo || notes[2].Text != "quiet" {
		t.Errorf("addMutedNote produced %+v", *notes[2])
	}
}
