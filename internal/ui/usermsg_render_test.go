package ui

import (
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
