package theme

import (
	"strings"
)

// GlyphSet is the signature glyph vocabulary, resolved to either the unicode set
// or a shape-parallel ASCII fallback. Branch/badge alignment depends on the
// fallbacks being width-1 stand-ins (so a tree row doesn't shift), which is why
// each glyph has an explicit ASCII twin rather than being dropped.
//
// Glyphs: brand ◆, activity states (◦ queued, ◌ active, ◷ waiting,
// ✓ done, × failed), the approval diamond ◇, tree branches (├─ └─ │), and the
// inline separators · ▌ ▏.
type GlyphSet struct {
	Brand        string   // ◆ — the DAINTREE marker / brand mark
	Queued       string   // ◦ — activity queued
	Active       string   // ◌ — activity active (spinner base; animated elsewhere)
	Waiting      string   // ◷ — activity waiting (e.g. watcher)
	Approval     string   // ◇ — waiting for approval
	Done         string   // ✓ — activity complete
	Failed       string   // × — activity failed
	BranchMid    string   // ├─ — non-last tree branch
	BranchLast   string   // └─ — last tree branch (square, not the arc ╰)
	Continuation string   // │  — note/continuation prefix
	Bullet       string   // ·  — inline separator / info bullet
	Caret        string   // ▌ — streaming caret
	Bar          string   // ▏ — user-message accent bar (U+258F)
	Rule         string   // ─ — horizontal rule unit
	Spinner      []string // braille spinner frames (active animation)
}

// unicodeGlyphs is the full-fidelity set.
var unicodeGlyphs = GlyphSet{
	Brand:        "◆",
	Queued:       "◦",
	Active:       "◌",
	Waiting:      "◷",
	Approval:     "◇",
	Done:         "✓",
	Failed:       "×",
	BranchMid:    "├─",
	BranchLast:   "└─",
	Continuation: "│ ",
	Bullet:       "·",
	Caret:        "▌",
	Bar:          "▏",
	Rule:         "─",
	// Braille spinner — matches the ⠋ active glyph family in the specs.
	Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
}

// asciiGlyphs is the shape-parallel fallback. Each stand-in keeps row alignment:
// the two-cell branches map to two-cell ASCII, the one-cell badges to one cell.
var asciiGlyphs = GlyphSet{
	Brand:        "#",
	Queued:       "o",
	Active:       "*",
	Waiting:      "~",
	Approval:     "?",
	Done:         "+",
	Failed:       "x",
	BranchMid:    "|-",
	BranchLast:   "`-",
	Continuation: "| ",
	Bullet:       "-",
	Caret:        "|",
	Bar:          "|",
	Rule:         "-",
	// ASCII spinner: a simple cyclic ramp; keep it width-1 so rows don't jump.
	Spinner: []string{"-", "\\", "|", "/"},
}

// glyphSet returns the active set for a unicode-ok flag.
func glyphSet(unicode bool) GlyphSet {
	if unicode {
		return unicodeGlyphs
	}
	return asciiGlyphs
}

// unicodeOK reports whether signature unicode glyphs are safe to emit. It is
// false when DAINTREE_ASCII is set (any value) OR the locale is non-UTF: a
// terminal in a Latin-1 / C locale renders ◆/├─ as mojibake, so we fall back.
//
// Locale check: LC_ALL, then LC_CTYPE, then LANG (POSIX precedence). A value
// that does NOT contain "utf" (case-insensitive) is treated as non-UTF. An
// entirely UNSET locale is treated as UTF-capable (modern default), so we don't
// punish a stripped env that simply forgot to set LANG.
func unicodeOK(env envLookup) bool {
	if _, ok := env("DAINTREE_ASCII"); ok {
		return false
	}
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v, ok := env(key); ok && v != "" {
			// First locale var that is set decides (POSIX precedence).
			return strings.Contains(strings.ToLower(v), "utf")
		}
	}
	// No locale set at all: assume a modern UTF-8 terminal.
	return true
}
