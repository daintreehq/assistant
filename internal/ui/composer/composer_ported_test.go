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

// statusThemedModel builds a composer with the real (unicode) glyph set resolved, which
// the status-row tests need: newModel()'s minimal set has no Waiting or Async glyph.
func statusThemedModel() Model {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	m.SetTheme(th)
	return m
}

// mcpRowOf returns the rendered status row (the one carrying the connection light), or
// "" when the frame has none.
func mcpRowOf(frame string) string {
	var row string
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "MCP") {
			row = line
		}
	}
	return row
}

// Background supervision was previously invisible: a durable 60-minute timer left the
// footer looking identical to an idle session's. The counts now ride the connection row,
// each category appearing only when it is nonzero — a session with only watchers must not
// claim "0 timers", and an idle one must stay exactly as quiet as it was before.
func TestView_SupervisionCountsOnTheMCPRow(t *testing.T) {
	cases := []struct {
		name     string
		timers   int
		watchers int
		want     string // "" → no supervision segment at all
	}{
		{"idle", 0, 0, ""},
		{"one timer", 1, 0, "◷ 1 timer"},
		{"several timers", 3, 0, "◷ 3 timers"},
		{"one watcher", 0, 1, "◷ 1 watcher"},
		{"several watchers", 0, 2, "◷ 2 watchers"},
		{"one of each", 1, 1, "◷ 1 timer · 1 watcher"},
		{"both plural", 2, 3, "◷ 2 timers · 3 watchers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := statusThemedModel()
			frame := stripAnsiC(m.View(ViewParams{
				Width: 100, MCPStatus: MCPConnected,
				TimerCount: tc.timers, WatcherCount: tc.watchers,
			}))
			row := mcpRowOf(frame)
			if row == "" {
				t.Fatalf("no MCP row in:\n%s", frame)
			}
			if tc.want == "" {
				// Hidden entirely at zero — the segment APPEARING is the signal, so an
				// idle session must not gain a glyph, a "0", or a stray separator.
				if strings.Contains(row, "◷") || strings.Contains(row, "timer") || strings.Contains(row, "watcher") {
					t.Errorf("idle session grew a supervision segment: %q", row)
				}
				return
			}
			if !strings.Contains(row, tc.want) {
				t.Errorf("want %q on the MCP row, got %q", tc.want, row)
			}
			// It belongs beside the connection light, not on the key-hints row.
			if strings.Contains(hintLineC(frame), "◷") {
				t.Errorf("supervision leaked onto the hint row: %q", hintLineC(frame))
			}
			// The connection light keeps the left anchor; supervision follows it.
			if !strings.HasPrefix(strings.TrimLeft(row, " "), "●") {
				t.Errorf("the connection light lost its left anchor: %q", row)
			}
			if strings.Index(row, "●") > strings.Index(row, "◷") {
				t.Errorf("supervision rendered before the connection light: %q", row)
			}
		})
	}
}

// The live footer's row count is load-bearing for the scrollback commit arithmetic, so
// supervision must share the connection row rather than claim one of its own — at zero
// counts, at nonzero counts, and when the counts change under it.
func TestView_SupervisionDoesNotChangeComposerHeight(t *testing.T) {
	m := statusThemedModel()
	base := ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052"}

	idle := m.View(base)
	oneTimer := m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052", TimerCount: 1})
	both := m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052", TimerCount: 4, WatcherCount: 7})

	if strings.Count(idle, "\n") != strings.Count(oneTimer, "\n") {
		t.Error("showing a supervision count changed the composer height")
	}
	if strings.Count(oneTimer, "\n") != strings.Count(both, "\n") {
		t.Error("changing the supervision counts changed the composer height")
	}
	// Zero counts must leave the frame byte-identical to one that never knew about
	// supervision at all — an idle session stays exactly as quiet as it was.
	if idle != m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052", TimerCount: 0, WatcherCount: 0}) {
		t.Error("zero counts perturbed the idle frame")
	}
}

// Priority on a pane too narrow for everything: connection light > supervision > bill.
// Each loser is dropped WHOLE (a half-rendered "◷ 1 ti…" reads as corruption), and the
// drops are MONOTONIC — once supervision goes, the bill does not move into the cells it
// freed, or shrinking by one column would swap one fact for another and make a
// width-driven disappearance read as "supervision ended".
func TestView_StatusRowDropsCostThenSupervisionWhenNarrow(t *testing.T) {
	m := statusThemedModel()
	const cost = "$0.0052"
	render := func(w int) string {
		return stripAnsiC(m.View(ViewParams{
			Width: w, MCPStatus: MCPConnected, Cost: cost,
			TimerCount: 1, WatcherCount: 2,
		}))
	}

	// Wide: all three facts share the row, the bill against the right edge.
	wide := mcpRowOf(render(100))
	if !strings.Contains(wide, "● MCP") || !strings.Contains(wide, "◷ 1 timer · 2 watchers") {
		t.Fatalf("wide row is missing a segment: %q", wide)
	}
	if !strings.HasSuffix(strings.TrimRight(wide, " "), cost) {
		t.Fatalf("the bill is not at the right edge: %q", wide)
	}

	// Sweep down. Track the phase transitions and assert they only ever go one way.
	costGone, supGone := false, false
	for w := 100; w >= 1; w-- {
		row := mcpRowOf(render(w))
		hasCost := strings.Contains(row, cost)
		hasSup := strings.Contains(row, "◷")

		if hasCost && costGone {
			t.Fatalf("width %d: the bill came back after being dropped: %q", w, row)
		}
		if hasSup && supGone {
			t.Fatalf("width %d: supervision came back after being dropped: %q", w, row)
		}
		if !hasCost {
			costGone = true
		}
		if !hasSup {
			supGone = true
			// Monotonic: supervision is only dropped once the bill already is.
			if hasCost {
				t.Fatalf("width %d: the bill outlived supervision: %q", w, row)
			}
		}
		// Never squeezed: a supervision segment that renders at all renders whole.
		if hasSup && !strings.Contains(row, "◷ 1 timer · 2 watchers") {
			t.Fatalf("width %d: supervision was truncated mid-segment: %q", w, row)
		}
		// The row must never exceed the width it was given.
		if got := ansiWidth(row); got > w {
			t.Fatalf("width %d: status row overflowed to %d cells: %q", w, got, row)
		}
	}
	if !costGone || !supGone {
		t.Fatal("the sweep never got narrow enough to drop both — the test proves nothing")
	}

	// The connection light is the last thing standing.
	if !strings.Contains(render(6), "MCP") {
		t.Errorf("the connection light was dropped before the others:\n%s", render(6))
	}
}

// The glyph comes from the theme, so the ASCII fallback (non-UTF locale / DAINTREE_ASCII)
// gets a stand-in rather than a mojibake box.
func TestView_SupervisionUsesTheThemeWaitingGlyph(t *testing.T) {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	th.Glyphs.Waiting = "~"
	m.SetTheme(th)

	row := mcpRowOf(stripAnsiC(m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, TimerCount: 1})))
	if !strings.Contains(row, "~ 1 timer") {
		t.Errorf("the Waiting glyph is hard-coded rather than read from the theme: %q", row)
	}
	if strings.Contains(row, "◷") {
		t.Errorf("the unicode glyph survived an ASCII theme: %q", row)
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
