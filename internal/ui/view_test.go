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
		theme:    th,
		md:       markdown.New(th),
		columns:  columns,
		rows:     40,
		view:     viewHome,
		composer: cmp,
		masthead: mastheadParams{Version: "9.9.9", ProjectName: "demo", Tier: domain.TierSystem},
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

// TestViewWidths_Degraded shows the DEGRADED status segment.
func TestViewWidths_Degraded(t *testing.T) {
	for _, w := range goldenWidths {
		m := testModel(w)
		m.degraded = true
		m.hasUsage = true
		m.contextPct = 42
		v := m.View()
		if !strings.Contains(ansi.Strip(v.Content), "DEGRADED") {
			t.Errorf("degraded@%d: status line missing DEGRADED: %q", w, ansi.Strip(v.Content))
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
