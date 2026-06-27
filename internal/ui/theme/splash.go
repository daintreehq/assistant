package theme

import (
	"fmt"
	"image/color"

	"charm.land/lipgloss/v2"
)

// Splash palette. The boot mark uses a small ink palette: brand green for solid
// pixels, one darker mix for anti-aliasing pixels, and the terminal background
// for empty cells. The splash animation itself (frames, timing) is a separate
// component; this file owns only the color choices so theme is the single source
// of brand green.
//
// Endpoints:
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

// SplashColor returns the solid brand green used when a caller wants no row ramp.
func SplashColor() color.Color {
	return lipgloss.Color(splashBaseHex)
}

// SplashRowColor returns the gradient color for row `i` of a `rows`-tall mark.
// It linearly interpolates each channel: t = i/(rows-1), channel = round(top +
// (base-top)*t). Row 0 is the crown (top), the last row is the brand base. With
// rows <= 1 (or i clamped) it returns the top color.
//
// When the theme has no color (ModeNone), the splash should not be tinted; the
// caller may skip tinting, but SplashRowColor still returns a valid color so the
// animation code stays branch-free — the renderer decides whether to apply it.
func SplashRowColor(i, rows int) color.Color {
	c := splashRowRGB(i, rows)
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b))
}

// SplashCoverageColor returns one of the splash's fixed ink colors. Solid cells
// use the darkest brand green; partial cells use one anti-aliasing tint so curved
// edges stay consistent instead of producing a wide color ramp.
func SplashCoverageColor(_, _ int, coverage float64) color.Color {
	if coverage >= 0.995 {
		return SplashColor()
	}
	if coverage < 0 {
		coverage = 0
	}
	if coverage > 1 {
		coverage = 1
	}
	const level = 0.72
	base := splashBase
	ground := rgb{0x07, 0x10, 0x0D}
	c := rgb{
		r: lerp(ground.r, base.r, level),
		g: lerp(ground.g, base.g, level),
		b: lerp(ground.b, base.b, level),
	}
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b))
}

func splashRowRGB(i, rows int) rgb {
	if rows <= 1 {
		return splashTop
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
	return c
}

// lerp interpolates a single channel and rounds to the nearest integer, keeping
// the ramp visually symmetric.
func lerp(from, to int, t float64) int {
	v := float64(from) + (float64(to)-float64(from))*t
	return int(v + 0.5)
}
