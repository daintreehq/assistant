package ui

import (
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// lipglossFg renders s in the given foreground color (the splash gradient tint).
func lipglossFg(_ theme.Theme, c color.Color, s string) string {
	if c == nil {
		return s
	}
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

// splash.go is the boot splash overlay (ui-input.md §5). It is a transient overlay
// that dismisses on its OWN timer and NEVER gates input — the composer is already
// interactive while the splash shows. It steps a frame index on a ~28fps tick, holds
// on the last frame, then emits SplashDoneMsg. When the terminal is too narrow
// (columns <= SplashWidth) it renders nothing and fires done immediately (a clipped
// logo looks broken).
//
// We embed a compact stylized wordmark rather than re-running the Python frame
// generator at runtime; the brand-green per-row gradient (theme.SplashRowColor)
// supplies the "depth down the mark" cue.

const (
	// SplashWidth/Height match the TS constants; SplashFrames is the animation length.
	SplashWidth  = 48
	SplashHeight = 18
	SplashFrames = 20
	// splashFPS sets the per-frame tick; lingerMs holds the last frame before done.
	splashFPS = 28
	lingerMs  = 420
)

// splashArt is the (static) mark drawn at full reveal. Earlier frames reveal a
// growing prefix of its rows so the mark "draws itself in" top-to-bottom.
var splashArt = []string{
	"        ╱╲        ",
	"       ╱  ╲       ",
	"      ╱ ╱╲ ╲      ",
	"     ╱ ╱  ╲ ╲     ",
	"    ╱ ╱ ╱╲ ╲ ╲    ",
	"   ╱ ╱ ╱  ╲ ╲ ╲   ",
	"  ╱_╱ ╱____╲ ╲_╲  ",
	"      ║    ║      ",
	"      ║    ║      ",
	"     D A I N T R E E",
}

// splashModel holds the overlay's frame index + running flag. It is mutated only in
// the root Update (in response to SplashTickMsg).
type splashModel struct {
	frame    int
	done     bool
	tooSmall bool
}

// newSplash builds the overlay; tooSmall short-circuits to an immediate done.
func newSplash(columns int) splashModel {
	return splashModel{tooSmall: columns <= SplashWidth}
}

// tick is the splash tick command (~28fps). The root re-arms it each SplashTickMsg
// until done.
func splashTickCmd() tea.Cmd {
	return tea.Tick(time.Second/time.Duration(splashFPS), func(time.Time) tea.Msg { return SplashTickMsg{} })
}

// lingerCmd fires SplashDoneMsg after the last-frame hold.
func lingerCmd() tea.Cmd {
	return tea.Tick(time.Duration(lingerMs)*time.Millisecond, func(time.Time) tea.Msg { return SplashDoneMsg{} })
}

// advance steps the frame index and returns the next command: another tick while
// drawing, the linger hold on the last frame, or nil when done.
func (s *splashModel) advance() tea.Cmd {
	if s.done || s.tooSmall {
		return nil
	}
	if s.frame < SplashFrames-1 {
		s.frame++
		return splashTickCmd()
	}
	// Reached the last frame: hold, then signal done.
	return lingerCmd()
}

// view renders the splash centered within columns-1 (one column shy of the edge so
// the right edge never hits the autowrap column), after 2 blank top lines. Rows are
// revealed proportional to the frame index and tinted with the green gradient.
func (s splashModel) view(th theme.Theme, columns int) string {
	if s.tooSmall || s.done {
		return ""
	}
	avail := columns - 1
	if avail < SplashWidth {
		avail = SplashWidth
	}
	// Reveal a growing number of rows as the animation progresses.
	rows := len(splashArt)
	reveal := (s.frame + 1) * rows / SplashFrames
	if reveal > rows {
		reveal = rows
	}
	var b strings.Builder
	b.WriteString("\n\n") // 2 blank lines of top breathing room
	for i := 0; i < reveal; i++ {
		line := splashArt[i]
		// Tint per-row with the gradient (skipped implicitly in ModeNone — the color
		// is still valid but body styling carries no hue there).
		styled := line
		if th.Mode.Colorize() {
			styled = lipglossFg(th, theme.SplashRowColor(i, rows), line)
		}
		b.WriteString(centerLine(styled, line, avail))
		b.WriteByte('\n')
	}
	return b.String()
}

// centerLine centers `styled` (whose visible width equals plain's) within w cells.
func centerLine(styled, plain string, w int) string {
	pad := (w - cellWidth(plain)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + styled
}
