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

// statusThemedModel builds a composer the status-row tests can assert exact glyphs
// against: newModel()'s minimal set has no Waiting or Async glyph, and theme.Resolve()
// picks the ASCII fallback under DAINTREE_ASCII=1 or a non-UTF locale — which would fail
// these tests for an environment reason rather than a behavioural one. The three glyphs
// this row spends are therefore pinned explicitly; the ASCII path gets its own test.
func statusThemedModel() Model {
	m := newModel()
	th := theme.Resolve()
	th.Mode = theme.ModeDark
	th.Color = theme.PaletteFor(theme.ModeDark)
	th.Glyphs.Async = "●"
	th.Glyphs.Waiting = "◷"
	th.Glyphs.Bullet = "·"
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
		want     string   // "" → no supervision segment at all
		absent   []string // nouns that must NOT appear (a zero category is omitted, not zeroed)
	}{
		{"idle", 0, 0, "", []string{"timer", "watcher"}},
		{"one timer", 1, 0, "◷ 1 timer", []string{"watcher"}},
		{"several timers", 3, 0, "◷ 3 timers", []string{"watcher"}},
		{"one watcher", 0, 1, "◷ 1 watcher", []string{"timer"}},
		{"several watchers", 0, 2, "◷ 2 watchers", []string{"timer"}},
		{"one of each", 1, 1, "◷ 1 timer · 1 watcher", nil},
		{"both plural", 2, 3, "◷ 2 timers · 3 watchers", nil},
		// Negative counts cannot arrive from production (they come from len()), but the
		// formatter must not render "-1 timers" if one ever did.
		{"negative counts", -1, -4, "", []string{"timer", "watcher"}},
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
			for _, noun := range tc.absent {
				if strings.Contains(row, noun) {
					t.Errorf("a zero/negative category was still named (%q): %q", noun, row)
				}
			}
			if tc.want == "" {
				// Hidden entirely at zero — the segment APPEARING is the signal, so an
				// idle session must not gain a glyph, a "0", or a trailing separator. The
				// row is pinned EXACTLY here, because a substring check would happily pass
				// on a dangling "● MCP · ".
				if got := strings.TrimRight(row, " "); got != "● MCP" {
					t.Errorf("idle status row is not bare: %q", got)
				}
				return
			}
			if !strings.Contains(row, tc.want) {
				// Fatal: the ordering checks below index on "◷" and would report a
				// nonsense second failure once it is absent.
				t.Fatalf("want %q on the MCP row, got %q", tc.want, row)
			}
			// It belongs beside the connection light, not on the key-hints row.
			if strings.Contains(hintLineC(frame), "◷") {
				t.Errorf("supervision leaked onto the hint row: %q", hintLineC(frame))
			}
			// The connection light keeps the left anchor and supervision follows it —
			// pinned by EQUALITY, not a prefix, so a dangling trailing separator or a
			// silently appended extra category cannot slip through.
			if want := "● MCP · " + tc.want; strings.TrimRight(row, " ") != want {
				t.Errorf("want the row to be %q, got %q", want, strings.TrimRight(row, " "))
			}
		})
	}
}

// With the connection light hidden the supervision segment becomes the left anchor
// itself — no leading separator, no cell of padding standing in for the missing MCP
// label. Unreachable from the production caller (which always reports a connection
// state), but it is a real branch in statusRow and cheap to pin.
func TestView_SupervisionAnchorsTheRowWhenMCPIsHidden(t *testing.T) {
	m := statusThemedModel()
	for _, cost := range []string{"", "$0.0052"} {
		frame := stripAnsiC(m.View(ViewParams{
			Width: 80, MCPStatus: MCPHidden, Cost: cost,
			TimerCount: 1, WatcherCount: 2,
		}))
		var row string
		for _, line := range strings.Split(frame, "\n") {
			if strings.Contains(line, "◷") {
				row = line
			}
		}
		if row == "" {
			t.Fatalf("cost=%q: no supervision row in:\n%s", cost, frame)
		}
		if !strings.HasPrefix(row, "◷ 1 timer · 2 watchers") {
			t.Errorf("cost=%q: supervision is not the left anchor: %q", cost, row)
		}
		if cost != "" && !strings.HasSuffix(strings.TrimRight(row, " "), cost) {
			t.Errorf("cost=%q: the bill left the right edge: %q", cost, row)
		}
	}
}

// The live footer's row count is load-bearing for the scrollback commit arithmetic, so
// supervision must share the connection row rather than claim one of its own — at zero
// counts, at nonzero counts, and when the counts change under it.
func TestView_SupervisionDoesNotChangeComposerHeight(t *testing.T) {
	m := statusThemedModel()

	idle := m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052"})
	oneTimer := m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052", TimerCount: 1})
	both := m.View(ViewParams{Width: 80, MCPStatus: MCPConnected, Cost: "$0.0052", TimerCount: 4, WatcherCount: 7})

	// Prove the counts actually rendered FIRST: an implementation that ignored them
	// outright would keep every height below identical and pass vacuously.
	if !strings.Contains(stripAnsiC(oneTimer), "◷ 1 timer") {
		t.Fatalf("the single count never rendered:\n%s", stripAnsiC(oneTimer))
	}
	if !strings.Contains(stripAnsiC(both), "◷ 4 timers · 7 watchers") {
		t.Fatalf("the combined counts never rendered:\n%s", stripAnsiC(both))
	}

	if strings.Count(idle, "\n") != strings.Count(oneTimer, "\n") {
		t.Error("showing a supervision count changed the composer height")
	}
	if strings.Count(oneTimer, "\n") != strings.Count(both, "\n") {
		t.Error("changing the supervision counts changed the composer height")
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
	const segment = "◷ 1 timer · 2 watchers"
	render := func(w int) string {
		return stripAnsiC(m.View(ViewParams{
			Width: w, MCPStatus: MCPConnected, Cost: cost,
			TimerCount: 1, WatcherCount: 2,
		}))
	}

	// Wide: all three facts share the row, the bill against the right edge.
	wide := mcpRowOf(render(100))
	if !strings.HasPrefix(wide, "● MCP · "+segment) {
		t.Fatalf("wide row is missing a segment: %q", wide)
	}
	if !strings.HasSuffix(wide, cost) || ansiWidth(wide) != 100 {
		t.Fatalf("the bill is not at the right edge of a full-width row: %q", wide)
	}

	// Sweep down. Assertions run against the WHOLE frame rather than a row located by
	// its "MCP" text: below the width that fits the connection label, that search finds
	// nothing and every check on it silently degrades to a no-op on "".
	costGone, supGone, sawMiddlePhase := false, false, false
	for w := 100; w >= 1; w-- {
		frame := render(w)
		hasCost := strings.Contains(frame, cost)
		hasSup := strings.Contains(frame, "◷")

		if hasCost && costGone {
			t.Fatalf("width %d: the bill came back after being dropped:\n%s", w, frame)
		}
		if hasSup && supGone {
			t.Fatalf("width %d: supervision came back after being dropped:\n%s", w, frame)
		}
		switch {
		case hasSup && !hasCost:
			// The phase that proves the priority is an ORDER and not a single cliff:
			// supervision outliving the bill by at least one column.
			sawMiddlePhase = true
		case !hasSup && hasCost:
			t.Fatalf("width %d: the bill outlived supervision:\n%s", w, frame)
		}
		if !hasCost {
			costGone = true
			// Atomic: the bill is dropped whole, never left as a stub like "$0.00…".
			if strings.Contains(frame, "$") {
				t.Fatalf("width %d: a partial bill survived:\n%s", w, frame)
			}
		}
		if !hasSup {
			supGone = true
		}
		// Never squeezed: a supervision segment that renders at all renders whole.
		if hasSup && !strings.Contains(frame, segment) {
			t.Fatalf("width %d: supervision was truncated mid-segment:\n%s", w, frame)
		}
		// No line of the frame may exceed the width it was given, at ANY width — this is
		// the part the "MCP"-anchored search used to stop checking.
		for i, line := range strings.Split(frame, "\n") {
			if got := ansiWidth(line); got > w {
				t.Fatalf("width %d: line %d overflowed to %d cells: %q", w, i, got, line)
			}
		}
	}
	if !costGone || !supGone {
		t.Fatal("the sweep never got narrow enough to drop both — the test proves nothing")
	}
	if !sawMiddlePhase {
		t.Fatal("the bill and supervision dropped at the same width — priority is a cliff, not an order")
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
