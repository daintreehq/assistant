package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Style helpers turn the semantic palette into ready lipgloss styles. Components
// call these instead of reaching for raw hexes so the "color = meaning" rule is
// enforced in one place. In ModeNone every color slot is empty, so Foreground on
// an empty color is a no-op — the attribute-only styles (Bold/Faint) still apply.

// fg builds a foreground style from a palette color, skipping the color set when
// the slot is nil (ModeNone, or terminal-default Text) so we never force a hue.
func fg(c color.Color) lipgloss.Style {
	s := lipgloss.NewStyle()
	if c != nil {
		s = s.Foreground(c)
	}
	return s
}

// Accent — Daintree's own voice (bold accent green): the ◆ DAINTREE marker.
func (t Theme) Accent() lipgloss.Style { return fg(t.Color.Accent).Bold(true) }

// Brand — the splash/masthead brand green (bold). Startup chrome only.
func (t Theme) Brand() lipgloss.Style { return fg(t.Color.Brand).Bold(true) }

// Info — code / links / informational cues (cyan).
func (t Theme) Info() lipgloss.Style { return fg(t.Color.Info) }

// Warning — soft alert (yellow).
func (t Theme) Warning() lipgloss.Style { return fg(t.Color.Warning) }

// Danger — failure / destructive (red).
func (t Theme) Danger() lipgloss.Style { return fg(t.Color.Danger) }

// Blocked — waiting-on-approval / gated (violet).
func (t Theme) Blocked() lipgloss.Style { return fg(t.Color.Blocked) }

// Muted — dim chrome (separators, labels, durations): the muted GRAY hue alone. NO Faint —
// the gray already carries the dimness, and compounding the SGR-2 faint atop the 16-color
// bright-black slot (ModeANSI) renders near-invisible (an accessibility regression).
func (t Theme) Muted() lipgloss.Style { return fg(t.Color.Muted) }

// Dim — secondary text. In any color-capable mode it uses the muted GRAY hue (a real
// luminance, so it stays legible even on terminals that IGNORE the unreliable SGR-2 faint
// attribute); only in ModeNone, where there is no hue to lean on, does it fall back to
// attribute-only Faint so some dimness still reads.
func (t Theme) Dim() lipgloss.Style {
	if t.Color.Muted != nil {
		return fg(t.Color.Muted)
	}
	return lipgloss.NewStyle().Faint(true)
}

// Body — the body foreground. EMPTY Text => terminal default (the never-force-white
// rule); only the light theme pins a near-black via Color.Text.
func (t Theme) Body() lipgloss.Style { return fg(t.Color.Text) }
