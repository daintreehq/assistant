package theme

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// Splash gradient palette. The boot mark is drawn in a per-row green gradient that
// implies depth down the trunk: the canopy crown (top) is lighter, the base is the
// brand green. The splash *animation* (frames, timing) is a separate component;
// this file owns only the COLOR ramp so theme is the single source of brand green.
//
// Endpoints ported from ui-input.md §5 / splash/frames.ts:
//
//	TOP  = #8febc4  (canopy crown, lighter)
//	BASE = #36ce94  (brand green, base)
const (
	splashTopHex  = "#8FEBC4"
	splashBaseHex = "#36CE94"
)

// rgb is a simple 8-bit-per-channel color used for the gradient interpolation.
type rgb struct{ r, g, b int }

var (
	splashTop  = rgb{0x8F, 0xEB, 0xC4}
	splashBase = rgb{0x36, 0xCE, 0x94}
)

// SplashRowColor returns the gradient color for row `i` of a `rows`-tall mark.
// It linearly interpolates each channel: t = i/(rows-1), channel = round(top +
// (base-top)*t). Row 0 is the crown (top), the last row is the brand base. With
// rows <= 1 (or i clamped) it returns the top color, matching the TS edge cases.
//
// When the theme has no color (ModeNone), the splash should not be tinted; the
// caller may skip tinting, but SplashRowColor still returns a valid color so the
// animation code stays branch-free — the renderer decides whether to apply it.
func SplashRowColor(i, rows int) color.Color {
	if rows <= 1 {
		return lipgloss.Color(splashTopHex)
	}
	if i < 0 {
		i = 0
	}
	if i >= rows {
		i = rows - 1
	}
	// t in [0,1] across the mark height.
	t := float64(i) / float64(rows-1)
	c := rgb{
		r: lerp(splashTop.r, splashBase.r, t),
		g: lerp(splashTop.g, splashBase.g, t),
		b: lerp(splashTop.b, splashBase.b, t),
	}
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b))
}

// lerp interpolates a single channel and rounds to the nearest integer (matching
// the TS `Math.round`), keeping the ramp visually symmetric to the original.
func lerp(from, to int, t float64) int {
	v := float64(from) + (float64(to)-float64(from))*t
	return int(v + 0.5)
}
