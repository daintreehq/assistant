package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// UserMessageSurface is the per-mode color set for the human's turn card (the YOU
// card: a left accent bar over a subtle fill — NOT a four-sided box). The fill
// must stay subtle (a near-background tint) so the user's message reads as a quiet
// surface, not a loud panel.
//
// A nil Fill means "no background fill" (ansi/none): 16-color backgrounds clash
// unpredictably, and no-color mode leans on the dim bar alone.
type UserMessageSurface struct {
	Bar  color.Color // the left accent bar (▏)
	Text color.Color // the message text (nil => terminal default)
	Fill color.Color // subtle background fill (nil => no fill)
}

// UserMessageSurface resolves the YOU-card colors for this theme's mode.
func (t Theme) UserMessageSurface() UserMessageSurface {
	switch t.Mode {
	case ModeLight:
		return UserMessageSurface{
			Bar:  lipgloss.Color("#94A3B8"),
			Text: lipgloss.Color("#1F2937"),
			Fill: lipgloss.Color("#EAEDF1"),
		}
	case ModeANSI:
		// Gray bar, no fill (16-color backgrounds clash unpredictably).
		return UserMessageSurface{
			Bar:  lipgloss.Color("8"),
			Text: nil,
			Fill: nil,
		}
	case ModeNone:
		// Muted bar, dim text, no fill — color-free.
		return UserMessageSurface{Bar: nil, Text: nil, Fill: nil}
	default: // ModeDark
		return UserMessageSurface{
			Bar:  lipgloss.Color("#6B7280"),
			Text: lipgloss.Color("#E5E7EB"),
			Fill: lipgloss.Color("#181D26"),
		}
	}
}
