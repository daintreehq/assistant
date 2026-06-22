package theme

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
)

// fakeEnv builds an envLookup from a map (present iff the key is in the map).
func fakeEnv(m map[string]string) envLookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestResolveModeFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Mode
	}{
		{"default dark", map[string]string{}, ModeDark},
		{"explicit light", map[string]string{"DAINTREE_THEME": "light"}, ModeLight},
		{"explicit ansi", map[string]string{"DAINTREE_THEME": "ansi"}, ModeANSI},
		{"explicit none", map[string]string{"DAINTREE_THEME": "none"}, ModeNone},
		{"alias var", map[string]string{"DAINTREE_TERMINAL_THEME": "light"}, ModeLight},
		{"NO_COLOR empty wins", map[string]string{"NO_COLOR": "", "DAINTREE_THEME": "dark"}, ModeNone},
		{"NO_COLOR value wins", map[string]string{"NO_COLOR": "1", "DAINTREE_THEME": "light"}, ModeNone},
		{"unknown defaults dark", map[string]string{"DAINTREE_THEME": "neon"}, ModeDark},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveMode(fakeEnv(c.env)); got != c.want {
				t.Fatalf("resolveMode = %v, want %v", got, c.want)
			}
		})
	}
}

func TestModeColorize(t *testing.T) {
	if ModeNone.Colorize() {
		t.Fatal("ModeNone must not colorize")
	}
	for _, m := range []Mode{ModeDark, ModeLight, ModeANSI} {
		if !m.Colorize() {
			t.Fatalf("%v must colorize", m)
		}
	}
}

func TestPaletteNoneIsBlank(t *testing.T) {
	p := PaletteFor(ModeNone)
	if p.Accent != nil || p.Info != nil || p.Danger != nil || p.Text != nil {
		t.Fatal("none palette must have nil color slots")
	}
}

func TestPaletteDarkTextIsTerminalDefault(t *testing.T) {
	// Dark body text must be nil (terminal default) — the never-force-white rule.
	if PaletteFor(ModeDark).Text != nil {
		t.Fatal("dark Text must be nil (terminal default fg)")
	}
	// Light pins a near-black so prose reads on white.
	if PaletteFor(ModeLight).Text == nil {
		t.Fatal("light Text must be pinned (near-black)")
	}
}

func TestUnicodeOK(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no locale => assume utf", map[string]string{}, true},
		{"DAINTREE_ASCII forces ascii", map[string]string{"DAINTREE_ASCII": "1"}, false},
		{"DAINTREE_ASCII empty still forces", map[string]string{"DAINTREE_ASCII": ""}, false},
		{"utf locale", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"non-utf locale", map[string]string{"LANG": "en_US.ISO8859-1"}, false},
		{"C locale", map[string]string{"LC_ALL": "C"}, false},
		{"LC_ALL precedence over LANG", map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unicodeOK(fakeEnv(c.env)); got != c.want {
				t.Fatalf("unicodeOK = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGlyphFallback(t *testing.T) {
	uni := glyphSet(true)
	asc := glyphSet(false)
	if uni.Brand != "◆" || asc.Brand != "#" {
		t.Fatalf("brand glyph map wrong: uni=%q ascii=%q", uni.Brand, asc.Brand)
	}
	if uni.Done != "✓" || asc.Done != "+" {
		t.Fatalf("done glyph map wrong: uni=%q ascii=%q", uni.Done, asc.Done)
	}
	// ASCII branch stand-ins must stay 2 cells wide so tree rows don't shift.
	if len(asc.BranchMid) != 2 || len(asc.BranchLast) != 2 {
		t.Fatalf("ascii branch glyphs must be 2 cells: mid=%q last=%q", asc.BranchMid, asc.BranchLast)
	}
}

func TestSplashGradientEndpoints(t *testing.T) {
	const rows = 18
	top := SplashRowColor(0, rows)
	base := SplashRowColor(rows-1, rows)

	if !sameColor(top, mustHex(splashTopHex)) {
		t.Fatalf("row 0 should be TOP %s, got %v", splashTopHex, top)
	}
	if !sameColor(base, mustHex(splashBaseHex)) {
		t.Fatalf("last row should be BASE %s, got %v", splashBaseHex, base)
	}
	// A middle row must lie strictly between the endpoints on the green channel
	// (top is lighter/higher green, base lower) — proves interpolation runs.
	_, mg, _ := rgbOf(SplashRowColor(rows/2, rows))
	_, tg, _ := rgbOf(top)
	_, bg, _ := rgbOf(base)
	if !(mg < tg && mg > bg) {
		t.Fatalf("mid green %d not between base %d and top %d", mg, bg, tg)
	}
}

func TestResolveTheme(t *testing.T) {
	th := resolveWith(fakeEnv(map[string]string{"DAINTREE_THEME": "ansi", "DAINTREE_ASCII": "1"}))
	if th.Mode != ModeANSI {
		t.Fatalf("mode = %v, want ansi", th.Mode)
	}
	if th.Unicode {
		t.Fatal("DAINTREE_ASCII should disable unicode")
	}
	if th.Glyphs.Brand != "#" {
		t.Fatalf("ascii brand glyph expected, got %q", th.Glyphs.Brand)
	}
}

// --- color helpers for the splash test ---

func rgbOf(c color.Color) (uint8, uint8, uint8) {
	r, g, b, _ := c.RGBA()
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab := rgbOf(a)
	br, bg, bb := rgbOf(b)
	return ar == br && ag == bg && ab == bb
}

func mustHex(h string) color.Color { return lipgloss.Color(h) }
