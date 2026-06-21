package theme

import (
	"image/color"

	"charm.land/glamour/v2/ansi"
)

// GlamourStyles builds the glamour v2 ansi.StyleConfig for this theme, mirroring
// the semantic styling map in ui-transcript.md §8:
//
//	body prose      → terminal default foreground (NEVER forced white)
//	headings        → accent green + bold
//	inline code     → info cyan
//	fenced code     → syntax-highlighted (chroma via glamour); cyan-ish fallback
//	link / url      → info cyan + underline
//	bold/italic/del → bold / italic / strikethrough
//	blockquote      → dim (faint)
//
// In ModeNone every color slot is nil, so the produced config carries NO Color
// fields — glamour then emits attributes only (bold/faint), which the renderer
// additionally strips of any stray SGR. Word-wrap is owned by the renderer
// (WithWordWrap), so margins/indents here stay 0 to keep the inline cockpit tight.
func (t Theme) GlamourStyles() ansi.StyleConfig {
	hexAccentP := colorHex(t.Color.Accent)
	hexInfoP := colorHex(t.Color.Info)
	hexMutedP := colorHex(t.Color.Muted)
	hexTextP := colorHex(t.Color.Text) // nil for default fg => no Color field

	zeroMargin := uint(0)
	zeroIndent := uint(0)

	return ansi.StyleConfig{
		// Document: no surrounding margin/indent — the cockpit owns insets.
		Document: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				// Body foreground: only set when the theme pins one (light mode).
				// Dark/ansi/none leave it nil => terminal default (never white).
				Color: hexTextP,
			},
			Margin: &zeroMargin,
			Indent: &zeroIndent,
		},
		Paragraph: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: hexTextP},
		},
		Text: ansi.StylePrimitive{Color: hexTextP},

		// Blockquote: dim/faint, no hue.
		BlockQuote: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Faint: ptrBool(true)},
			Indent:         ptrUint(1),
		},

		// Headings: accent green + bold (all levels inherit via Heading).
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)},
		},
		H1: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},
		H2: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},
		H3: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},
		H4: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},
		H5: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},
		H6: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexAccentP, Bold: ptrBool(true)}},

		// Emphasis.
		Strong:        ansi.StylePrimitive{Bold: ptrBool(true)},
		Emph:          ansi.StylePrimitive{Italic: ptrBool(true)},
		Strikethrough: ansi.StylePrimitive{CrossedOut: ptrBool(true)},

		// Inline code: info cyan.
		Code: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: hexInfoP},
		},

		// Fenced code block: chroma-highlighted (glamour default). We pin a Theme
		// name so glamour drives chroma; if highlighting fails for an unknown
		// lexer, glamour falls back to the plain block and the renderer's own
		// plain fallback covers the rest.
		CodeBlock: ansi.StyleCodeBlock{
			StyleBlock: ansi.StyleBlock{
				StylePrimitive: ansi.StylePrimitive{Color: hexInfoP},
				Margin:         &zeroMargin,
			},
			Theme: chromaTheme(t.Mode),
		},

		// Links: info cyan + underline. LinkText keeps the visible label cyan.
		Link:     ansi.StylePrimitive{Color: hexInfoP, Underline: ptrBool(true)},
		LinkText: ansi.StylePrimitive{Color: hexInfoP},

		// List bullets/enumeration: muted so structure reads without shouting.
		List: ansi.StyleList{
			StyleBlock:  ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP}},
			LevelIndent: 2,
		},
		Item:        ansi.StylePrimitive{Color: hexMutedP},
		Enumeration: ansi.StylePrimitive{Color: hexMutedP},

		// Horizontal rule: muted.
		HorizontalRule: ansi.StylePrimitive{Color: hexMutedP, Faint: ptrBool(true)},
	}
}

// chromaTheme picks a chroma highlight theme name for fenced code, matched to the
// terminal background. These are real chroma-registry style names (glamour passes
// the string straight to chroma's quick.Highlight); an unknown name would make
// glamour just use chroma's fallback style, never an error. For ModeNone we return
// "" so glamour skips highlighting entirely (no ANSI in code blocks) — the
// no-color contract holds, and the renderer also strips any stray escapes after.
func chromaTheme(m Mode) string {
	switch m {
	case ModeLight:
		return "github" // light chroma style (bundled)
	case ModeNone:
		return "" // no highlighting => no color
	case ModeANSI:
		return "vim" // a 16-color-friendly chroma style
	default:
		return "monokai" // a solid dark chroma style (bundled)
	}
}

// colorHex renders a palette color.Color back to a glamour Color string pointer.
// glamour takes hex/ANSI strings, so we serialize the color to "#RRGGBB" (or an
// ANSI index for the ansi theme). Returns nil for a nil slot so glamour omits the
// field entirely (terminal default / no-color).
func colorHex(c color.Color) *string {
	if c == nil {
		return nil
	}
	s := hexString(c)
	return &s
}
