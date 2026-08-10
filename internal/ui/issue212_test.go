package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/ui/theme"
)

// --- Gap 1: manual redraw key (Ctrl+L) ---

// TestCtrlLRedrawIsNonDestructive verifies Ctrl+L preserves the transcript MODEL and the
// active view/scroll state. Ctrl+L is now the full nuclear redraw (host wipe + re-commit,
// same as a resize-redraw — see resize_test.go TestCtrlL_PerformsNuclearRedraw): the host
// pixels are rebuilt, but everything the app owns re-commits from this untouched model, so
// "non-destructive" here means no model/view state is lost, unlike /clear.
func TestCtrlLRedrawIsNonDestructive(t *testing.T) {
	m := testModel(80)
	m.transcript = []TranscriptCell{{Note: &NoteCell{ID: "n1", Level: NoteInfo, Text: "keep me"}}}
	m.view = viewOperations
	m.opsScroll = 3
	before := len(m.transcript)

	next, cmd := m.onKey(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'l'})
	if cmd == nil {
		t.Fatal("Ctrl+L must return a repaint command")
	}
	nm := next.(Model)
	if len(nm.transcript) != before {
		t.Errorf("Ctrl+L must not touch scrollback: transcript was %d, now %d", before, len(nm.transcript))
	}
	if nm.view != viewOperations || nm.opsScroll != 3 {
		t.Errorf("Ctrl+L must not change view/scroll: view=%v opsScroll=%d", nm.view, nm.opsScroll)
	}
}

// --- Gap 2: scrollable ops/help decks ---

// TestClampWindowScrolls exercises the windowing helper that replaced clampHeight: a long
// deck scrolls (offset-driven) instead of dead-ending, and the result is ALWAYS height-safe
// (<= n lines) with scroll cues replacing — never adding — the boundary rows (#1613).
func TestClampWindowScrolls(t *testing.T) {
	th := theme.Resolve()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line"+itoa(i))
	}
	full := strings.Join(lines, "\n")

	// Fits whole when the budget covers it: unchanged, no cues.
	if got := clampWindow(full, 0, 20, th); got != full {
		t.Errorf("clampWindow must return content unchanged when it fits the budget")
	}

	// Top of a long deck: exactly n lines, a down-cue, no up-cue, first line present.
	top := clampWindow(full, 0, 6, th)
	if got := lineCount(top); got != 6 {
		t.Fatalf("window must be exactly 6 lines, got %d", got)
	}
	topPlain := ansi.Strip(top)
	if strings.Contains(topPlain, "↑ more") {
		t.Error("top window must not show an up-more cue")
	}
	if !strings.Contains(topPlain, "↓ more") {
		t.Error("top window must show a down-more cue")
	}
	if !strings.Contains(topPlain, "line0") {
		t.Error("top window must include the first line")
	}

	// Scrolled into the middle: both cues, still exactly n lines.
	mid := clampWindow(full, 5, 6, th)
	if got := lineCount(mid); got != 6 {
		t.Fatalf("mid window must be exactly 6 lines, got %d", got)
	}
	if midPlain := ansi.Strip(mid); !strings.Contains(midPlain, "↑ more") || !strings.Contains(midPlain, "↓ more") {
		t.Errorf("mid window must show both scroll cues: %q", midPlain)
	}

	// Bottom (offset over-clamped): up-cue only, last line reachable.
	bottom := ansi.Strip(clampWindow(full, 999, 6, th))
	if strings.Contains(bottom, "↓ more") {
		t.Error("bottom window must not show a down-more cue")
	}
	if !strings.Contains(bottom, "line19") {
		t.Error("bottom window must include the last line")
	}
}

// TestClampWindowSmallBudget covers the rows<=4 floor: at n<3 the scroll cues would consume
// every row, so they are suppressed and the raw window shows — keeping all content reachable
// and the height bounded to exactly n.
func TestClampWindowSmallBudget(t *testing.T) {
	th := theme.Resolve()
	full := strings.Join([]string{"a", "b", "c", "d", "e"}, "\n")

	for _, n := range []int{1, 2} {
		// Every line must be reachable at SOME offset (no row hidden behind a cue).
		seen := map[string]bool{}
		for off := 0; off <= 5; off++ {
			w := clampWindow(full, off, n, th)
			if got := lineCount(w); got > n {
				t.Fatalf("n=%d off=%d: window is %d lines, exceeds budget %d", n, off, got, n)
			}
			if strings.Contains(ansi.Strip(w), "more") {
				t.Errorf("n=%d: no scroll cue should show at the floor, got %q", n, ansi.Strip(w))
			}
			for _, ln := range strings.Split(ansi.Strip(w), "\n") {
				seen[ln] = true
			}
		}
		for _, ln := range []string{"a", "b", "c", "d", "e"} {
			if !seen[ln] {
				t.Errorf("n=%d: line %q is unreachable across all offsets", n, ln)
			}
		}
	}
}

// TestCommandCompleteResetsDeckScroll covers the slash-command deck entry (/help, /watchers,
// …): like the ?/^O key paths, a freshly-opened deck must start at the top.
func TestCommandCompleteResetsDeckScroll(t *testing.T) {
	m := testModel(60)
	m.helpScroll = 4
	m.opsScroll = 3

	next, _ := m.onCommandComplete(CommandCompleteMsg{Title: "Help", SwitchPanel: PanelHelp})
	if nm := next.(Model); nm.helpScroll != 0 {
		t.Errorf("/help must reset helpScroll to 0, got %d", nm.helpScroll)
	}

	next2, _ := m.onCommandComplete(CommandCompleteMsg{Title: "Watchers", SwitchPanel: PanelWatchers})
	if nm := next2.(Model); nm.opsScroll != 0 {
		t.Errorf("/watchers must reset opsScroll to 0, got %d", nm.opsScroll)
	}
}

// TestHelpDeckScrolls drives the full key path: a tall help deck in a short terminal scrolls
// via the arrow keys, never overflows the terminal height, and End jumps to the bottom.
func TestHelpDeckScrolls(t *testing.T) {
	m := testModel(60)
	m.view = viewHelp
	m.rows = 8 // visible window = rows-2 = 6; the help text is much taller → must scroll

	v := m.View().Content
	if !strings.Contains(ansi.Strip(v), "↓ more") {
		t.Fatalf("a tall help deck in a short terminal must show a scroll cue:\n%s", ansi.Strip(v))
	}
	assertNoHeightOverflow(t, "help-scroll-top", v, m.rows)

	// Down advances the offset (clamped to content) and the View stays height-safe at the
	// intermediate offset (both cues showing — the worst case for the #1613 budget).
	next, _ := m.onKey(tea.KeyPressMsg{Code: tea.KeyDown})
	nm := next.(Model)
	if nm.helpScroll != 1 {
		t.Fatalf("Down must advance helpScroll to 1, got %d", nm.helpScroll)
	}
	assertNoHeightOverflow(t, "help-scroll-mid", nm.View().Content, nm.rows)

	// End jumps to the bottom: the down-cue is gone, the up-cue shows, height stays safe.
	next2, _ := nm.onKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	nm2 := next2.(Model)
	bottomV := nm2.View().Content
	bottom := ansi.Strip(bottomV)
	if strings.Contains(bottom, "↓ more") {
		t.Errorf("at the bottom there must be no down-more cue:\n%s", bottom)
	}
	if !strings.Contains(bottom, "↑ more") {
		t.Errorf("at the bottom the up-more cue must show:\n%s", bottom)
	}
	assertNoHeightOverflow(t, "help-scroll-bottom", bottomV, m.rows)
}

// TestEscResetsDeckScroll verifies leaving a deck (Esc → home) clears its scroll offset.
func TestEscResetsDeckScroll(t *testing.T) {
	m := testModel(60)
	m.view = viewHelp
	m.rows = 8
	m.helpScroll = 4

	next, _ := m.onKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	nm := next.(Model)
	if nm.view != viewHome {
		t.Fatalf("Esc must return home, got view %v", nm.view)
	}
	if nm.helpScroll != 0 {
		t.Errorf("leaving the deck must reset helpScroll to 0, got %d", nm.helpScroll)
	}
}
