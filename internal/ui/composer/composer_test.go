package composer

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/ui/theme"
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
	// The visible buffer is a single-line placeholder token (no '\n') so the composer
	// stays one row and never trips the too-small fallback.
	if got := m.Value(); got != "[pasted 8 lines #1]" {
		t.Fatalf("placeholder buffer = %q, want %q", got, "[pasted 8 lines #1]")
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
	if got := m.Value(); got != "[pasted 600 chars #1]" {
		t.Fatalf("single-line placeholder = %q, want %q", got, "[pasted 600 chars #1]")
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
	if len(m.pastes) != 0 {
		t.Fatalf("small paste must not stash; pastes = %+v", m.pastes)
	}
}

func TestLargePasteEscClears(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	out := press(&m, tea.KeyEscape, 0)
	if out.Cancel {
		t.Fatal("Esc on a stashed paste should clear, not cancel")
	}
	if m.Value() != "" || len(m.pastes) != 0 {
		t.Fatalf("Esc should clear buffer and stash; buffer=%q pastes=%+v", m.Value(), m.pastes)
	}
}

func TestLargePasteEditDissolvesPlaceholder(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	// Editing INTO the token breaks it apart, so it stops being a substring of the
	// buffer and its stash self-heals away: the placeholder becomes ordinary literal
	// text and the hidden paste is discarded.
	press(&m, tea.KeyBackspace, 0) // deletes the trailing ']' of "[pasted 8 lines #1]"
	if len(m.pastes) != 0 {
		t.Fatalf("editing into the token should clear the stash; pastes = %+v", m.pastes)
	}
	if got := m.Value(); got != "[pasted 8 lines #1" {
		t.Fatalf("edited buffer = %q, want %q", got, "[pasted 8 lines #1")
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != "[pasted 8 lines #1" {
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
	if len(m.pastes) != 0 || m.Value() != "" {
		t.Fatalf("AcceptSubmit should reset; buffer=%q pastes=%+v", m.Value(), m.pastes)
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
	if got := m.Value(); got != "[pasted 8 lines #1]" {
		t.Fatalf("history recall should re-stash; buffer = %q", got)
	}
	sub2 := press(&m, tea.KeyEnter, 0)
	if sub2.Submit == nil || sub2.Submit.Text != paste {
		t.Fatalf("recalled paste should submit the real text; got %+v", sub2.Submit)
	}
}

func TestLargePasteCoexistsWithPrefilledBuffer(t *testing.T) {
	m := newModel()
	typeRunes(&m, "hi ")
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	// A large paste does NOT wipe what the user already typed: its placeholder token
	// is inserted at the cursor, so typed text and the pasted block coexist.
	if got := m.Value(); got != "hi [pasted 8 lines #1]" {
		t.Fatalf("paste should coexist with typed text; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != "hi "+paste {
		t.Fatalf("submit should carry typed text + the paste; got %+v", sub.Submit)
	}
}

func TestWhitespaceOnlyLargePasteInsertsVerbatim(t *testing.T) {
	m := newModel()
	// A large but whitespace-only paste must NOT stash (a placeholder would trim to
	// "" on submit and silently swallow Enter); it inserts verbatim instead.
	paste := strings.Repeat(" \n", 8) // 8 newlines, whitespace only
	m.Update(tea.PasteMsg{Content: paste})
	if len(m.pastes) != 0 {
		t.Fatalf("whitespace-only paste must not stash; pastes = %+v", m.pastes)
	}
	if strings.Contains(m.Value(), "pasted") {
		t.Fatalf("whitespace-only paste must not show a placeholder; got %q", m.Value())
	}
	if got := m.Value(); got != normalizeNewlines(paste) {
		t.Fatalf("whitespace paste should insert verbatim; got %q want %q", got, normalizeNewlines(paste))
	}
	// The whole point of the branch: a whitespace-only buffer must not swallow Enter.
	if sub := press(&m, tea.KeyEnter, 0); sub.Submit != nil {
		t.Fatalf("a whitespace-only buffer must not submit; got %+v", sub.Submit)
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
			gotStashed := len(m.pastes) > 0
			if gotStashed != tc.stashed {
				t.Fatalf("stashed = %v, want %v (buffer %q)", gotStashed, tc.stashed, m.Value())
			}
		})
	}
}

func TestLargePasteDoublePaste(t *testing.T) {
	m := newModel()
	pasteA := nLinePaste(8)
	pasteB := nLinePaste(12)
	m.Update(tea.PasteMsg{Content: pasteA})
	m.Update(tea.PasteMsg{Content: pasteB})
	// Each large paste gets its OWN numbered placeholder; both coexist in the buffer
	// (the second no longer clobbers the first).
	if got := m.Value(); got != "[pasted 8 lines #1][pasted 12 lines #2]" {
		t.Fatalf("both pastes should get their own placeholder; got %q", got)
	}
	if len(m.pastes) != 2 {
		t.Fatalf("expected 2 stashes, got %d", len(m.pastes))
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != pasteA+pasteB {
		t.Fatalf("submit should carry BOTH pastes in order; got %+v", sub.Submit)
	}
}

func TestKillLineClearsStash(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)})
	// A whole-line kill removes the token, so reconcile drops the now-orphaned stash;
	// a later submit must not resurrect the paste.
	pressChord(&m, 'u', tea.ModCtrl) // Ctrl-U kills the whole logical line
	if len(m.pastes) != 0 {
		t.Fatalf("killing the line should drop the stash; pastes = %+v", m.pastes)
	}
	if m.Value() != "" {
		t.Fatalf("buffer should be empty after the kill; got %q", m.Value())
	}
}

func TestSearchEscRestoresStash(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("hello") // seed a small history entry so search can start
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste}) // stash active: buffer = placeholder
	pressChord(&m, 'r', tea.ModCtrl)       // Ctrl-R: snapshot buffer + stash
	typeRunes(&m, "hello")                 // matches the small entry → recall clears stash
	if len(m.pastes) != 0 {
		t.Fatalf("matching a small entry should clear the stash; pastes = %+v", m.pastes)
	}
	press(&m, tea.KeyEscape, 0) // cancel: restore the pre-search buffer AND stash
	if got := m.Value(); got != "[pasted 8 lines #1]" {
		t.Fatalf("Esc should restore the placeholder; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste {
		t.Fatalf("restored stash should submit the real paste; got %+v", sub.Submit)
	}
}

func TestPasteCoexistsWithTypedNewlines(t *testing.T) {
	m := newModel()
	// The reported bug: type a message, add line breaks, THEN paste a big block —
	// the paste must not wipe the typed text. Shift+Enter inserts a newline.
	typeRunes(&m, "please review")
	press(&m, tea.KeyEnter, tea.ModShift)
	typeRunes(&m, "this:")
	paste := nLinePaste(20)
	m.Update(tea.PasteMsg{Content: paste})
	wantBuf := "please review\nthis:[pasted 20 lines #1]"
	if got := m.Value(); got != wantBuf {
		t.Fatalf("typed text (with newlines) must survive the paste; got %q want %q", got, wantBuf)
	}
	sub := press(&m, tea.KeyEnter, 0)
	want := "please review\nthis:" + paste
	if sub.Submit == nil || sub.Submit.Text != want {
		t.Fatalf("submit should carry typed text + the paste; got %+v", sub.Submit)
	}
}

func TestPasteInsertsAtCursor(t *testing.T) {
	m := newModel()
	typeRunes(&m, "AB")
	press(&m, tea.KeyLeft, 0) // cursor between A and B
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	if got := m.Value(); got != "A[pasted 8 lines #1]B" {
		t.Fatalf("paste should insert at the cursor; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != "A"+paste+"B" {
		t.Fatalf("submit should expand the paste in place; got %+v", sub.Submit)
	}
}

func TestMultiplePastesInterleavedWithText(t *testing.T) {
	m := newModel()
	a := nLinePaste(8)            // "[pasted 8 lines #1]"
	b := strings.Repeat("z", 600) // char-threshold paste → "[pasted 600 chars #2]"
	m.Update(tea.PasteMsg{Content: a})
	typeRunes(&m, " and ")
	m.Update(tea.PasteMsg{Content: b})
	wantBuf := "[pasted 8 lines #1] and [pasted 600 chars #2]"
	if got := m.Value(); got != wantBuf {
		t.Fatalf("two pastes with text between; got %q want %q", got, wantBuf)
	}
	sub := press(&m, tea.KeyEnter, 0)
	want := a + " and " + b
	if sub.Submit == nil || sub.Submit.Text != want {
		t.Fatalf("submit should expand both pastes in order; got %+v", sub.Submit)
	}
}

func TestPasteAppendKeepsStash(t *testing.T) {
	m := newModel()
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	typeRunes(&m, "!") // typing AFTER the token leaves it intact — the stash survives
	if len(m.pastes) != 1 {
		t.Fatalf("appending after the token must keep the stash; pastes = %+v", m.pastes)
	}
	if got := m.Value(); got != "[pasted 8 lines #1]!" {
		t.Fatalf("buffer = %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != paste+"!" {
		t.Fatalf("submit should be the paste then the typed char; got %+v", sub.Submit)
	}
}

func TestEditingOneTokenKeepsOtherStash(t *testing.T) {
	m := newModel()
	a := nLinePaste(8)  // "[pasted 8 lines #1]"
	b := nLinePaste(12) // "[pasted 12 lines #2]"
	m.Update(tea.PasteMsg{Content: a})
	m.Update(tea.PasteMsg{Content: b}) // cursor at end
	// Backspace breaks the SECOND token only; the first stash must survive.
	press(&m, tea.KeyBackspace, 0)
	if len(m.pastes) != 1 || m.pastes[0].text != a {
		t.Fatalf("only the damaged stash should drop; pastes = %+v", m.pastes)
	}
	sub := press(&m, tea.KeyEnter, 0)
	// #1 expands to a; the broken #2 token is now literal text (its ']' deleted).
	want := a + "[pasted 12 lines #2"
	if sub.Submit == nil || sub.Submit.Text != want {
		t.Fatalf("submit should expand #1 and keep the broken #2 literal; got %+v", sub.Submit)
	}
}

func TestHistoryDraftRoundTripPreservesMixedDraft(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("older entry") // seed history so ↑ has something to recall
	// A mixed draft: typed text + a large paste coexisting.
	typeRunes(&m, "hi ")
	paste := nLinePaste(8)
	m.Update(tea.PasteMsg{Content: paste})
	if got := m.Value(); got != "hi [pasted 8 lines #1]" {
		t.Fatalf("precondition draft; got %q", got)
	}
	// ↑ recalls the history entry; ↓ steps back past the newest → restore the draft.
	press(&m, tea.KeyUp, 0)
	if m.Value() != "older entry" {
		t.Fatalf("↑ should recall history; got %q", m.Value())
	}
	press(&m, tea.KeyDown, 0)
	// The mixed draft must return EXACTLY — typed prefix + the inline token intact,
	// not collapsed into a single re-stashed paste.
	if got := m.Value(); got != "hi [pasted 8 lines #1]" {
		t.Fatalf("↓ should restore the mixed draft verbatim; got %q", got)
	}
	if len(m.pastes) != 1 {
		t.Fatalf("draft restore should keep the stash; pastes = %+v", m.pastes)
	}
	sub := press(&m, tea.KeyEnter, 0)
	if sub.Submit == nil || sub.Submit.Text != "hi "+paste {
		t.Fatalf("restored draft should submit typed text + paste; got %+v", sub.Submit)
	}
}

func TestThreePastesMiddleEditKeepsOthers(t *testing.T) {
	m := newModel()
	a := nLinePaste(6)
	b := nLinePaste(8)
	c := nLinePaste(10)
	m.Update(tea.PasteMsg{Content: a}) // #1
	m.Update(tea.PasteMsg{Content: b}) // #2
	m.Update(tea.PasteMsg{Content: c}) // #3
	// Damage the MIDDLE token by deleting its trailing ']' (cursor just past it).
	tok2 := "[pasted 8 lines #2]"
	idx := strings.Index(m.Value(), tok2)
	if idx < 0 {
		t.Fatalf("precondition: buffer %q missing %q", m.Value(), tok2)
	}
	m.cursor = idx + len([]rune(tok2)) // tokens are ASCII, so byte idx == rune idx here
	press(&m, tea.KeyBackspace, 0)     // delete #2's ']'
	if len(m.pastes) != 2 {
		t.Fatalf("only the middle stash should drop; pastes = %+v", m.pastes)
	}
	sub := press(&m, tea.KeyEnter, 0)
	// #1 and #3 still expand; the broken #2 stays literal (its ']' gone).
	want := a + "[pasted 8 lines #2" + c
	if sub.Submit == nil || sub.Submit.Text != want {
		t.Fatalf("submit should expand #1 and #3 and keep #2 literal; got %+v", sub.Submit)
	}
}

func TestPasteBodyContainingAnotherTokenLiteralExpandsOnce(t *testing.T) {
	m := newModel()
	a := nLinePaste(8)                                            // → "[pasted 8 lines #1]"
	b := "look: [pasted 8 lines #1] is a token\n" + nLinePaste(6) // body literally contains A's token
	m.Update(tea.PasteMsg{Content: a})
	m.Update(tea.PasteMsg{Content: b})
	if got := m.Value(); got != "[pasted 8 lines #1][pasted 7 lines #2]" {
		t.Fatalf("tokens; got %q", got)
	}
	sub := press(&m, tea.KeyEnter, 0)
	// Expand scans the BUFFER (only tokens live there) and never re-scans emitted
	// body text, so the "[pasted 8 lines #1]" literal inside b's body is NOT
	// recursively expanded — each token expands exactly once.
	if sub.Submit == nil || sub.Submit.Text != a+b {
		t.Fatalf("submit should expand each token exactly once; got %+v", sub.Submit)
	}
}

func TestBackspaceThroughWholeTokenDropsStash(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)}) // "[pasted 8 lines #1]", cursor at end
	for range []rune("[pasted 8 lines #1]") {
		press(&m, tea.KeyBackspace, 0)
	}
	if m.Value() != "" {
		t.Fatalf("buffer should be empty after deleting the token; got %q", m.Value())
	}
	if len(m.pastes) != 0 {
		t.Fatalf("stash must be gone; pastes = %+v", m.pastes)
	}
	// The paste must not be resurrected: an empty buffer submits nothing.
	if sub := press(&m, tea.KeyEnter, 0); sub.Submit != nil {
		t.Fatalf("empty buffer should not submit; got %+v", sub.Submit)
	}
}

func TestCtrlWPartialTokenDamageDropsStash(t *testing.T) {
	m := newModel()
	m.Update(tea.PasteMsg{Content: nLinePaste(8)}) // "[pasted 8 lines #1]", cursor at end
	// Ctrl-W kills the previous whitespace-word. Tokens contain spaces, so this eats
	// only the trailing "#1]" — breaking the token and dropping its stash.
	pressChord(&m, 'w', tea.ModCtrl)
	if len(m.pastes) != 0 {
		t.Fatalf("word-kill into the token should drop the stash; pastes = %+v", m.pastes)
	}
	if got := m.Value(); got != "[pasted 8 lines " {
		t.Fatalf("buffer after Ctrl-W; got %q", got)
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
