package composer

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/ui/theme"
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

// The buffered-follow-up cue no longer lives INSIDE the composer box. Its home is the
// cockpit's queued card above the composer, which shows the queued TEXT alongside the count
// (ui.renderQueuedInjections) — a bare count under the input said something was waiting
// without showing what. The composer must not restate it: two "queued" rows a line apart
// is the duplication the state-truth pass exists to prevent. QueueDepth stays in ViewParams
// because the Escape hint is still derived from it (TestComposerHints_EscapeMatchesNextPress).
func TestView_QueuedCountDoesNotLiveInTheComposer(t *testing.T) {
	m := newModel()
	m.SetBusy(true)
	for _, depth := range []int{0, 1, 2} {
		frame := stripAnsiC(m.View(ViewParams{Width: 60, QueueDepth: depth}))
		if strings.Contains(frame, "queued") {
			t.Errorf("depth %d: the composer must not restate the queued cue: %q", depth, frame)
		}
	}
	// The grammar itself still lives here, with its own tests (TestQueuedFollowupLabel);
	// the cockpit renders it as the queued card's anchor.
	if got := QueuedFollowupLabel(2, 60); !strings.Contains(got, "2 follow-ups queued") {
		t.Errorf("QueuedFollowupLabel must still own the cue grammar, got %q", got)
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

// The connection light lives on its OWN row below the key hints, not appended to them —
// on a narrow pane it was the first thing truncation ate, and it is the one fact down
// here that is not a keyboard shortcut.
func TestView_MCPStatusLivesOnItsOwnRow(t *testing.T) {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	m.SetTheme(th)
	connecting := m.View(ViewParams{Width: 100, MCPStatus: MCPConnecting})
	connected := m.View(ViewParams{Width: 100, MCPStatus: MCPConnected})
	degraded := m.View(ViewParams{Width: 100, MCPStatus: MCPDegraded})

	// Not on the hint row any more, but present in the frame.
	if strings.Contains(hintLineC(stripAnsiC(connected)), "MCP") {
		t.Errorf("MCP is back on the hint row: %q", hintLineC(stripAnsiC(connected)))
	}
	for name, frame := range map[string]string{"connecting": connecting, "connected": connected, "degraded": degraded} {
		if !strings.Contains(stripAnsiC(frame), "● MCP") {
			t.Errorf("%s: compact MCP status missing from the frame:\n%s", name, stripAnsiC(frame))
		}
		// Compact: the dot plus the acronym, never a spelled-out state.
		if strings.Contains(stripAnsiC(frame), "Connecting") || strings.Contains(stripAnsiC(frame), "Connected") {
			t.Errorf("%s: MCP status is not compact:\n%s", name, stripAnsiC(frame))
		}
	}
	// A colour-state swap must not change the composer's height — the live footer's
	// row count is load-bearing for the scrollback commit arithmetic.
	if strings.Count(connecting, "\n") != strings.Count(connected, "\n") {
		t.Fatal("MCP color-state swap changed composer height")
	}
	if connecting == connected || degraded == connected {
		t.Fatal("the three MCP states must use distinct styling")
	}
}

// The session bill shares the connection row, pushed to the right edge. It belongs
// beside the connection state (both are "what is true right now") and a third row for
// one short figure would spend more of a cramped band than it is worth.
func TestView_CostIsRightAlignedOnTheMCPRow(t *testing.T) {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	m.SetTheme(th)

	const width = 60
	frame := stripAnsiC(m.View(ViewParams{Width: width, MCPStatus: MCPConnected, Cost: "$0.0052"}))
	var row string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "MCP") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no MCP row in:\n%s", frame)
	}
	if !strings.Contains(row, "$0.0052") {
		t.Errorf("the cost is not on the MCP row: %q", row)
	}
	// Right-aligned: the figure ends at the far edge, with the connection light at the
	// left. Trailing-space tolerance of zero — that is what "right-aligned" means.
	if !strings.HasSuffix(strings.TrimRight(row, " "), "$0.0052") {
		t.Errorf("the cost is not at the right edge: %q", row)
	}
	if !strings.HasPrefix(strings.TrimLeft(row, " "), "●") {
		t.Errorf("the connection light lost its left anchor: %q", row)
	}

	// Adding a cost must not add a row — it shares the connection line.
	without := m.View(ViewParams{Width: width, MCPStatus: MCPConnected})
	with := m.View(ViewParams{Width: width, MCPStatus: MCPConnected, Cost: "$0.0052"})
	if strings.Count(without, "\n") != strings.Count(with, "\n") {
		t.Error("showing the cost changed the composer height")
	}
}

// Too narrow for both: the connection state survives and the figure is dropped. It is
// the more important of the two, and a squeezed-together "● MCP$0.0052" reads as one
// corrupt token rather than two facts.
func TestView_CostYieldsToTheConnectionLightWhenNarrow(t *testing.T) {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	m.SetTheme(th)

	frame := stripAnsiC(m.View(ViewParams{Width: 8, MCPStatus: MCPConnected, Cost: "$0.0052"}))
	if strings.Contains(frame, "$0.0052") {
		t.Errorf("the cost was crammed onto a row too narrow for it:\n%s", frame)
	}
	if !strings.Contains(frame, "MCP") {
		t.Errorf("the connection light was dropped instead:\n%s", frame)
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
