package composer

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// composer_ported_test.go exercises the remaining editor-key
// contract (forward-delete, Home/^A/^E, Alt+D, ^U, never-insert-chords) and the
// composer presentation (single › glyph, queued-count surface/omit, restore
// then edit+submit, the ^O hint promotion). The existing composer_test.go covers
// kill-ring/word-motion/history/slash/paste/wrap; this fills the gaps.

// --- editor key contract ---

func TestForwardDeleteAtCursor(t *testing.T) {
	m := newModel()
	typeRunes(&m, "abc")
	press(&m, tea.KeyHome, 0)   // cursor to start
	press(&m, tea.KeyDelete, 0) // delete the 'a' under the cursor
	if m.Value() != "bc" {
		t.Fatalf("Delete: got %q want %q", m.Value(), "bc")
	}
}

func TestCtrlDDeletesAtCursor(t *testing.T) {
	m := newModel()
	typeRunes(&m, "abc")
	pressChord(&m, 'a', tea.ModCtrl) // ^A → start
	pressChord(&m, 'd', tea.ModCtrl) // ^D → delete forward
	if m.Value() != "bc" {
		t.Fatalf("Ctrl-D: got %q want %q", m.Value(), "bc")
	}
}

func TestHomeCtrlAAndCtrlEJumps(t *testing.T) {
	m := newModel()
	typeRunes(&m, "abc")
	pressChord(&m, 'a', tea.ModCtrl) // start
	typeRunes(&m, "X")               // → Xabc
	pressChord(&m, 'e', tea.ModCtrl) // end
	typeRunes(&m, "Z")               // → XabcZ
	if m.Value() != "XabcZ" {
		t.Fatalf("^A/^E jumps: got %q want %q", m.Value(), "XabcZ")
	}
}

func TestAltDDeletesNextWord(t *testing.T) {
	m := newModel()
	typeRunes(&m, "foo bar")
	pressChord(&m, 'a', tea.ModCtrl) // cursor to start
	pressChord(&m, 'd', tea.ModAlt)  // kill "foo" forward
	if m.Value() != " bar" {
		t.Fatalf("Alt-D: got %q want %q", m.Value(), " bar")
	}
}

func TestCtrlUDeletesWholeLine(t *testing.T) {
	m := newModel()
	typeRunes(&m, "hello world")
	press(&m, tea.KeyLeft, tea.ModCtrl) // cursor into the middle (start of "world")
	pressChord(&m, 'u', tea.ModCtrl)    // whole line, not just to start/end
	if m.Value() != "" {
		t.Fatalf("^U whole line: got %q want empty", m.Value())
	}
}

func TestNeverInsertsAppChords(t *testing.T) {
	m := newModel()
	typeRunes(&m, "ab")
	pressChord(&m, 'c', tea.ModCtrl) // ^C
	pressChord(&m, 'o', tea.ModCtrl) // ^O
	pressChord(&m, 'x', tea.ModCtrl) // ^X
	if m.Value() != "ab" {
		t.Fatalf("app chords must not be typed: got %q want %q", m.Value(), "ab")
	}
}

func TestMultilinePasteNeverSubmits(t *testing.T) {
	m := newModel()
	out := m.Update(tea.PasteMsg{Content: "line one\nline two"})
	if out.Submit != nil {
		t.Fatal("a multi-line paste must never submit")
	}
	if m.Value() != "line one\nline two" {
		t.Fatalf("paste landed wrong: %q", m.Value())
	}
}

// --- presentation ---

func TestView_SinglePromptGlyph(t *testing.T) {
	m := newModel()
	out := m.View(ViewParams{Width: 60, Placeholder: "Ask Daintree"})
	if !strings.Contains(out, "›") {
		t.Errorf("composer must show the › prompt glyph: %q", out)
	}
	if strings.Contains(out, "daintree ❯") {
		t.Errorf("no repeated branding at the prompt: %q", out)
	}
	if !strings.Contains(stripAnsiC(out), "Ask Daintree") {
		t.Errorf("placeholder missing: %q", out)
	}
}

func TestView_QueuedCountSurfacesAndOmits(t *testing.T) {
	m := newModel()
	m.SetBusy(true)
	// Queued > 0 → "N queued" surfaces.
	withQ := stripAnsiC(m.View(ViewParams{Width: 60, QueueDepth: 2}))
	if !strings.Contains(withQ, "2 queued") {
		t.Errorf("queued count must surface while busy: %q", withQ)
	}
	// Queued == 0 → no queued suffix.
	noQ := stripAnsiC(m.View(ViewParams{Width: 60, QueueDepth: 0}))
	if strings.Contains(noQ, "queued") {
		t.Errorf("no queued suffix when nothing waits: %q", noQ)
	}
}

func TestView_EscCancelHintOnlyWhileBusy(t *testing.T) {
	m := newModel()
	cancellable := false
	idle := stripAnsiC(m.View(ViewParams{Width: 60, Cancellable: &cancellable}))
	if strings.Contains(idle, "cancel") {
		t.Errorf("idle composer must not show a cancel hint: %q", idle)
	}
	cancellable = true
	busy := stripAnsiC(m.View(ViewParams{Width: 60, Cancellable: &cancellable}))
	if !strings.Contains(busy, "cancel") {
		t.Errorf("cancellable turn must show the Esc-cancel hint: %q", busy)
	}
}

func TestView_OpsHintPromotedWhenAttentionPending(t *testing.T) {
	m := newModel()
	cancellable := false
	// Attention pending + not cancellable → ^O leads ahead of /commands.
	line := stripAnsiC(m.View(ViewParams{Width: 80, Attention: true, Cancellable: &cancellable}))
	hint := hintLineC(line)
	if !strings.Contains(hint, "inspect ops") || !strings.Contains(hint, "commands") {
		t.Fatalf("hint row missing ops/commands: %q", hint)
	}
	if strings.Index(hint, "inspect ops") > strings.Index(hint, "commands") {
		t.Errorf("^O must lead ahead of commands when attention pending: %q", hint)
	}
	if strings.Count(hint, "inspect ops") != 1 {
		t.Errorf("^O must appear exactly once: %q", hint)
	}
}

func TestView_OpsHintTrailingWhenNoAttention(t *testing.T) {
	m := newModel()
	cancellable := false
	line := stripAnsiC(m.View(ViewParams{Width: 80, Attention: false, Cancellable: &cancellable}))
	hint := hintLineC(line)
	if strings.Index(hint, "commands") > strings.Index(hint, "inspect ops") {
		t.Errorf("default order: commands before the trailing ^O: %q", hint)
	}
	if strings.Count(hint, "inspect ops") != 1 {
		t.Errorf("^O must appear exactly once: %q", hint)
	}
}

func TestView_MCPStatusLivesInHintRowWithoutChangingHeight(t *testing.T) {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	m.SetTheme(th)
	connecting := m.View(ViewParams{Width: 100, MCPStatus: MCPConnecting})
	connected := m.View(ViewParams{Width: 100, MCPStatus: MCPConnected})
	degraded := m.View(ViewParams{Width: 100, MCPStatus: MCPDegraded})

	connectingHint := hintLineC(stripAnsiC(connecting))
	connectedHint := hintLineC(stripAnsiC(connected))
	if !strings.Contains(connectingHint, "MCP") || !strings.Contains(connectedHint, "MCP") {
		t.Fatalf("MCP status missing from hint row: connecting=%q connected=%q", connectingHint, connectedHint)
	}
	if strings.Contains(connectingHint, "Connecting") || strings.Contains(connectedHint, "Connected") {
		t.Fatalf("MCP hint must stay compact: connecting=%q connected=%q", connectingHint, connectedHint)
	}
	if strings.Count(connecting, "\n") != strings.Count(connected, "\n") {
		t.Fatal("MCP color-state swap changed composer height")
	}
	if connecting == connected {
		t.Fatal("connecting and connected MCP states must use distinct styling")
	}
	if degraded == connected || !strings.Contains(hintLineC(stripAnsiC(degraded)), "● MCP") {
		t.Fatal("degraded MCP state must use the compact red-dot status")
	}
}

func TestView_CancelLeadsOverOpsWhenCancellable(t *testing.T) {
	m := newModel()
	cancellable := true
	// Cancel takes precedence: Esc leads even though attention is pending; ^O stays trailing.
	line := stripAnsiC(m.View(ViewParams{Width: 80, Attention: true, Cancellable: &cancellable}))
	hint := hintLineC(line)
	if !strings.Contains(hint, "cancel") {
		t.Fatalf("cancel hint missing: %q", hint)
	}
	if strings.Index(hint, "cancel") > strings.Index(hint, "commands") {
		t.Errorf("cancel must lead ahead of commands: %q", hint)
	}
	if strings.Index(hint, "cancel") > strings.Index(hint, "inspect ops") {
		t.Errorf("cancel must lead ahead of ^O: %q", hint)
	}
	if strings.Count(hint, "inspect ops") != 1 {
		t.Errorf("^O must appear exactly once: %q", hint)
	}
}

func TestRestoreThenEditSubmit(t *testing.T) {
	m := newModel()
	m.Restore("edit me")
	if m.Value() != "edit me" {
		t.Fatalf("Restore did not push the text: %q", m.Value())
	}
	// The cursor parks at the end after an external replacement, so typing appends.
	typeRunes(&m, "!")
	out := press(&m, tea.KeyEnter, 0)
	if out.Submit == nil || out.Submit.Text != "edit me!" {
		t.Fatalf("restored text should be editable + submittable: %+v", out.Submit)
	}
}

// stripAnsiC strips SGR escapes for plain-text assertions in the composer package.
func stripAnsiC(s string) string { return ansi.Strip(s) }

// hintLineC returns the rendered hint line (the one carrying "commands").
func hintLineC(frame string) string {
	for _, l := range strings.Split(frame, "\n") {
		if strings.Contains(l, "commands") {
			return l
		}
	}
	return ""
}
