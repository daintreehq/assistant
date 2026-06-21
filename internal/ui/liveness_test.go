package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// liveness_test.go locks in the interactive liveness contract (_interaction-ux.md):
// explicit-phase status, the synchronous ack, the announced-then-promoted tool
// batch, ordered prose/tool interleaving, visible+promoted queued follow-ups,
// synchronous Cancelling, and the composer never being gated by the splash. These
// drive the real reducers/render helpers, not the types in isolation.

// liveModel builds a Model with a controller whose dispatched cmds are never run in
// these tests (we only assert the synchronous state mutation), so a zero App is fine.
func liveModel(columns int) Model {
	m := testModel(columns)
	m.ctx = context.Background()
	pump := newEventPump()
	m.pump = pump
	m.controller = &controller{pump: pump}
	return m
}

// --- 1. Explicit phase drives the live status (never an emptiness heuristic) ---

func TestLiveStatusLabel_FromPhase(t *testing.T) {
	// The silent-work gaps get an exact label; self-evident phases get none.
	cases := []struct {
		phase domain.RunPhase
		want  string
	}{
		{domain.PhaseAnalyzing, "Analyzing request"},
		{domain.PhaseIntegrating, "Integrating results"},
		{domain.PhaseAwaitingApproval, "Waiting for approval"},
		{domain.PhaseCancelling, "Cancelling"},
		{domain.PhaseGenerating, ""}, // streaming prose carries itself
		{domain.PhaseToolRunning, ""},
		{domain.PhaseReceived, ""},
	}
	for _, c := range cases {
		if got := liveStatusLabel(c.phase); got != c.want {
			t.Errorf("liveStatusLabel(%v) = %q, want %q", c.phase, got, c.want)
		}
	}
}

func TestRunStageLabel_NeverThinking_GenericProcessing(t *testing.T) {
	// The composer cue uses the exact vocabulary and NEVER "Thinking".
	want := map[domain.RunPhase]string{
		domain.PhaseReceived:         "Received",
		domain.PhaseAnalyzing:        "Analyzing request…",
		domain.PhaseIntegrating:      "Integrating results…",
		domain.PhaseAwaitingApproval: "Waiting for approval…",
		domain.PhaseToolRunning:      "Inspecting project…",
		domain.PhaseCancelling:       "Cancelling…",
		domain.PhaseGenerating:       "",
	}
	for p, w := range want {
		if got := runStageLabel(p); got != w {
			t.Errorf("runStageLabel(%v) = %q, want %q", p, got, w)
		}
	}
	// The generic fallback for an unlabeled phase is "Processing…", not "Thinking".
	if got := runStageLabel(domain.PhaseToolQueued); got != "Processing…" {
		t.Errorf("fallback = %q, want Processing…", got)
	}
	for p := domain.RunPhase(0); p <= domain.PhaseCancelled; p++ {
		if strings.Contains(strings.ToLower(runStageLabel(p)), "thinking") {
			t.Fatalf("phase %v produced a forbidden \"Thinking\" label", p)
		}
		if strings.Contains(strings.ToLower(liveStatusLabel(p)), "thinking") {
			t.Fatalf("phase %v produced a forbidden \"Thinking\" liveStatus", p)
		}
	}
}

func TestPhasePumpEvent_DrivesActiveTurn(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive, Phase: domain.PhaseReceived}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"

	m.applyPumpEvent(pumpEvent{kind: pumpPhase, phase: domain.PhaseIntegrating})
	if cell.Phase != domain.PhaseIntegrating {
		t.Fatalf("phase = %v, want Integrating", cell.Phase)
	}
	if cell.PhaseStartedAt == 0 {
		t.Error("PhaseStartedAt not stamped on a phase change (no live elapsed)")
	}
}

// --- 2. Synchronous ack: "◆ DAINTREE · received", yields to first token ---

func TestStartTurn_SynchronousReceivedAck(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("inspect the build")
	nm := next.(Model)
	cell := nm.activeTurnCell()
	if cell == nil {
		t.Fatal("startTurn did not create an active turn")
	}
	if cell.Phase != domain.PhaseReceived {
		t.Fatalf("phase = %v, want Received (synchronous ack)", cell.Phase)
	}
	if !nm.inFlight {
		t.Error("single-flight lock not taken")
	}
	// The marker renders "· received" while in the received phase…
	marker := ansi.Strip(renderMarker(nm.theme, cell.Phase, true))
	if !strings.Contains(marker, "DAINTREE") || !strings.Contains(marker, "received") {
		t.Errorf("ack marker = %q, want DAINTREE · received", marker)
	}
	if strings.Contains(marker, "OK Daintree") {
		t.Error("ack must never read \"OK Daintree\"")
	}
	// …and the ack DISAPPEARS the instant a token arrives (phase advances).
	m2 := nm
	m2.applyPumpEvent(pumpEvent{kind: pumpPhase, phase: domain.PhaseGenerating})
	m2.applyPumpEvent(pumpEvent{kind: pumpTokens, text: "Here is"})
	c2 := m2.activeTurnCell()
	if strings.Contains(ansi.Strip(renderMarker(m2.theme, c2.Phase, true)), "received") {
		t.Error("ack must yield the instant a token arrives")
	}
	if !hasProse(c2) {
		t.Error("real output must not be held back behind the ack")
	}
}

// --- 3. Tool batch announced queued before dispatch, then promoted ---

func TestToolBatch_AllQueuedThenPromoted(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive, Phase: domain.PhaseToolQueued}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"

	m.applyPumpEvent(pumpEvent{kind: pumpBatch, batch: []agent.BatchedToolCall{
		{ID: "a", Name: "fs.read", Args: "{}"},
		{ID: "b", Name: "fs.search", Args: "{}"},
		{ID: "c", Name: "fs.read", Args: "{}"},
	}})
	// All three appear immediately as queued.
	for _, id := range []string{"a", "b", "c"} {
		if act := cell.findActivity(id); act == nil || act.State != ActQueued {
			t.Fatalf("call %s not queued on batch announce", id)
		}
	}
	// Promote one queued→active→done.
	m.applyPumpEvent(pumpEvent{kind: pumpToolState, toolID: "a", toolState: agent.ToolStateActive})
	if cell.findActivity("a").State != ActActive {
		t.Error("queued→active promotion missing")
	}
	m.applyPumpEvent(pumpEvent{kind: pumpToolResult, result: agent.ToolResultEvent{
		ID: "a", Name: "fs.read", Result: domain.ToolResult{Ok: true, Summary: "view.go"}, EndedAt: 5,
	}})
	if a := cell.findActivity("a"); a.State != ActDone || a.Detail != "view.go" {
		t.Errorf("active→done resolution missing: %+v", a)
	}
	// A failure carries the outcome alongside the target.
	m.applyPumpEvent(pumpEvent{kind: pumpToolResult, result: agent.ToolResultEvent{
		ID: "b", Name: "fs.search", Result: domain.Fail("X", "boom"), EndedAt: 6,
	}})
	if b := cell.findActivity("b"); b.State != ActFailed || b.Outcome == "" {
		t.Errorf("failure must set ActFailed + outcome: %+v", b)
	}
}

// --- 3b. In-tool progress substep sets the active activity's live detail ---

func TestToolProgress_SetsActiveActivityDetail(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive, Phase: domain.PhaseToolRunning}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"

	m.applyPumpEvent(pumpEvent{kind: pumpBatch, batch: []agent.BatchedToolCall{
		{ID: "a", Name: "agentTask.spawnForEdits", Args: "{}"},
	}})
	m.applyPumpEvent(pumpEvent{kind: pumpToolState, toolID: "a", toolState: agent.ToolStateActive})

	// An in-tool substep for call "a" must land on that activity's ProgressMsg.
	m.applyPumpEvent(pumpEvent{kind: pumpToolProgress, toolID: "a", msg: "launching terminal"})
	if a := cell.findActivity("a"); a == nil || a.ProgressMsg != "launching terminal" {
		t.Fatalf("progress not applied to active activity: %+v", a)
	}
	// A later progress beat for an unknown call id is a harmless no-op.
	m.applyPumpEvent(pumpEvent{kind: pumpToolProgress, toolID: "zzz", msg: "ignored"})
	if a := cell.findActivity("a"); a.ProgressMsg != "launching terminal" {
		t.Fatalf("unrelated progress must not overwrite: %+v", a)
	}
	// The active row renders the live substep as its detail (§4).
	row := renderActivityRow(m.theme, *cell.findActivity("a"), true, false, 0, domain.NowMS(), 80)
	if !strings.Contains(stripAnsi(row), "launching terminal") {
		t.Fatalf("active row did not show the live substep: %q", stripAnsi(row))
	}
}

// --- 4. Ordered prose/tool interleaving (a new prose step after a batch) ---

func TestOrderedSteps_ProseToolProse(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"

	m.applyPumpEvent(pumpEvent{kind: pumpTokens, text: "I'll inspect."})
	m.applyPumpEvent(pumpEvent{kind: pumpBatch, batch: []agent.BatchedToolCall{{ID: "a", Name: "fs.read"}}})
	m.applyPumpEvent(pumpEvent{kind: pumpTokens, text: "Found it."})

	kinds := []TurnStepKind{}
	for _, s := range cell.Steps {
		kinds = append(kinds, s.Kind)
	}
	want := []TurnStepKind{StepProse, StepTool, StepProse}
	if len(kinds) != 3 || kinds[0] != want[0] || kinds[1] != want[1] || kinds[2] != want[2] {
		t.Fatalf("step order = %v, want [Prose Tool Prose] (not merged)", kinds)
	}
	// The two prose steps stay distinct (post-batch prose is appended, not merged).
	if cell.Steps[0].Text == cell.Steps[2].Text {
		t.Error("post-batch prose merged into the pre-batch step")
	}
}

// --- 6. Queued follow-ups are visible and promoted in place ---

func TestQueuedFollowup_VisibleThenPromotedInPlace(t *testing.T) {
	m := liveModel(80)
	// A turn is in flight.
	next, _ := m.startTurn("first task")
	m = next.(Model)
	before := len(m.transcript)

	// Typing while busy queues a VISIBLE dimmed turn (not just a counter).
	next, _ = m.onSubmit("second task")
	m = next.(Model)
	if len(m.transcript) != before+1 {
		t.Fatalf("queued follow-up did not appear as its own turn cell")
	}
	if len(m.queuedInput) != 1 {
		t.Fatalf("queuedInput depth = %d, want 1", len(m.queuedInput))
	}
	q := m.transcript[len(m.transcript)-1].Turn
	if !q.Queued || q.UserText != "second task" {
		t.Fatalf("queued cell wrong: %+v", q)
	}
	// It renders DIMMED in the live footer.
	if strings.TrimSpace(ansi.Strip(m.liveCellsView(m.contentW()))) == "" {
		t.Error("queued follow-up not visible in the footer")
	}

	// Promotion happens IN PLACE — the same cell id becomes active, no new cell.
	qid := q.ID
	cellCount := len(m.transcript)
	m.activeTurn = "" // simulate the prior turn having sealed
	m.inFlight = false
	cmd := m.promoteQueued(m.queuedInput[0])
	_ = cmd
	if len(m.transcript) != cellCount {
		t.Error("promotion created a second entry (must promote in place)")
	}
	if m.activeTurn != qid {
		t.Errorf("active turn = %q, want the promoted cell %q", m.activeTurn, qid)
	}
	if m.transcript[len(m.transcript)-1].Turn.Queued {
		t.Error("promoted cell still flagged Queued")
	}
}

// --- 7. Esc is synchronous: Cancelling before the abort propagates ---

func TestCancel_SynchronousCancellingPhase(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("long task")
	m = next.(Model)
	next, _ = m.onCancel()
	m = next.(Model)
	cell := m.activeTurnCell()
	if cell == nil || cell.Phase != domain.PhaseCancelling {
		t.Fatalf("Esc must set Cancelling synchronously, got %v", cell)
	}
}

// --- 8. Splash never gates input: the composer is focusable while booting ---

func TestSplash_DoesNotGateComposer(t *testing.T) {
	m := liveModel(80)
	m.booting = true
	if !m.composerFocus() {
		t.Error("composer must be focusable while the splash is up (input not gated)")
	}
	// The footer renders the composer even during boot (splash is just an overlay).
	v := ansi.Strip(m.footer())
	if !strings.Contains(v, "Ask Daintree") {
		t.Errorf("composer placeholder missing during boot: %q", v)
	}
}
