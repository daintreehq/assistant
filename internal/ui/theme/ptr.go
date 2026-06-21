package theme

import (
	"fmt"
	"image/color"
)

// Small pointer helpers for building glamour's StyleConfig, whose fields are
// *bool / *uint / *string (so "unset" is distinguishable from a zero value).

func ptrBool(b bool) *bool { return &b }

func ptrUint(u uint) *uint { return &u }

// hexString renders any color.Color to a "#RRGGBB" string. lipgloss.Color of an
// ANSI index (e.g. "2") still satisfies color.Color, so this collapses it to the
// nearest truecolor hex via RGBA — acceptable because glamour re-quantizes to the
// active color profile anyway. The 16-bit RGBA channels are scaled down to 8-bit.
func hexString(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
}
