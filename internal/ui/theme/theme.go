// Package theme owns the cockpit's visual vocabulary: the semantic color palette,
// the theme MODE (dark / light / ansi / none), the signature glyph set with ASCII
// fallbacks, and the green splash gradient. It is the shared, dependency-light base
// that every other UI package builds on (the spec calls this `ui/theme`).
//
// Colors carry MEANING, not decoration: accent green = Daintree's own voice, info
// cyan = code/links, warning/danger/blocked = operational severity, muted = chrome.
// The mode is resolved ONCE from the environment (DAINTREE_THEME / NO_COLOR /
// DAINTREE_ASCII + locale) so the whole render tree agrees on whether color and
// unicode are live. Source of truth ported from src/ui/theme.ts.
package theme

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// Mode is the resolved terminal theme. It decides both the palette variant and
// whether color is emitted at all. `none` drops color entirely and leans on the
// concealed/dim markers; `ansi` uses the terminal's own 16-color slots.
type Mode int

const (
	// ModeDark is the default: bright accents on the terminal's dark background.
	ModeDark Mode = iota
	// ModeLight tunes hues for a light terminal background.
	ModeLight
	// ModeANSI restricts to the 16 ANSI palette slots (no truecolor hexes), so a
	// 16-color or remapped-palette terminal renders predictably.
	ModeANSI
	// ModeNone disables color entirely (NO_COLOR / DAINTREE_THEME=none). Spans
	// keep their bold/dim attributes but emit no hue.
	ModeNone
)

// String returns the canonical lowercase name (mirrors DAINTREE_THEME values).
func (m Mode) String() string {
	switch m {
	case ModeLight:
		return "light"
	case ModeANSI:
		return "ansi"
	case ModeNone:
		return "none"
	default:
		return "dark"
	}
}

// Colorize reports whether this mode emits any color. Markdown and styled spans
// gate on this up-front: when false, the output is stripped of all SGR color.
func (m Mode) Colorize() bool { return m != ModeNone }

// envLookup is os.LookupEnv-shaped: it returns the value AND whether the key is
// present. Presence matters for NO_COLOR (any value, including "", disables
// color). All env-reading helpers take one so tests can inject a fake process
// env without touching os.
type envLookup func(key string) (string, bool)

// osLookup wraps os.LookupEnv as an envLookup (production wiring).
func osLookup(key string) (string, bool) { return os.LookupEnv(key) }

// resolveMode is the env → Mode decision. NO_COLOR wins over everything (the
// no-color.org contract: ANY value, even empty, disables color). Otherwise
// DAINTREE_THEME / DAINTREE_TERMINAL_THEME selects the variant; unknown/empty
// defaults to dark.
func resolveMode(env envLookup) Mode {
	// NO_COLOR: presence alone (even empty) forces no-color.
	if _, ok := env("NO_COLOR"); ok {
		return ModeNone
	}
	raw, _ := env("DAINTREE_THEME")
	if raw == "" {
		raw, _ = env("DAINTREE_TERMINAL_THEME")
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "light":
		return ModeLight
	case "ansi":
		return ModeANSI
	case "none":
		return ModeNone
	default:
		return ModeDark
	}
}

// ResolveMode reads the process environment to pick the active theme mode.
func ResolveMode() Mode { return resolveMode(osLookup) }

// Palette is the resolved semantic color set for a Mode. Each field is a
// color.Color (built via lipgloss.Color), or nil when the slot has no hue — nil
// means "terminal default" (Text) or "no color at all" (ModeNone). Consumers
// build lipgloss styles from these; they never hardcode hexes elsewhere.
type Palette struct {
	// Accent is Daintree's own voice (the ◆ DAINTREE marker, headings).
	Accent color.Color
	// Brand is the deeper splash/masthead green (used only at startup).
	Brand color.Color
	// Info is code / links / informational cues (cyan).
	Info color.Color
	// Warning is a soft alert (yellow).
	Warning color.Color
	// Danger is a failure / destructive cue (red).
	Danger color.Color
	// Blocked is a waiting-on-approval / gated cue (violet).
	Blocked color.Color
	// Muted is dim chrome (separators, labels, durations).
	Muted color.Color
	// Text is the body foreground. NIL => terminal default foreground (the
	// "never force white" rule); only the light theme pins a near-black.
	Text color.Color
}

// Truecolor hexes (dark theme baseline). Mirrors theme.ts `ui.color`.
const (
	hexAccent  = "#6EE7B7" // accent green
	hexBrand   = "#36CE94" // brand green (masthead / splash base)
	hexInfo    = "#67E8F9" // info cyan
	hexWarning = "#F6C85F" // warning yellow
	hexDanger  = "#FB7185" // danger red
	hexBlocked = "#C4B5FD" // blocked violet
	hexMuted   = "#9CA3AF" // muted gray
)

// PaletteFor builds the Palette for a Mode. Exported so callers (and tests) can
// assemble a Theme with a populated palette without re-deriving the hexes.
func PaletteFor(m Mode) Palette {
	switch m {
	case ModeNone:
		// No color: every slot is empty so styles emit attributes only (bold/dim),
		// never a hue. Text stays terminal-default.
		return Palette{}
	case ModeANSI:
		// 16-color slots by ANSI index (lipgloss.Color accepts "1".."15"). Chosen
		// to echo the truecolor meaning: green=2, cyan=6, yellow=3, red=1,
		// magenta=5, bright-black=8 for muted.
		return Palette{
			Accent:  lipgloss.Color("2"),
			Brand:   lipgloss.Color("2"),
			Info:    lipgloss.Color("6"),
			Warning: lipgloss.Color("3"),
			Danger:  lipgloss.Color("1"),
			Blocked: lipgloss.Color("5"),
			Muted:   lipgloss.Color("8"),
			Text:    nil, // terminal default
		}
	case ModeLight:
		// Light terminal: keep semantic hues but darken so they read on white.
		return Palette{
			Accent:  lipgloss.Color("#047857"),
			Brand:   lipgloss.Color("#059669"),
			Info:    lipgloss.Color("#0E7490"),
			Warning: lipgloss.Color("#B45309"),
			Danger:  lipgloss.Color("#BE123C"),
			Blocked: lipgloss.Color("#6D28D9"),
			Muted:   lipgloss.Color("#6B7280"),
			Text:    lipgloss.Color("#1F2937"), // near-black on light bg
		}
	default: // ModeDark
		return Palette{
			Accent:  lipgloss.Color(hexAccent),
			Brand:   lipgloss.Color(hexBrand),
			Info:    lipgloss.Color(hexInfo),
			Warning: lipgloss.Color(hexWarning),
			Danger:  lipgloss.Color(hexDanger),
			Blocked: lipgloss.Color(hexBlocked),
			Muted:   lipgloss.Color(hexMuted),
			Text:    nil, // terminal default foreground (never forced white)
		}
	}
}

// Theme bundles the resolved Mode, its Palette, and the active glyph set. It is
// the single value passed through the render tree so every component agrees on
// color + unicode. Build it once (Resolve) and thread it; it is immutable.
type Theme struct {
	Mode    Mode
	Color   Palette
	Glyphs  GlyphSet
	Unicode bool // false => ASCII fallbacks active (DAINTREE_ASCII / non-UTF locale)
}

// Resolve builds the active Theme from the process environment.
func Resolve() Theme { return resolveWith(osLookup) }

// resolveWith is the testable core: it takes an env accessor so tests can inject
// DAINTREE_THEME / NO_COLOR / DAINTREE_ASCII / LANG without touching the process.
func resolveWith(env envLookup) Theme {
	mode := resolveMode(env)
	uni := unicodeOK(env)
	return Theme{
		Mode:    mode,
		Color:   PaletteFor(mode),
		Glyphs:  glyphSet(uni),
		Unicode: uni,
	}
}
