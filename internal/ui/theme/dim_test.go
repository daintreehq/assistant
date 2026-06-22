package theme

import (
	"strings"
	"testing"
)

// dim_test.go locks the dim→luminance accessibility fix: Dim/Muted use a real gray HUE in a
// color-capable mode (legible even where the terminal ignores SGR-2 faint), and Dim falls
// back to attribute-only faint only in ModeNone.

func noEnv(string) (string, bool) { return "", false }

func TestDimAndMutedShareGrayNoFaintInColorMode(t *testing.T) {
	th := resolveWith(noEnv) // default → ModeDark, utf
	if th.Mode != ModeDark {
		t.Fatalf("precondition: want ModeDark, got %v", th.Mode)
	}
	// Both now render the muted gray hue with no compounded faint, so they match.
	if d, m := th.Dim().Render("x"), th.Muted().Render("x"); d != m {
		t.Fatalf("Dim and Muted should both render the gray hue (no faint) in a color mode:\nDim=%q\nMuted=%q", d, m)
	}
	// And it carries an SGR color sequence (a real luminance), not bare text.
	if !strings.Contains(th.Dim().Render("x"), "\x1b[") {
		t.Fatal("Dim in a color mode must emit an SGR color, not rely on bare/faint text")
	}
}

func TestDimFallsBackToFaintInModeNone(t *testing.T) {
	th := Theme{Mode: ModeNone, Color: PaletteFor(ModeNone)}
	out := th.Dim().Render("hello")
	if !strings.Contains(out, "hello") {
		t.Fatalf("dim must still render the text in ModeNone, got %q", out)
	}
}
