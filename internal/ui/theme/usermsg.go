package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// UserMessageSurface is the per-mode color set for the human's turn card (the YOU
// card: a left accent bar over a subtle fill — NOT a four-sided box). The fill
// must stay subtle (a near-background tint) so the user's message reads as a quiet
// surface, not a loud panel. The fill is what visually separates the human's own
// words from everything else (especially the bar'd tool tree, which the lone bar
// read too much like) — it turns the message into a contiguous block butting up
// against the bar.
//
// A nil Fill means "no background fill" (ansi/none): 16-color backgrounds clash
// unpredictably, and no-color mode leans on the dim bar alone.
type UserMessageSurface struct {
	Bar   color.Color // the left accent bar (▏)
	Text  color.Color // the message text (nil => terminal default)
	Fill  color.Color // subtle background fill (nil => no fill)
	Label color.Color // the quiet "YOU" anchor (nil => fall back to the dim tone)
}

// UserMessageSurface resolves the YOU-card colors for this theme's mode. The palette
// is deliberately NEUTRAL grey (no blue/slate tint) — a quiet Codex-style surface:
// a grey fill block with slightly-greyed (not stark-white) text and our own left
// accent bar. Label is a notch quieter than Text: the fill carries the "this is
// yours" signal, so the "YOU" anchor recedes (a faint, unbold marker).
func (t Theme) UserMessageSurface() UserMessageSurface {
	switch t.Mode {
	case ModeLight:
		return UserMessageSurface{
			Bar:   lipgloss.Color("#A3A3A3"), // neutral mid-grey line
			Text:  lipgloss.Color("#3A3A3A"), // slightly-greyed near-black
			Fill:  lipgloss.Color("#ECECEC"), // neutral light-grey block
			Label: lipgloss.Color("#7A7A7A"), // faint, readable on a light bg
		}
	case ModeANSI:
		// Gray bar, no fill (16-color backgrounds clash unpredictably).
		return UserMessageSurface{
			Bar:   lipgloss.Color("8"),
			Text:  nil,
			Fill:  nil,
			Label: nil,
		}
	case ModeNone:
		// Muted bar, dim text, no fill — color-free.
		return UserMessageSurface{Bar: nil, Text: nil, Fill: nil, Label: nil}
	default: // ModeDark
		return UserMessageSurface{
			Bar:   lipgloss.Color("#737373"), // neutral grey line on the block
			Text:  lipgloss.Color("#C9C9C9"), // slightly-greyed light grey (not white)
			Fill:  lipgloss.Color("#2B2B2B"), // neutral grey block, clearly visible
			Label: lipgloss.Color("#5C5C5C"), // faint neutral grey, dimmer than the bar
		}
	}
}

// SkillLoadedSurface resolves the colors for the inline "Skill loaded" card — the same
// left-bar-over-fill idiom as the YOU card, but tinted a calm blue/turquoise so a
// server-side capability load reads as its own distinct, quiet event (neither the human's
// words nor a system note). It reuses UserMessageSurface (a generic bar/text/fill/label
// set). As there, a nil Fill means "no background fill" (ansi/none): 16-color backgrounds
// clash unpredictably, and no-color mode leans on the dim bar alone.
func (t Theme) SkillLoadedSurface() UserMessageSurface {
	switch t.Mode {
	case ModeLight:
		return UserMessageSurface{
			Bar:   lipgloss.Color("#3BA5C0"), // teal-blue line
			Text:  lipgloss.Color("#1F3A44"), // dark teal text
			Fill:  lipgloss.Color("#E2F1F8"), // very pale blue block
			Label: lipgloss.Color("#3A7E8C"), // quieter teal anchor
		}
	case ModeANSI:
		// Cyan bar, no fill (16-color backgrounds clash unpredictably).
		return UserMessageSurface{Bar: lipgloss.Color("6"), Text: nil, Fill: nil, Label: nil}
	case ModeNone:
		return UserMessageSurface{Bar: nil, Text: nil, Fill: nil, Label: nil}
	default: // ModeDark
		return UserMessageSurface{
			Bar:   lipgloss.Color("#5EEAD4"), // turquoise line on the block
			Text:  lipgloss.Color("#CBEAEC"), // light cyan-grey, easy on the fill
			Fill:  lipgloss.Color("#173A42"), // subtle dark teal-blue block
			Label: lipgloss.Color("#7FCDD6"), // brighter teal anchor for "Skill loaded"
		}
	}
}
