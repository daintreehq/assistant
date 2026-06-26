package theme

import (
	"image/color"

	"charm.land/glamour/v2/ansi"
)

// GlamourStyles builds the glamour v2 ansi.StyleConfig for this theme, mirroring
// the semantic styling map:
//
//	body prose      → terminal default foreground (NEVER forced white)
//	headings        → bold body text (NO hue — accent green is reserved for
//	                  Daintree's own voice, the ◆ DAINTREE marker, so the user can
//	                  tell what the assistant DID from headings inside its prose)
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
	hexInfoP := colorHex(t.Color.Info)
	hexMutedP := colorHex(t.Color.Muted)
	hexTextP := colorHex(t.Color.Text) // nil for default fg => no Color field

	zeroMargin := uint(0)
	zeroIndent := uint(0)

	// Table separators: box-drawing when Unicode is on, ASCII otherwise. The
	// renderer width-wraps tables (WithTableWrap) so a SIMPLE table (few columns,
	// short cells — the base prompt enforces this) renders as an aligned grid that
	// fits the narrow cockpit, instead of being flattened to a record list. We set
	// the glyphs only (no Color) so cell text keeps its normal foreground and the
	// separators read as plain dividers.
	colSep, centerSep, rowSep := " │ ", "─┼─", "─"
	if !t.Unicode {
		colSep, centerSep, rowSep = " | ", "-+-", "-"
	}

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

		// Headings: BOLD only, body foreground — NO accent green. Green is Daintree's
		// own voice (the ◆ DAINTREE marker); coloring in-prose markdown headings green
		// blurred "what the assistant said" with "what it is". hexTextP is nil in
		// dark/ansi/none (terminal default) and the near-black in light, so a heading
		// reads as bold body text in every mode. (All levels inherit via Heading.)
		Heading: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)},
		},
		H1: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},
		H2: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},
		H3: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},
		H4: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},
		H5: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},
		H6: ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP, Bold: ptrBool(true)}},

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

		// List bullets/enumeration: muted so structure reads without shouting. The
		// BlockPrefix is the MARKER glamour prints before each item — WITHOUT it glamour
		// renders list items as bare indentation (no "•"), which reads as broken "mystery
		// indentation" (a nested list collapses to orphaned indents with no bullets — the
		// worktree-list bug). "• " for unordered; for ordered, glamour supplies the number
		// as a dynamic prefix and ". " here closes it to "1. ".
		List: ansi.StyleList{
			StyleBlock:  ansi.StyleBlock{StylePrimitive: ansi.StylePrimitive{Color: hexTextP}},
			LevelIndent: 2,
		},
		Item:        ansi.StylePrimitive{Color: hexMutedP, BlockPrefix: "• "},
		Enumeration: ansi.StylePrimitive{Color: hexMutedP, BlockPrefix: ". "},

		// Horizontal rule: muted.
		HorizontalRule: ansi.StylePrimitive{Color: hexMutedP, Faint: ptrBool(true)},

		// Table: an aligned grid with simple separators. Width-wrapping is owned by
		// the renderer (WithTableWrap); the base prompt keeps tables small enough to
		// fit the inline cockpit, so a scoreboard reads as a real table.
		Table: ansi.StyleTable{
			ColumnSeparator: &colSep,
			CenterSeparator: &centerSep,
			RowSeparator:    &rowSep,
		},
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
