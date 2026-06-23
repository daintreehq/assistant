package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// --- Gap 1: manual redraw key (Ctrl+L) ---

// TestCtrlLRedrawIsNonDestructive verifies Ctrl+L issues a repaint command without mutating
// any state — crucially NOT wiping the transcript (native scrollback) the way /clear or a
// resize-redraw does. It is a pure footer repaint, available in every view.
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

	// Down advances the offset (clamped to content).
	next, _ := m.onKey(tea.KeyPressMsg{Code: tea.KeyDown})
	nm := next.(Model)
	if nm.helpScroll != 1 {
		t.Fatalf("Down must advance helpScroll to 1, got %d", nm.helpScroll)
	}

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

// --- Gap 5: streaming live preview strips inline markdown ---

// TestStripLivePreviewMarkdown proves the streaming live preview cleans common inline markers
// (so they don't show raw until the paragraph seals) while staying conservative — a lone "*"
// or an intra-word "_" must NOT be mangled.
func TestStripLivePreviewMarkdown(t *testing.T) {
	stripped := []struct {
		in          string
		mustNotHave string
		mustHave    string
	}{
		{"**bold** text", "**", "bold"},
		{"a `code` snippet", "`", "code"},
		{"# Heading", "#", "Heading"},
		{"## Sub heading", "#", "Sub heading"},
		{"_emph_ word", "_", "emph"},
		{"__strong__ word", "__", "strong"},
		{"an *italic* run", "*", "italic"},
	}
	for _, c := range stripped {
		got := stripLivePreviewMarkdown(c.in)
		if c.mustNotHave != "" && strings.Contains(got, c.mustNotHave) {
			t.Errorf("strip(%q) = %q still contains %q", c.in, got, c.mustNotHave)
		}
		if !strings.Contains(got, c.mustHave) {
			t.Errorf("strip(%q) = %q lost %q", c.in, got, c.mustHave)
		}
	}

	// Conservative: non-markdown text with stray markers must pass through untouched.
	for _, s := range []string{"3*4", "foo_bar", "a * b", "2 * 3 = 6", "snake_case_name"} {
		if got := stripLivePreviewMarkdown(s); got != s {
			t.Errorf("strip(%q) must be unchanged, got %q", s, got)
		}
	}
}
