package composer

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// revamp_test.go covers the composer ergonomics tier: word/WORD motion (T13), Ctrl-R reverse
// search (T13), visual-row vertical motion + caret visibility (T14), and the fuzzy palette
// with selection nav + arg-token matching (T11).

// --- T13: word/WORD semantics ---

func TestWordMotionStopsAtPunctuation(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/usr/local/bin")
	pressChord(&m, 'b', tea.ModAlt) // Alt+b: word motion stops at the last path segment
	if m.cursor != 11 {
		t.Fatalf("Alt+b should stop at 'bin' (offset 11), got %d", m.cursor)
	}
}

func TestCtrlWWhitespaceKillsWholePath(t *testing.T) {
	m := newModel()
	typeRunes(&m, "cmd /usr/local/bin")
	pressChord(&m, 'w', tea.ModCtrl) // Ctrl-W: WORD (whitespace) — wipes the whole path
	if m.Value() != "cmd " {
		t.Fatalf("Ctrl-W should wipe the whole path back to the space; got %q", m.Value())
	}
}

// --- T13: Ctrl-R reverse-i-search ---

func TestReverseSearchFindsAndSteps(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("deploy the app")
	m.AcceptSubmit("run the tests")
	m.AcceptSubmit("deploy again")

	pressChord(&m, 'r', tea.ModCtrl)
	if !m.searching {
		t.Fatal("Ctrl-R did not enter search mode")
	}
	typeRunes(&m, "deploy")
	if m.Value() != "deploy again" {
		t.Fatalf("search should land on the newest 'deploy' match; got %q", m.Value())
	}
	pressChord(&m, 'r', tea.ModCtrl) // step to the older match
	if m.Value() != "deploy the app" {
		t.Fatalf("second Ctrl-R should step to the older match; got %q", m.Value())
	}
	out := press(&m, tea.KeyEnter, 0)
	if m.searching {
		t.Fatal("Enter did not leave search mode")
	}
	if out.Submit == nil || out.Submit.Text != "deploy the app" {
		t.Fatalf("Enter should accept + submit the match; got %+v", out.Submit)
	}
}

func TestReverseSearchEscRestoresDraft(t *testing.T) {
	m := newModel()
	m.AcceptSubmit("history item")
	typeRunes(&m, "draft")
	pressChord(&m, 'r', tea.ModCtrl)
	typeRunes(&m, "history")
	if m.Value() != "history item" {
		t.Fatalf("search did not find the match; got %q", m.Value())
	}
	press(&m, tea.KeyEscape, 0)
	if m.searching {
		t.Fatal("Esc did not leave search mode")
	}
	if m.Value() != "draft" {
		t.Fatalf("Esc should restore the pre-search draft; got %q", m.Value())
	}
}

// --- T14: visual-row motion + caret ---

func TestVisualRowMotionStaysInBuffer(t *testing.T) {
	m := newModel()
	m.SetWidth(12) // narrow → the single logical line wraps onto several visual rows
	typeRunes(&m, "alpha bravo charlie delta")
	press(&m, tea.KeyUp, 0)
	// With visual-row motion, ↑ moves up a wrapped row (mid-buffer). Without it, a single
	// logical line has no row above, so it would fall to history (empty → cursor 0).
	if c := m.Cursor(); c == 0 || c >= m.runeLen() {
		t.Fatalf("↑ should move up one VISUAL row (mid-buffer), got cursor %d of %d", c, m.runeLen())
	}
}

func TestCaretRendersAtInteriorEOL(t *testing.T) {
	m := newModel()
	// Caret at/after a row's end must render a trailing caret cell, not bare text — so the
	// cursor stays visible at the EOL of a wrapped/interior line.
	if out := m.renderLine([]rune("abc"), true, 3); out == "abc" {
		t.Fatal("caret at EOL rendered no caret cell")
	}
}

// --- T11: fuzzy palette + nav ---

func TestFuzzyPaletteSubsequence(t *testing.T) {
	cmds := []Command{
		{Name: "/watchers", Desc: "supervised agents"},
		{Name: "/compact", Desc: "summarize"},
	}
	s := suggestionsFor(cmds, "/wtch") // "wtch" is a subsequence of "watchers"
	if len(s) == 0 || s[0].Name != "/watchers" {
		t.Fatalf("fuzzy subsequence should match /watchers; got %+v", s)
	}
}

func TestPaletteStaysOpenWhileTypingArgs(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/inbox urgent")
	s := m.activeSuggestions()
	if len(s) != 1 || s[0].Name != "/inbox" {
		t.Fatalf("palette should stay open (match the command token) while typing args; got %+v", s)
	}
}

func TestPaletteNavAndTabAccept(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/") // lists all commands
	s := m.activeSuggestions()
	if len(s) < 2 {
		t.Fatalf("'/' should list commands; got %d", len(s))
	}
	press(&m, tea.KeyDown, 0)
	if m.paletteSel != 1 {
		t.Fatalf("↓ should move the palette selection to 1; got %d", m.paletteSel)
	}
	press(&m, tea.KeyTab, 0)
	if want := s[1].Name + " "; m.Value() != want {
		t.Fatalf("Tab should accept the highlighted command %q; got %q", want, m.Value())
	}
}

// TestEnterAcceptsHighlightedCommand is the regression for the "/cl"+Enter bug: with
// the palette open and "/clear" highlighted, plain Enter must ACCEPT the highlight and
// submit "/clear" — not the raw partial "/cl" (which the registry rejects as unknown).
func TestEnterAcceptsHighlightedCommand(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/cl")
	out := press(&m, tea.KeyEnter, 0)
	if out.Submit == nil || out.Submit.Text != "/clear" {
		t.Fatalf("Enter on a partial should submit the highlighted command; got %+v", out.Submit)
	}
}

// TestEnterAcceptsCaseInsensitive covers the uppercase entry ("/CL"): the highlight is
// still "/clear", so Enter submits the canonical, lower-case command name.
func TestEnterAcceptsCaseInsensitive(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/CL")
	out := press(&m, tea.KeyEnter, 0)
	if out.Submit == nil || out.Submit.Text != "/clear" {
		t.Fatalf("Enter should normalize case to the highlighted command; got %+v", out.Submit)
	}
}

// TestEnterAcceptPreservesArgs ensures accepting the highlight keeps the args the user
// already typed after the command token ("/inb urgent" → "/inbox urgent").
func TestEnterAcceptPreservesArgs(t *testing.T) {
	m := newModel()
	typeRunes(&m, "/inb urgent")
	out := press(&m, tea.KeyEnter, 0)
	if out.Submit == nil || out.Submit.Text != "/inbox urgent" {
		t.Fatalf("Enter should complete the command token but keep the args; got %+v", out.Submit)
	}
}
