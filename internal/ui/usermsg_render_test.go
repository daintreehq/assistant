package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// uiTheme builds a fully-resolved theme pinned to one mode (palette populated), so
// the styled paths actually emit the mode's hues — mirrors theme.Resolve() in prod.
func uiTheme(mode theme.Mode) theme.Theme {
	t := theme.Resolve()
	t.Mode = mode
	t.Color = theme.PaletteFor(mode)
	return t
}

// userMessageBody returns the body rows of the YOU card (everything after the
// first "YOU" label line).
func userMessageBody(out string) []string {
	lines := strings.Split(out, "\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

// The YOU card is committed to native scrollback; a body row that exceeds the chrome
// width (the `width` passed here) would, after the LeftPad indent, cross the reserved
// autowrap gutter and wrap the frozen line. Assert EVERY row stays within width in
// EVERY mode and at narrow widths — the old `inner >= 10` floor could overflow a
// sub-14-column terminal, which this guards against.
func TestUserMessageRowsWithinWidth(t *testing.T) {
	text := "Give me one interesting fact off the top of your head, not related to the codebase. Respond quickly.\n" +
		"Second line with a verylongunbreakabletokenxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx end."
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		for _, w := range []int{13, 14, 20, 40, 72} {
			out := renderUserMessage(uiTheme(mode), text, w)
			for i, line := range strings.Split(out, "\n") {
				if cw := cellWidth(line); cw > w {
					t.Errorf("mode=%v width=%d row %d exceeds width (cells=%d): %q",
						mode, w, i, cw, stripAnsi(line))
				}
			}
		}
	}
}

// In the fill modes the body is a full-width block: the fill pads every body row to
// exactly width-1 (bar + block) regardless of how short the text is, so the block
// reads as one contiguous rectangle. This pins the "Codex-style block" look.
func TestUserMessageFillBlockIsFullWidth(t *testing.T) {
	const w = 72
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		out := renderUserMessage(uiTheme(mode), "short", w)
		for i, line := range userMessageBody(out) {
			if cw := cellWidth(line); cw != w-1 {
				t.Errorf("mode=%v: fill body row %d width=%d, want %d (full block)", mode, i, cw, w-1)
			}
		}
	}
}

// The fill block must actually emit a background SGR in the color modes, and the
// no-background modes (ansi/none) must fall back to the bar-only path with NO
// background. render_views_ported_test.go strips ANSI, so it can't catch this.
func TestUserMessageBackgroundByMode(t *testing.T) {
	const text = "hello there"
	hasBG := func(s string) bool {
		// truecolor background, standalone (\x1b[48;…) or combined with a fg (…;48;…).
		return strings.Contains(s, "\x1b[48;") || strings.Contains(s, ";48;")
	}
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		body := strings.Join(userMessageBody(renderUserMessage(uiTheme(mode), text, 40)), "\n")
		if !hasBG(body) {
			t.Errorf("mode=%v: expected a background fill SGR in the body, got %q", mode, body)
		}
	}
	for _, mode := range []theme.Mode{theme.ModeANSI, theme.ModeNone} {
		out := renderUserMessage(uiTheme(mode), text, 40)
		if hasBG(out) {
			t.Errorf("mode=%v: expected NO background fill (fallback path), got %q", mode, out)
		}
	}
}

// The "YOU" label is a quiet anchor now — NOT bold (it used to be Dim().Bold(true)).
// The fill carries the "this is yours" signal, so pin the bold removal.
func TestUserMessageLabelNotBold(t *testing.T) {
	isBold := func(s string) bool {
		return strings.Contains(s, "\x1b[1m") || strings.Contains(s, ";1m")
	}
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		out := renderUserMessage(uiTheme(mode), "hi", 40)
		label := strings.SplitN(out, "\n", 2)[0]
		if !strings.Contains(stripAnsi(label), "YOU") {
			t.Fatalf("mode=%v: first line is not the YOU label: %q", mode, label)
		}
		if isBold(label) {
			t.Errorf("mode=%v: YOU label must not be bold: %q", mode, label)
		}
	}
}

// renderUserMessage must produce a deterministic row count that does NOT depend on
// the theme mode (the wrap width `inner` is mode-independent), so the fill and
// fallback paths stay interchangeable for the flush/seal row-exact prefix contract.
func TestUserMessageRowCountStableAcrossModes(t *testing.T) {
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa\nsecond line here too"
	const w = 30
	want := -1
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		n := lineCount(renderUserMessage(uiTheme(mode), text, w))
		if want < 0 {
			want = n
			continue
		}
		if n != want {
			t.Errorf("row count differs by mode: mode=%v got %d want %d", mode, n, want)
		}
	}
}

// numberedLines builds an n-line message whose lines are individually identifiable
// ("line-01"…"line-NN"), short enough not to wrap at the test widths.
func numberedLines(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("line-%02d", i+1)
	}
	return strings.Join(rows, "\n")
}

// trimRuleRow returns the rendered body row carrying the "N lines hidden" rule, or "".
func trimRuleRow(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(stripAnsi(line), "lines hidden") {
			return line
		}
	}
	return ""
}

// A long paste is collapsed to its head + a "N lines hidden" rule + its tail (favoring
// the tail), in EVERY mode: the head and tail lines survive, the middle is gone, and the
// rule reports the hidden LOGICAL-line count.
func TestUserMessageLongPasteTrimmed(t *testing.T) {
	const total = 50
	text := numberedLines(total)
	hidden := total - userMsgHeadLines - userMsgTailLines // 30
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		plain := stripAnsi(renderUserMessage(uiTheme(mode), text, 72))
		// EVERY line is accounted for: the first head and last tail survive verbatim,
		// the whole middle is gone — so a regression that drops one shown line or leaks
		// one hidden line is caught (not just the endpoints).
		for i := 1; i <= total; i++ {
			line := fmt.Sprintf("line-%02d", i)
			shown := i <= userMsgHeadLines || i > total-userMsgTailLines
			if got := strings.Contains(plain, line); got != shown {
				verb := map[bool]string{true: "shown", false: "hidden"}
				t.Errorf("mode=%v: %s should be %s but present=%v:\n%s",
					mode, line, verb[shown], got, plain)
			}
		}
		if !strings.Contains(plain, fmt.Sprintf("%d lines hidden", hidden)) {
			t.Errorf("mode=%v: rule must report %d lines hidden:\n%s", mode, hidden, plain)
		}
	}
}

// The collapse triggers only when it hides at least 2 lines: a head+tail+1 paste shows
// in full, head+tail+2 collapses (hiding exactly 2).
func TestUserMessageTrimThreshold(t *testing.T) {
	th := uiTheme(theme.ModeDark)
	full := numberedLines(userMsgHeadLines + userMsgTailLines + 1)
	if out := stripAnsi(renderUserMessage(th, full, 72)); strings.Contains(out, "hidden") {
		t.Errorf("a %d-line paste must render in full (no rule):\n%s", userMsgHeadLines+userMsgTailLines+1, out)
	}
	over := numberedLines(userMsgHeadLines + userMsgTailLines + 2)
	if out := stripAnsi(renderUserMessage(th, over, 72)); !strings.Contains(out, "2 lines hidden") {
		t.Errorf("a %d-line paste must hide exactly 2 lines:\n%s", userMsgHeadLines+userMsgTailLines+2, out)
	}
}

// The trim rule is just another body row: it stays within width at every width, is a
// full-width fill block (width-1) carrying the fill bg in the color modes and NO bg in
// the fallback modes, and the trimmed card's row count stays mode-independent (the
// flush/seal prefix contract).
func TestUserMessageTrimmedRowGeometry(t *testing.T) {
	text := numberedLines(60)
	hasBG := func(s string) bool {
		return strings.Contains(s, "\x1b[48;") || strings.Contains(s, ";48;")
	}
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		for _, w := range []int{13, 20, 40, 72} {
			out := renderUserMessage(uiTheme(mode), text, w)
			for i, line := range strings.Split(out, "\n") {
				if cw := cellWidth(line); cw > w {
					t.Errorf("mode=%v width=%d row %d exceeds width (cells=%d): %q",
						mode, w, i, cw, stripAnsi(line))
				}
			}
		}
	}
	// Fill modes: every body row — head, tail, AND the trim rule — is a full-width
	// block (width-1), at narrow widths too (not just at a comfortable width).
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight} {
		for _, w := range []int{13, 20, 40, 72} {
			out := renderUserMessage(uiTheme(mode), text, w)
			for i, line := range userMessageBody(out) {
				if cw := cellWidth(line); cw != w-1 {
					t.Errorf("mode=%v width=%d: fill body row %d width=%d, want %d", mode, w, i, cw, w-1)
				}
			}
		}
		// At a comfortable width the rule renders in full and carries the fill bg.
		rule := trimRuleRow(renderUserMessage(uiTheme(mode), text, 72))
		if rule == "" {
			t.Fatalf("mode=%v: no trim rule row found", mode)
		}
		if !hasBG(rule) {
			t.Errorf("mode=%v: trim rule must carry the fill bg: %q", mode, rule)
		}
	}
	for _, mode := range []theme.Mode{theme.ModeANSI, theme.ModeNone} {
		if rule := trimRuleRow(renderUserMessage(uiTheme(mode), text, 72)); hasBG(rule) {
			t.Errorf("mode=%v: trim rule must have no bg (fallback path): %q", mode, rule)
		}
	}
	want := -1
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		n := lineCount(renderUserMessage(uiTheme(mode), text, 40))
		if want < 0 {
			want = n
			continue
		}
		if n != want {
			t.Errorf("trimmed row count differs by mode: mode=%v got %d want %d", mode, n, want)
		}
	}
}

// A trailing newline is noise — it must not change the card, and in particular must
// not tip a full-length paste over the collapse threshold (renderUserMessage trims
// trailing "\n" before counting logical lines).
func TestUserMessageTrailingNewlineIgnored(t *testing.T) {
	th := uiTheme(theme.ModeDark)
	base := numberedLines(userMsgHeadLines + userMsgTailLines + 1) // exactly at the no-trim boundary
	withNL := renderUserMessage(th, base+"\n", 72)
	without := renderUserMessage(th, base, 72)
	if withNL != without {
		t.Errorf("a trailing newline changed the card:\nwith:\n%s\nwithout:\n%s",
			stripAnsi(withNL), stripAnsi(without))
	}
	if strings.Contains(stripAnsi(withNL), "hidden") {
		t.Errorf("a trailing newline must not trigger trimming:\n%s", stripAnsi(withNL))
	}
}

// The rule dashes come from the theme's glyph set (g.Rule), so in ASCII glyph mode it
// uses '-' and never emits box-drawing — pinning the g.Rule wiring (not a hard-coded ─).
func TestUserMessageHiddenRuleAsciiDash(t *testing.T) {
	t.Setenv("DAINTREE_ASCII", "1") // flips theme.Resolve() to the ASCII glyph set
	th := uiTheme(theme.ModeNone)
	if th.Glyphs.Rule != "-" {
		t.Fatalf("expected the ASCII rule glyph '-', got %q", th.Glyphs.Rule)
	}
	rule := stripAnsi(trimRuleRow(renderUserMessage(th, numberedLines(50), 72)))
	if rule == "" {
		t.Fatal("no trim rule row found in ASCII mode")
	}
	if strings.Contains(rule, "─") {
		t.Errorf("ASCII glyph mode must not emit box-drawing dashes: %q", rule)
	}
	if !strings.Contains(rule, "--") {
		t.Errorf("ASCII rule should use '-' dashes: %q", rule)
	}
}
