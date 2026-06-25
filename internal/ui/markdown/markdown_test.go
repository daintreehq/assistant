package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// darkTheme is a fully-resolved dark theme (palette populated), so colored
// styles actually emit hues — mirrors what theme.Resolve() yields in prod.
func darkTheme() theme.Theme {
	t := theme.Resolve()
	t.Mode = theme.ModeDark
	t.Color = theme.PaletteFor(theme.ModeDark)
	return t
}

func noColorTheme() theme.Theme {
	t := theme.Resolve()
	t.Mode = theme.ModeNone
	t.Color = theme.PaletteFor(theme.ModeNone)
	return t
}

func lightTheme() theme.Theme {
	t := theme.Resolve()
	t.Mode = theme.ModeLight
	t.Color = theme.PaletteFor(theme.ModeLight)
	return t
}

// Headings are BOLD only — never accent green. Green is Daintree's own voice (the
// ◆ DAINTREE marker), so coloring in-prose markdown headings green blurred "what the
// assistant said" with "what it is". Assert the heading carries the bold attribute
// but NOT the accent-green foreground SGR, in both color modes.
func TestHeadingsBoldNotAccent(t *testing.T) {
	// Accent-green truecolor triples (theme.go: dark #6EE7B7, light #047857).
	const darkAccent = "110;231;183"
	const lightAccent = "4;120;87"

	dark := New(darkTheme()).Render("# Heading", 60, false).ANSI
	if !strings.Contains(dark, "\x1b[1m") && !strings.Contains(dark, ";1m") {
		t.Errorf("dark heading should carry the bold SGR: %q", dark)
	}
	if strings.Contains(dark, darkAccent) {
		t.Errorf("dark heading must NOT use the accent-green foreground: %q", dark)
	}

	light := New(lightTheme()).Render("# Heading", 60, false).ANSI
	if !strings.Contains(light, "\x1b[1m") && !strings.Contains(light, ";1m") {
		t.Errorf("light heading should carry the bold SGR: %q", light)
	}
	if strings.Contains(light, lightAccent) {
		t.Errorf("light heading must NOT use the accent-green foreground: %q", light)
	}
}

// TestListMarkersRendered locks the list-bullet fix: without an Item/Enumeration
// BlockPrefix glamour renders list items as bare indentation (no "•"), which reads as
// broken "mystery indentation" — a nested bullet list collapses to orphaned indents. We
// assert real markers appear for unordered, ordered, AND nested lists.
func TestListMarkersRendered(t *testing.T) {
	r := New(noColorTheme())

	unordered := r.Render("- alpha\n- beta", 60, false).Plain
	if !strings.Contains(unordered, "• alpha") || !strings.Contains(unordered, "• beta") {
		t.Errorf("unordered list missing the • marker:\n%q", unordered)
	}

	ordered := r.Render("1. first\n2. second", 60, false).Plain
	if !strings.Contains(ordered, "1. first") || !strings.Contains(ordered, "2. second") {
		t.Errorf("ordered list missing the N. marker:\n%q", ordered)
	}

	// Nested unordered list — the worktree-list shape from the bug report. Each level
	// must carry a bullet, not bare indentation.
	nested := r.Render("- outer:\n  - inner one\n  - inner two", 60, false).Plain
	if c := strings.Count(nested, "•"); c < 3 {
		t.Errorf("nested list should have a bullet per item (>=3 •), got %d:\n%q", c, nested)
	}
}

// TestCacheHit asserts a second render of the same (content,width,theme,expanded)
// returns the cached result and does not grow the cache — the render is pure.
func TestCacheHit(t *testing.T) {
	r := NewWithCacheSize(darkTheme(), 8)
	const src = "# Title\n\nBody **text** with `code`."

	first := r.Render(src, 50, false)
	if r.cache.len() != 1 {
		t.Fatalf("expected 1 cache entry after first render, got %d", r.cache.len())
	}
	second := r.Render(src, 50, false)
	if r.cache.len() != 1 {
		t.Fatalf("cache grew on identical render: got %d entries, want 1", r.cache.len())
	}
	if first.ANSI != second.ANSI || first.Plain != second.Plain {
		t.Fatal("cached render differs from first render")
	}

	// A different width is a different key → a new entry.
	r.Render(src, 30, false)
	if r.cache.len() != 2 {
		t.Fatalf("expected 2 entries after width change, got %d", r.cache.len())
	}
	// A different expanded flag is also a distinct key.
	r.Render(src, 50, true)
	if r.cache.len() != 3 {
		t.Fatalf("expected 3 entries after expanded change, got %d", r.cache.len())
	}
}

// TestCacheEvicts asserts the LRU bound holds (oldest evicted past capacity).
func TestCacheEvicts(t *testing.T) {
	r := NewWithCacheSize(darkTheme(), 2)
	r.Render("a", 40, false)
	r.Render("b", 40, false)
	r.Render("c", 40, false) // evicts "a"
	if got := r.cache.len(); got != 2 {
		t.Fatalf("LRU did not bound: got %d entries, want 2", got)
	}
}

// TestNoColorStripsANSI asserts that with the none theme the ANSI output carries
// NO escape sequences and equals its own plain fallback.
func TestNoColorStripsANSI(t *testing.T) {
	r := New(noColorTheme())
	out := r.Render("# Heading\n\n**bold** and `code` and a [link](http://x).", 60, false)

	if out.ANSI != out.Plain {
		t.Fatalf("no-color: ANSI must equal Plain\nANSI=%q\nPlain=%q", out.ANSI, out.Plain)
	}
	if ansi.Strip(out.ANSI) != out.ANSI {
		t.Fatalf("no-color output still contains ANSI escapes: %q", out.ANSI)
	}
	if !strings.Contains(out.Plain, "Heading") {
		t.Fatalf("no-color output dropped content: %q", out.Plain)
	}
}

// TestDarkColorsPresent is a guard that the colored path actually emits hues, so
// TestNoColorStripsANSI is meaningful (proves stripping isn't trivially true).
func TestDarkColorsPresent(t *testing.T) {
	r := New(darkTheme())
	out := r.Render("# Heading", 60, false)
	if ansi.Strip(out.ANSI) == out.ANSI {
		t.Fatalf("dark render emitted no ANSI escapes (heading should be bold): %q", out.ANSI)
	}
}

// TestWidthWrapByCells asserts every rendered line measures <= width DISPLAY
// CELLS (not runes/bytes), including for wide CJK runes that count as 2 cells.
func TestWidthWrapByCells(t *testing.T) {
	const width = 24
	r := New(darkTheme())

	// A long ASCII paragraph that must wrap.
	long := "the quick brown fox jumps over the lazy dog again and again and again"
	out := r.Render(long, width, false)
	for _, line := range strings.Split(out.Plain, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("ASCII line exceeds %d cells (got %d): %q", width, w, line)
		}
	}

	// Wide CJK: each rune is 2 cells. A run longer than width/2 runes must wrap,
	// and no line may exceed the cell budget — proves cell (not rune) measurement.
	cjk := strings.Repeat("漢", 40) // 40 runes = 80 cells, must wrap at 24 cells
	outCJK := r.Render(cjk, width, false)
	sawWrap := false
	for _, line := range strings.Split(outCJK.Plain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("CJK line exceeds %d cells (got %d): %q", width, w, line)
		}
		sawWrap = true
	}
	if !sawWrap {
		t.Fatal("CJK content produced no measurable lines")
	}
	if len(strings.Split(strings.TrimRight(outCJK.Plain, "\n \t"), "\n")) < 2 {
		t.Fatal("wide CJK run did not wrap by cells (expected multiple lines)")
	}
}

// TestANSIInputStripped asserts injected ANSI in the model's own prose never
// reaches the output (security: strip input before parsing).
func TestANSIInputStripped(t *testing.T) {
	r := New(noColorTheme())
	// Inject a raw red SGR + an OSC-8 link into the "model output".
	malicious := "normal \x1b[31mRED\x1b[0m \x1b]8;;http://evil\x07click\x1b]8;;\x07 text"
	out := r.Render(malicious, 80, false)
	if strings.Contains(out.ANSI, "\x1b") {
		t.Fatalf("injected ANSI survived to output: %q", out.ANSI)
	}
	if !strings.Contains(out.Plain, "RED") || !strings.Contains(out.Plain, "click") {
		t.Fatalf("stripping ate the visible text: %q", out.Plain)
	}
}

// TestEmptyInput asserts whitespace-only prose renders to an empty Rendered (no
// lone hole), per the plain-fallback rule.
func TestEmptyInput(t *testing.T) {
	r := New(darkTheme())
	out := r.Render("   \n\t\n  ", 40, false)
	if out.ANSI != "" || out.Plain != "" {
		t.Fatalf("empty input should render empty, got ANSI=%q Plain=%q", out.ANSI, out.Plain)
	}
}
