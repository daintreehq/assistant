package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// testModel builds a root Model WITHOUT a real App (the View/render path never
// touches the app or controller). We force the unicode dark theme for stable golden
// rendering, and seed geometry.
func testModel(columns int) Model {
	th := theme.Resolve()
	cmp := composer.New(th)
	return Model{
		theme:               th,
		md:                  markdown.New(th),
		columns:             columns,
		rows:                40,
		view:                viewHome,
		composer:            cmp,
		summarizedTerminals: map[string]struct{}{},
		masthead:            mastheadParams{Version: "9.9.9", ProjectName: "demo", Tier: domain.TierSystem},
	}
}

// goldenWidths is the matrix of terminal widths the contract is proven at.
var goldenWidths = []int{40, 55, 72, 100, 120}

// usableWidth is the cell budget a live line must never exceed: columns - gutter.
// The content is rendered at chromeWidth (= columns - gutter - LeftPad) then given a
// LeftPad inset, so a full composed line is at most chromeWidth + LeftPad = columns
// - gutter. The right gutter (>=1) reserves the autowrap column, so no line ever
// lands there.
func (m Model) usableWidth() int { return m.columns - m.gutter() }

// assertNoOverflow fails if any line of s exceeds the usable width (cell-measured).
// This proves the inline contract: a live line never lands in the autowrap column.
func assertNoOverflow(t *testing.T, label string, s string, usable int) {
	t.Helper()
	for i, line := range strings.Split(s, "\n") {
		if w := ansi.StringWidth(line); w > usable {
			t.Errorf("%s: line %d width %d exceeds usable %d: %q", label, i, w, usable, ansi.Strip(line))
		}
	}
}

// assertNoHeightOverflow fails if the view has more lines than the terminal rows — the
// core minimum-size invariant: a footer taller than the terminal scrolls a frozen partial
// copy into native scrollback and corrupts it (bubbletea#1613).
func assertNoHeightOverflow(t *testing.T, label, s string, rows int) {
	t.Helper()
	if got := lineCount(s); got > rows {
		t.Fatalf("%s: View() is %d lines, exceeds terminal rows %d:\n%s", label, got, rows, ansi.Strip(s))
	}
}

// assertNoForbiddenEscapes fails if the view enables the alternate screen or mouse
// tracking — both are forbidden on the inline cockpit (host owns scroll/selection).
func assertNoForbiddenEscapes(t *testing.T, label, s string) {
	t.Helper()
	forbidden := map[string]string{
		"\x1b[?1049h": "alt-screen enter",
		"\x1b[?1000h": "mouse X10 tracking",
		"\x1b[?1002h": "mouse button tracking",
		"\x1b[?1003h": "mouse any-motion tracking",
		"\x1b[?1006h": "SGR mouse mode",
	}
	for esc, name := range forbidden {
		if strings.Contains(s, esc) {
			t.Errorf("%s: View() contains forbidden %s escape", label, name)
		}
	}
}

// TestViewWidths_Idle renders the idle footer at every width and asserts no overflow
// + no forbidden escapes.
func TestViewWidths_Idle(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		v := m.View()
		assertNoOverflow(t, "idle@"+itoa(w), v.Content, m.usableWidth())
		assertNoForbiddenEscapes(t, "idle@"+itoa(w), v.Content)
	}
}

// TestViewWidths_Streaming drives an active turn with prose + a tool and asserts the
// live footer stays within the usable width.
func TestViewWidths_Streaming(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		m.transcript = []TranscriptCell{{Turn: &TurnCell{
			ID:       "turn_1",
			UserText: "Please inspect the deeply nested UI rendering path and report findings",
			State:    TurnActive,
			Phase:    domain.PhaseGenerating,
			Steps: []TurnStep{
				{Kind: StepProse, Text: "I'll inspect the relevant UI path and summarize the long winded result here", Streaming: true},
				{Kind: StepTool, Activity: &Activity{ID: "t1", Name: "fs.read", State: ActActive, Detail: "internal/ui/view.go", StartedAt: 1}},
			},
		}}}
		m.activeTurn = "turn_1"
		m.inFlight = true
		v := m.View()
		assertNoOverflow(t, "streaming@"+itoa(w), v.Content, m.usableWidth())
		assertNoForbiddenEscapes(t, "streaming@"+itoa(w), v.Content)
	}
}

// TestViewWidths_Approval shows the approval sheet and asserts width + escapes.
func TestViewWidths_Approval(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		m.pending = &pendingConfirm{req: tools.ConfirmRequest{
			ToolName:    "git.push",
			Risk:        domain.RiskGit,
			Summary:     "push the current branch to origin so the changes are shared",
			Consequence: "pushes the local branch to the shared origin remote, visible to everyone",
		}}
		v := m.View()
		assertNoOverflow(t, "approval@"+itoa(w), v.Content, m.usableWidth())
		assertNoForbiddenEscapes(t, "approval@"+itoa(w), v.Content)
	}
}

// TestViewWidths_Degraded shows the degraded MCP status segment.
func TestViewWidths_Degraded(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		m.degraded = true
		v := m.View()
		if !strings.Contains(ansi.Strip(v.Content), "Daintree MCP") {
			t.Errorf("degraded@%d: status line missing the MCP-unavailable segment: %q", w, ansi.Strip(v.Content))
		}
		assertNoOverflow(t, "degraded@"+itoa(w), v.Content, m.usableWidth())
		assertNoForbiddenEscapes(t, "degraded@"+itoa(w), v.Content)
	}
}

// TestViewWidths_Operations renders the ops deck and asserts width + escapes.
func TestViewWidths_Operations(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		m.view = viewOperations
		m.dashboard = Dashboard{
			Inbox: []domain.QueueEvent{{
				Title: "Agent term_8 needs input on a long-running migration step", Severity: domain.SeverityAttention, Count: 2,
				Summary: "the migration paused waiting for a destructive confirmation prompt", EpistemicKind: domain.EpistemicObserved,
			}},
			Timers: []domain.TimerRecord{{Title: "nightly digest", FireAt: 1_000_000_000_000}},
			Agents: []AgentRow{{ID: "term_8", Title: "migrate schema", Badge: "NEEDS INPUT", Priority: 0, AgentState: "running", Preview: "waiting…"}},
		}
		v := m.View()
		assertNoOverflow(t, "operations@"+itoa(w), v.Content, m.usableWidth())
		assertNoForbiddenEscapes(t, "operations@"+itoa(w), v.Content)
	}
}

// tallApprovalConfirm builds a representative multi-row approval sheet (the tallest thing
// the fixed bottom band can hold) for the height-floor tests.
func tallApprovalConfirm() *pendingConfirm {
	return &pendingConfirm{req: tools.ConfirmRequest{
		ToolName:    "git.push",
		Risk:        domain.RiskGit,
		Summary:     "push the current branch to origin so the changes are shared",
		Consequence: "pushes the local branch to the shared origin remote, visible to everyone",
	}}
}

// TestViewHeight_TinyRows drives a WindowSizeMsg shrinking the pane below the row floor
// with an approval sheet open (exactly the issue's repro) and asserts the footer collapses
// to a single line, so View() can never outgrow the terminal and corrupt scrollback
// (bubbletea#1613).
func TestViewHeight_TinyRows(t *testing.T) {
	m := testModel(40)
	m.pending = tallApprovalConfirm()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 3})
	m = next.(Model)
	v := m.View()
	if got := lineCount(v.Content); got != 1 {
		t.Fatalf("tiny-rows footer must be exactly 1 line, got %d: %q", got, ansi.Strip(v.Content))
	}
	if got := lineCount(v.Content); got > m.rows {
		t.Fatalf("View() (%d lines) exceeds terminal rows (%d)", got, m.rows)
	}
	if !strings.Contains(ansi.Strip(v.Content), "terminal too small") {
		t.Errorf("tiny-rows footer missing the too-small notice: %q", ansi.Strip(v.Content))
	}
	assertNoOverflow(t, "tiny-rows", v.Content, m.usableWidth())
	assertNoForbiddenEscapes(t, "tiny-rows", v.Content)
}

// TestViewHeight_BottomBandOverflow proves the content-aware floor: at rows=5 (above the
// minCockpitRows geometry floor) a multi-row approval sheet alone overruns the pane, so the
// footer must still collapse to the one-liner rather than letting the band push View() taller
// than the terminal.
func TestViewHeight_BottomBandOverflow(t *testing.T) {
	m := testModel(40)
	m.rows = 5
	m.pending = tallApprovalConfirm()
	v := m.View()
	if got := lineCount(v.Content); got != 1 {
		t.Fatalf("overflow footer must be exactly 1 line, got %d: %q", got, ansi.Strip(v.Content))
	}
	if !strings.Contains(ansi.Strip(v.Content), "terminal too small") {
		t.Errorf("overflow footer missing the too-small notice: %q", ansi.Strip(v.Content))
	}
	assertNoOverflow(t, "overflow", v.Content, m.usableWidth())
	assertNoForbiddenEscapes(t, "overflow", v.Content)
}

// TestViewHeight_ResizeRecovery confirms the floor is not sticky: once the pane grows back
// the real footer returns (the too-small notice disappears and the full approval sheet
// renders again).
func TestViewHeight_ResizeRecovery(t *testing.T) {
	m := testModel(40)
	m.pending = tallApprovalConfirm()
	m.rows = 3
	if got := lineCount(m.View().Content); got != 1 {
		t.Fatalf("expected collapsed footer at rows=3, got %d lines", got)
	}
	m.rows = 40
	v := m.View()
	if got := lineCount(v.Content); got <= 1 {
		t.Fatalf("footer should restore to multiple lines at rows=40, got %d", got)
	}
	if strings.Contains(ansi.Strip(v.Content), "terminal too small") {
		t.Errorf("restored footer must not show the too-small notice: %q", ansi.Strip(v.Content))
	}
	assertNoHeightOverflow(t, "recovery", v.Content, m.rows)
	assertNoOverflow(t, "recovery", v.Content, m.usableWidth())
	assertNoForbiddenEscapes(t, "recovery", v.Content)
}

// TestViewHeight_StreamingFloor exercises the live-aware (+2) branch of the content-aware
// floor: a streaming turn forces at least one live row plus a blank separator above the
// fixed band, so the band fits only when rows >= lineCount(band)+2. At exactly that height
// it must render; one row shorter it must collapse — proving the +2 accounting is tight
// (neither over-eager nor leaving a one-row overflow that re-triggers bubbletea#1613).
func TestViewHeight_StreamingFloor(t *testing.T) {
	m := streamingModel(40)
	b := lineCount(m.bottomBand(m.contentW()))
	// Precondition: band+1 must clear the geometry floor so the COLLAPSE below is driven by
	// the content-aware floor (the +2 path), not by m.rows < minCockpitRows.
	if b+1 < minCockpitRows {
		t.Fatalf("test precondition: band+1 (%d) must exceed the geometry floor %d", b+1, minCockpitRows)
	}
	// Exactly enough: 1 forced live row + 1 blank separator + band == rows. Must NOT collapse.
	m.rows = b + 2
	v := m.View()
	if strings.Contains(ansi.Strip(v.Content), "terminal too small") {
		t.Fatalf("streaming footer collapsed when it should fit at rows=%d (band=%d)", m.rows, b)
	}
	assertNoHeightOverflow(t, "streaming-fit", v.Content, m.rows)
	assertNoForbiddenEscapes(t, "streaming-fit", v.Content)
	// One row shorter: the forced live row + separator + band overflows by one. Must collapse.
	m.rows = b + 1
	v = m.View()
	if got := lineCount(v.Content); got != 1 {
		t.Fatalf("streaming footer must collapse to 1 line at rows=%d (band=%d), got %d", m.rows, b, got)
	}
	if !strings.Contains(ansi.Strip(v.Content), "terminal too small") {
		t.Errorf("collapsed streaming footer missing the too-small notice")
	}
	assertNoHeightOverflow(t, "streaming-collapse", v.Content, m.rows)
}

// TestViewHeight_NonHomeTinyRows verifies the geometry floor is cockpit-wide: the ops and
// help decks (which emit a body line + an "Esc back" line) also collapse to one line below
// minCockpitRows, never overflowing the terminal.
func TestViewHeight_NonHomeTinyRows(t *testing.T) {
	for _, view := range []viewMode{viewOperations, viewHelp} {
		for _, rows := range []int{1, 2, 3} {
			m := testModel(40)
			m.view = view
			m.rows = rows
			v := m.View()
			label := "view" + itoa(int(view)) + "@rows" + itoa(rows)
			if got := lineCount(v.Content); got != 1 {
				t.Fatalf("%s: expected collapsed 1-line footer, got %d: %q", label, got, ansi.Strip(v.Content))
			}
			if !strings.Contains(ansi.Strip(v.Content), "terminal too small") {
				t.Errorf("%s: missing too-small notice: %q", label, ansi.Strip(v.Content))
			}
			assertNoHeightOverflow(t, label, v.Content, rows)
			assertNoForbiddenEscapes(t, label, v.Content)
		}
	}
}

// streamingModel builds a model with one active in-flight turn so liveCellsView returns
// non-empty content (exercising the live-aware footer path).
func streamingModel(columns int) Model {
	m := testModel(columns)
	m.transcript = []TranscriptCell{{Turn: &TurnCell{
		ID:       "turn_1",
		UserText: "inspect the deeply nested UI rendering path and report findings",
		State:    TurnActive,
		Phase:    domain.PhaseGenerating,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "Working through the relevant rendering path and summarizing the result here", Streaming: true},
		},
	}}}
	m.activeTurn = "turn_1"
	m.inFlight = true
	return m
}

// TestViewOptions_NoAltScreenNoMouse asserts the tea.View carries the inline
// program options: AltScreen off, MouseMode none, bracketed paste ON.
func TestViewOptions_NoAltScreenNoMouse(t *testing.T) {
	m := testModel(80)
	v := m.View()
	if v.AltScreen {
		t.Error("View.AltScreen must be false (normal screen buffer)")
	}
	if v.MouseMode != tea.MouseModeNone {
		t.Error("View.MouseMode must be MouseModeNone (no mouse capture)")
	}
	if v.DisableBracketedPasteMode {
		t.Error("bracketed paste must be ENABLED (DisableBracketedPasteMode=false)")
	}
}
