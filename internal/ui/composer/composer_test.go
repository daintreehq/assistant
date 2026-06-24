package composer

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

func ansiWidth(s string) int { return ansi.StringWidth(s) }

// newModel builds a composer for tests with a small command list and the
// default (unicode) theme resolved from an empty env.
func newModel() Model {
	m := New(theme.Theme{Glyphs: unicodeGlyphsForTest()})
	m.SetCommands([]Command{
		{Name: "/clear", Desc: "wipe the transcript and host scrollback"},
		{Name: "/watchers", Desc: "show supervised agents"},
		{Name: "/inbox", Desc: "show attention queue"},
		{Name: "/help", Desc: "list commands"},
	})
	return m
}

// unicodeGlyphsForTest returns a minimal glyph set so View() doesn't panic in
// tests (the theme package's set is unexported; tests only need non-empty cues).
func unicodeGlyphsForTest() theme.GlyphSet {
	return theme.GlyphSet{Active: "◌", Bullet: "·"}
}

// typeRunes feeds each rune as a printable key press.
func typeRunes(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// press sends a special key with an optional modifier.
func press(m *Model, code rune, mod tea.KeyMod) Outcome {
	return m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
}

// pressText sends a chord that also carries text (e.g. Alt+b).
func pressChord(m *Model, code rune, mod tea.KeyMod) Outcome {
	return m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
}

func TestKillRingYank(t *testing.T) {
	m := newModel()
	typeRunes(&m, "hello world")
	// Ctrl-W kills the previous word ("world").
	pressChord(&m, 'w', tea.ModCtrl)
	if got := m.Value(); got != "hello " {
		t.Fatalf("after Ctrl-W: got %q want %q", got, "hello ")
	}
	// Ctrl-Y yanks it back at the cursor.
	pressChord(&m, 'y', tea.ModCtrl)
	if got := m.Value(); got != "hello world" {
		t.Fatalf("after Ctrl-Y: got %q want %q", got, "hello world")
	}
	if m.killRing != "world" {
		t.Fatalf("killRing = %q want %q", m.killRing, "world")
	}
}

func TestKillToEndEatsNewline(t *testing.T) {
	m := newModel()
	typeRunes(&m, "ab")
	m.insert("\n")
	typeRunes(&m, "cd")
	// Move cursor to end of first line (offset 2).
	m.cursor = 2
	// Ctrl-K at EOL eats the '\n', joining the lines.
	pressChord(&m, 'k', tea.ModCtrl)
	if got := m.Value(); got != "abcd" {
		t.Fatalf("Ctrl-K join: got %q want %q", got, "abcd")
	}
}

func TestWordMotion(t *testing.T) {
	m := newModel()
	typeRunes(&m, "alpha beta gamma")
	// Cursor at end. Alt+b → start of "gamma" (offset 11).
	pressChord(&m, 'b', tea.ModAlt)
	if m.cursor != 11 {
		t.Fatalf("Alt+b cursor = %d want 11", m.cursor)
	}
	// Ctrl+Left → start of "beta" (offset 6).
	press(&m, tea.KeyLeft, tea.ModCtrl)
	if m.cursor != 6 {
		t.Fatalf("Ctrl+Left cursor = %d want 6", m.cursor)
	}
	// Alt+f → end of "beta" (offset 10).
	pressChord(&m, 'f', tea.ModAlt)
	if m.cursor != 10 {
		t.Fatalf("Alt+f cursor = %d want 10", m.cursor)
	}
}

func TestHistoryRecall(t *testing.T) {
	m := newModel()
	// Two accepted submits seed the history.
	m.AcceptSubmit("first prompt")
	m.AcceptSubmit("second prompt")
	// Start a fresh draft, then ↑ recalls newest, ↑ again older.
	typeRunes(&m, "draft")
	press(&m, tea.KeyUp, 0)
	if m.Value() != "second prompt" {
		t.Fatalf("first ↑: got %q want %q", m.Value(), "second prompt")
	}
	press(&m, tea.KeyUp, 0)
	if m.Value() != "first prompt" {
		t.Fatalf("second ↑: got %q want %q", m.Value(), "first prompt")
	}
	// ↓ walks forward, then ↓ past the newest restores the live draft.
	press(&m, tea.KeyDown, 0)
	if m.Value() != "second prompt" {
		t.Fatalf("↓: got %q want %q", m.Value(), "second prompt")
	}
	press(&m, tea.KeyDown, 0)
	if m.Value() != "draft" {
		t.Fatalf("↓ past newest should restore draft: got %q", m.Value())
	}
}

func TestHistoryCapAndDedupe(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("same")
	m.AcceptSubmit("same") // immediate duplicate collapsed
	if len(m.history) != 1 {
		t.Fatalf("dedupe failed: history len = %d want 1", len(m.history))
	}
	for i := 0; i < historyLimit+50; i++ {
		m.recordHistory(itoa(i))
	}
	if len(m.history) != historyLimit {
		t.Fatalf("cap failed: history len = %d want %d", len(m.history), historyLimit)
	}
	// Newest kept.
	if m.history[len(m.history)-1] != itoa(historyLimit+49) {
		t.Fatalf("cap kept wrong tail: %q", m.history[len(m.history)-1])
	}
}

func TestBackslashNewlineFallback(t *testing.T) {
	m := newModel()
	typeRunes(&m, "line one\\")
	// Plain Enter with a trailing backslash converts it to a newline in place.
	out := press(&m, tea.KeyEnter, 0)
	if out.Submit != nil {
		t.Fatalf("backslash+Enter should NOT submit")
	}
	if m.Value() != "line one\n" {
		t.Fatalf("backslash newline: got %q want %q", m.Value(), "line one\n")
	}
	// A subsequent plain Enter (no trailing backslash) submits.
	typeRunes(&m, "line two")
	out = press(&m, tea.KeyEnter, 0)
	if out.Submit == nil || out.Submit.Text != "line one\nline two" {
		t.Fatalf("plain Enter should submit; got %+v", out.Submit)
	}
}

func TestModifierEnterNewline(t *testing.T) {
	m := newModel()
	typeRunes(&m, "x")
	press(&m, tea.KeyEnter, tea.ModShift)
	typeRunes(&m, "y")
	if m.Value() != "x\ny" {
		t.Fatalf("Shift+Enter newline: got %q want %q", m.Value(), "x\ny")
	}
}

func TestSlashFilterAndTab(t *testing.T) {
	m := newModel()
	// "/wa" matches /watchers by name prefix; the desc-substring branch is
	// exercised separately below. (A bare "/w" also matches /clear via "wipe" in
	// its description — the contract is name-prefix OR desc-contains.)
	typeRunes(&m, "/wa")
	s := m.activeSuggestions()
	if len(s) != 1 || s[0].Name != "/watchers" {
		t.Fatalf("/wa should match /watchers; got %+v", s)
	}
	// Description-substring match: "/queue" not a command, but "attention" is in
	// /inbox's desc.
	m.Reset()
	typeRunes(&m, "/attention")
	s = m.activeSuggestions()
	if len(s) != 1 || s[0].Name != "/inbox" {
		t.Fatalf("desc match should find /inbox; got %+v", s)
	}
	// Tab completes the top match with a trailing space.
	m.Reset()
	typeRunes(&m, "/cl")
	press(&m, tea.KeyTab, 0)
	if m.Value() != "/clear " {
		t.Fatalf("Tab complete: got %q want %q", m.Value(), "/clear ")
	}
}

func TestSlashPaletteSuppressedWhenBusy(t *testing.T) {
	m := newModel()
	m.SetBusy(true)
	typeRunes(&m, "/w")
	if s := m.activeSuggestions(); s != nil {
		t.Fatalf("palette must be suppressed while busy; got %+v", s)
	}
}

func TestEscClearsThenCancels(t *testing.T) {
	m := newModel()
	typeRunes(&m, "draft text")
	// Nonempty: Esc clears.
	out := press(&m, tea.KeyEscape, 0)
	if out.Cancel {
		t.Fatalf("Esc on nonempty should clear, not cancel")
	}
	if m.Value() != "" {
		t.Fatalf("Esc should clear buffer; got %q", m.Value())
	}
	// Empty + busy: Esc reports Cancel UP.
	m.SetBusy(true)
	out = press(&m, tea.KeyEscape, 0)
	if !out.Cancel {
		t.Fatalf("Esc on empty+busy should signal Cancel")
	}
	// Empty + idle: no-op.
	m.SetBusy(false)
	out = press(&m, tea.KeyEscape, 0)
	if out.Cancel {
		t.Fatalf("Esc on empty+idle should be a no-op")
	}
}

func TestBracketedPasteVerbatim(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: "one\r\ntwo\rthree"})
	if m.Value() != "one\ntwo\nthree" {
		t.Fatalf("paste newline-normalize: got %q want %q", m.Value(), "one\ntwo\nthree")
	}
}

// nLinePaste builds an n-line paste body ("line\nline\n…") for the large-paste
// tests; n lines means n-1 embedded newlines.
func nLinePaste(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line"
	}
	return strings.Join(lines, "\n")
}

func TestLargePasteShowsPlaceholder(t *testing.T) {
	m := newModel()
	paste := nLinePaste(8) // 8 lines ≥ the 5-line threshold
	out := m.Update(tea.PasteMsg{Content: paste})
	if out.Submit != nil {
		t.Fatal("a paste must never submit")
	}
	// The visible buffer is a single-line placeholder (no '\n') so the composer
	// stays one row and never trips the too-small fallback.
	if got := m.Value(); got != "[pasted 8 lines]" {
		t.Fatalf("placeholder buffer = %q, want %q", got, "[pasted 8 lines]")
	}
	if strings.Contains(m.Value(), "\n") {
		t.Fatalf("placeholder must contain no newline: %q", m.Value())
	}
	// Enter substitutes the real text back in.
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste {
		t.Fatalf("submit should carry the real paste; got %+v", sub.Submit)
	}
}

func TestLargePasteSingleLongLine(t *testing.T) {
	m := newModel()
	paste := strings.Repeat("a", 600) // one line, ≥ the 500-char threshold
	m.Update(tea.PasteMsg{Content: paste})
	if got := m.Value(); got != "[pasted 600 chars]" {
		t.Fatalf("single-line placeholder = %q, want %q", got, "[pasted 600 chars]")
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste {
		t.Fatalf("submit should carry the real long line; got %+v", sub.Submit)
	}
}

func TestSmallPasteBelowThresholdVerbatim(t *testing.T) {
	m := newModel()
	paste := nLinePaste(4) // 4 lines < 5 and well under 500 chars
	m.Update(tea.PasteMsg{Content: paste})
	if got := m.Value(); got != paste {
		t.Fatalf("small paste should insert verbatim; got %q want %q", got, paste)
	}
	if m.pasteText != "" {
		t.Fatalf("small paste must not stash; pasteText = %q", m.pasteText)
	}
}

func TestLargePasteEscClears(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	out := press(&m, tea.KeyEscape, 0)
	if out.Cancel {
		t.Fatal("Esc on a stashed paste should clear, not cancel")
	}
	if m.Value() != "" || m.pasteText != "" {
		t.Fatalf("Esc should clear buffer and stash; buffer=%q pasteText=%q", m.Value(), m.pasteText)
	}
}

func TestLargePasteEditDissolvesPlaceholder(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	// Typing into the placeholder dissolves the stash: the buffer becomes ordinary
	// editable text and the hidden paste is discarded (self-healing).
	typeRunes(&m, "x")
	if m.pasteText != "" {
		t.Fatalf("editing should clear the stash; pasteText = %q", m.pasteText)
	}
	if got := m.Value(); got != "[pasted 8 lines]x" {
		t.Fatalf("edited buffer = %q, want %q", got, "[pasted 8 lines]x")
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != "[pasted 8 lines]x" {
		t.Fatalf("submit should send the edited literal text; got %+v", sub.Submit)
	}
}

func TestLargePasteAcceptSubmitRecordsRealText(t *testing.T) {
	m := newModel()
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil {
		t.Fatal("expected a submit")
	}
	m.AcceptSubmit(sub.Submit.Text) // parent records the accepted text + resets
	if n := len(m.history); n == 0 || m.history[n-1] != paste {
		t.Fatalf("history should record the real paste, not the placeholder; got %+v", m.history)
	}
	if m.pasteText != "" || m.Value() != "" {
		t.Fatalf("AcceptSubmit should reset; buffer=%q pasteText=%q", m.Value(), m.pasteText)
	}
}

func TestLargePasteHistoryRecallReStashes(t *testing.T) {
	m := newModel()
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	sub := press(&m, tea.KeyEnter, 0)
	m.AcceptSubmit(sub.Submit.Text)
	// ↑ recalls the large paste: it must re-stash behind the placeholder (not paste
	// the full block back into the buffer) and still submit the real text.
	press(&m, tea.KeyUp, 0)
	if got := m.Value(); got != "[pasted 8 lines]" {
		t.Fatalf("history recall should re-stash; buffer = %q", got)
	}
	sub2 := press(&m, tea.KeyEnter, 0)
	if sub2.Submit == nil || sub2.Submit.Text != paste {
		t.Fatalf("recalled paste should submit the real text; got %+v", sub2.Submit)
	}
}

func TestLargePasteReplacesPrefilledBuffer(t *testing.T) {
	m := newModel()
	typeRunes(&m, "hi ")
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	// A large paste REPLACES the buffer with its placeholder (documented behavior);
	// the submitted text is the paste alone.
	if got := m.Value(); got != "[pasted 8 lines]" {
		t.Fatalf("large paste should replace the buffer; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste {
		t.Fatalf("submit should be the paste alone; got %+v", sub.Submit)
	}
}

func TestWhitespaceOnlyLargePasteInsertsVerbatim(t *testing.T) {
	m := newModel()
	// A large but whitespace-only paste must NOT stash (a placeholder would trim to
	// "" on submit and silently swallow Enter); it inserts verbatim instead.
	paste := strings.Repeat(" \n", 8) // 8 newlines, whitespace only
	m.Update(tea.PasteMsg{Content: paste})
	if m.pasteText != "" {
		t.Fatalf("whitespace-only paste must not stash; pasteText = %q", m.pasteText)
	}
	if strings.Contains(m.Value(), "pasted") {
		t.Fatalf("whitespace-only paste must not show a placeholder; got %q", m.Value())
	}
}

func TestLargePasteThresholdBoundary(t *testing.T) {
	// Exactly at the line / char thresholds triggers the placeholder; just below
	// inserts verbatim.
	cases := []struct {
		name    string
		paste   string
		stashed bool
	}{
		{"5 lines (== line threshold)", nLinePaste(5), true},
		{"4 lines (< line threshold)", nLinePaste(4), false},
		{"500 chars (== char threshold)", strings.Repeat("a", 500), true},
		{"499 chars (< char threshold)", strings.Repeat("a", 499), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel()
			m.Update(tea.PasteMsg{Content: tc.paste})
			gotStashed := m.pasteText != ""
			if gotStashed != tc.stashed {
				t.Fatalf("stashed = %v, want %v (buffer %q)", gotStashed, tc.stashed, m.Value())
			}
		})
	}
}

func TestLargePasteDoublePaste(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	pasteB := nLinePaste(12)
	m.Update(tea.PasteMsg{Content: pasteB})
	// The second large paste replaces the first stash entirely.
	if got := m.Value(); got != "[pasted 12 lines]" {
		t.Fatalf("second paste should replace the stash; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != pasteB {
		t.Fatalf("submit should carry the second paste; got %+v", sub.Submit)
	}
}

func TestTabCompletionClearsStalePaste(t *testing.T) {
	m := newModel()
	// Simulate a stale stash, then a Tab completion: the completion must clear it so
	// a later submit sends the command, not the paste.
	m.pasteText = nLinePaste(8)
	typeRunes(&m, "/cl")
	press(&m, tea.KeyTab, 0)
	if m.pasteText != "" {
		t.Fatalf("Tab completion should clear the stash; pasteText = %q", m.pasteText)
	}
	if m.Value() != "/clear " {
		t.Fatalf("Tab completion buffer = %q, want %q", m.Value(), "/clear ")
	}
}

func TestSearchEscRestoresStash(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("hello") // seed a small history entry so search can start
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste}) // stash active: buffer = placeholder
	pressChord(&m, 'r', tea.ModCtrl)       // Ctrl-R: snapshot buffer + stash
	typeRunes(&m, "hello")                 // matches the small entry → recall clears stash
	if m.pasteText != "" {
		t.Fatalf("matching a small entry should clear the stash; pasteText = %q", m.pasteText)
	}
	press(&m, tea.KeyEscape, 0) // cancel: restore the pre-search buffer AND stash
	if got := m.Value(); got != "[pasted 8 lines]" {
		t.Fatalf("Esc should restore the placeholder; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste {
		t.Fatalf("restored stash should submit the real paste; got %+v", sub.Submit)
	}
}

func TestWrapByCells(t *testing.T) {
	// View measures by cells, so a wide-rune draft never overflows the width.
	m := newModel()
	typeRunes(&m, "日本語テスト") // each CJK rune is 2 cells
	out := m.View(ViewParams{Width: 40, Placeholder: ""})
	for _, line := range strings.Split(out, "\n") {
		// No rendered line should exceed the width when measured by cells. The
		// prompt + content here is well under 40 cells; the assertion guards the
		// truncation/measurement path stays cell-aware.
		if w := ansiWidth(line); w > 60 {
			t.Fatalf("line exceeds expected cell bound: %d (%q)", w, line)
		}
	}
	// The caret column tracks runes, not bytes: cursor at end == rune count.
	if m.Cursor() != 6 {
		t.Fatalf("cursor should be 6 runes (not bytes); got %d", m.Cursor())
	}
}

func TestRenderInputWordWraps(t *testing.T) {
	// A long single logical line must wrap onto multiple visual rows (each within
	// the content width) instead of running off the right edge. footer() bounds the
	// live region by counting "\n"-delimited rows, so every rendered row must be
	// within Width — that contract is what this guards.
	m := newModel()
	const width = 24
	words := "the quick brown fox jumps over the lazy dog and keeps on running forever"
	typeRunes(&m, words)

	out := m.View(ViewParams{Width: width, Placeholder: ""})

	// Inspect every rendered line's cell width and collect the wrapped input rows.
	var inputRows []string
	for _, line := range strings.Split(stripAnsiC(out), "\n") {
		if w := ansiWidth(line); w > width {
			t.Fatalf("rendered line exceeds width %d: %d (%q)", width, w, line)
		}
		// The wrapped input rows carry the prompt or the indent; collect the prose by
		// trimming the leading prompt/indent (both 2 cells).
		if strings.HasPrefix(line, "› ") || strings.HasPrefix(line, "  ") {
			inputRows = append(inputRows, strings.TrimPrefix(strings.TrimPrefix(line, "› "), "  "))
		}
	}
	if len(inputRows) < 3 {
		t.Fatalf("expected the long line to wrap onto several rows, got %d: %v", len(inputRows), inputRows)
	}
	// Re-joining the wrapped rows (segments keep their trailing spaces) reproduces
	// the original text exactly — no runes dropped, no spaces collapsed.
	joined := strings.TrimRight(strings.Join(inputRows, ""), " ")
	if joined != words {
		t.Fatalf("wrapped rows must reconstruct the input:\n got %q\nwant %q", joined, words)
	}
}

func TestWrapSegmentsExactReconstruction(t *testing.T) {
	cases := []string{
		"hello world this is a long sentence",
		"supercalifragilisticexpialidocious-is-one-very-long-unbreakable-word",
		"日本語 テスト wide runes mixed in",
		"",
		"short",
	}
	for _, c := range cases {
		line := []rune(c)
		segs := wrapSegments(line, 10)
		var rebuilt []rune
		for _, s := range segs {
			if ansiWidth(string(s)) > 10 && len(s) > 1 {
				// Only a single over-wide rune may exceed the width; a multi-rune segment
				// must fit.
				t.Fatalf("segment exceeds width for %q: %q", c, string(s))
			}
			rebuilt = append(rebuilt, s...)
		}
		if string(rebuilt) != c {
			t.Fatalf("wrapSegments lost runes for %q: rebuilt %q", c, string(rebuilt))
		}
	}
}

func TestUpDownColumnMemory(t *testing.T) {
	m := newModel()
	typeRunes(&m, "longline")
	m.insert("\n")
	typeRunes(&m, "hi")
	// Cursor at end of "hi" (col 2). ↑ snaps to col 2 of "longline".
	press(&m, tea.KeyUp, 0)
	row, col := locate(runesOf(m.Value()), m.Cursor())
	if row != 0 || col != 2 {
		t.Fatalf("↑ column: got row %d col %d want row 0 col 2", row, col)
	}
}
