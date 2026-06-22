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

// splash.go is the boot splash overlay. It is a
// transient overlay that dismisses on its OWN timer and NEVER gates input — the
// composer is already interactive while the splash shows. It steps a frame index on a
// ~28fps tick (one frame every 1000/28 ≈ 35.7ms), holds the finished logo for lingerMs,
// then emits SplashDoneMsg ONCE. When the terminal is too narrow (columns <= SplashWidth)
// it renders nothing and fires done immediately (a clipped logo looks broken).
//
// The frames are the SPLASH_FRAMES in splash_frames.go, a pre-rendered 20-step
// reveal — trunk grows up, then roots, then the canopy arch draws
// on 45° diagonals to the centre peak. We do NOT re-derive the art at runtime; we just
// step the index and tint each of the 18 rows with the green gradient (theme.SplashRowColor)
// so the canopy reads lit and the trunk grounded.
//
// INLINE SIZING: the cockpit renders into the terminal's
// MAIN buffer, so the splash draws at its NATURAL height — two blank lines down for
// breathing room, NOT vertically centered / fullscreen — and is HORIZONTALLY centered
// across columns-1 so the mark's right edge never reaches the autowrap column.

const (
	// SplashWidth/Height are the splash mark's dimensions; SplashFrames is the
	// animation length (len(splashFrames)).
	SplashWidth  = 48
	SplashHeight = 18
	SplashFrames = 20
	// splashFPS sets the per-frame tick (28fps over 20 frames ≈ 0.7s draw); lingerMs holds
	// the completed logo before signalling done (~0.42s) so it doesn't vanish the instant
	// the draw lands. ~0.7s draw + ~0.42s hold ≈ 1.1s total.
	splashFPS = 28
	lingerMs  = 420
)

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

// commitArmCmd fires CommitArmMsg after one short delay, arming the first scrollback
// commit once Bubble Tea has flushed the short footer (see scheduleCommit).
func commitArmCmd() tea.Cmd {
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return CommitArmMsg{} })
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

// view renders the current frame centered within columns-1 (one column shy of the edge
// so the right edge never hits the autowrap column), after 2 blank top lines. The frame
// is the verbatim pre-rendered art — every row already drawn for this step — and each of
// its SplashHeight rows is tinted by the gradient (crown→base) for the depth cue.
//
// rows is the terminal height: the splash is the WHOLE live View during boot, and the
// Bubble Tea inline renderer can only repaint a View that fits the viewport — a block
// taller than the terminal overflows and the cursor-up math drifts (the mark "offsets
// and goes weird" as it grows). So when the terminal is too short for the 2 margin rows
// + the 18-row mark we render nothing (the boot gates still settle on their timers).
func (s splashModel) view(th theme.Theme, columns, rows int) string {
	if s.tooSmall || s.done {
		return ""
	}
	if rows > 0 && rows < SplashHeight+2 {
		return ""
	}
	avail := columns - 1
	if avail < SplashWidth {
		avail = SplashWidth
	}
	// Index the pre-rendered frames; clamp defensively to the last (complete) frame.
	idx := s.frame
	if idx < 0 {
		idx = 0
	}
	if idx >= len(splashFrames) {
		idx = len(splashFrames) - 1
	}
	lines := strings.Split(splashFrames[idx], "\n")
	var b strings.Builder
	b.WriteString("\n\n") // 2 blank lines of top breathing room (TSX marginTop={2})
	for i, line := range lines {
		// Tint per-row with the gradient over the full SplashHeight (t = i/(rows-1)),
		// matching StartupSplash's rowColor(i, SPLASH_HEIGHT). Skipped in ModeNone — the
		// color stays valid but body styling carries no hue there.
		styled := line
		if th.Mode.Colorize() {
			styled = lipglossFg(th, theme.SplashRowColor(i, SplashHeight), line)
		}
		b.WriteString(centerLine(styled, line, avail))
		b.WriteByte('\n')
	}
	return b.String()
}

// bootView renders the splash as the ENTIRE boot screen: a fixed block exactly rows-1
// lines tall (the 18-row mark under a 2-row top margin, blank-filled to height). This
// is the load-bearing fix for the inline-renderer drift: Bubble Tea's standard renderer
// repaints the View by moving the cursor up by the PREVIOUS view's line count — if the
// View is short and printed at the bottom, each ~28fps frame scrolls the terminal and
// the up-count desyncs, smearing the mark across the top of the screen. Giving boot a
// stable, screen-filling block means the region never scrolls and every frame repaints
// in place. The hand-off (completeBoot → hostClearCmd) wipes this and drops to the
// short live footer.
//
// It always renders the current frame while booting (ignoring s.done) so the height
// stays stable right up to the hand-off; a too-small/too-narrow terminal yields a
// full-height blank block (still stable) instead of a clipped, broken mark.
func (s splashModel) bootView(th theme.Theme, columns, rows int) string {
	// The boot view is a CONSTANT-height block (2-row top margin + the 18-row mark) —
	// NOT full-screen. A fixed height keeps the live region stable so the per-frame
	// ClearScreen repaints cleanly, while staying small enough that the boot hand-off
	// (masthead → scrollback, then the short footer) does not shove the masthead off
	// the top of the screen.
	const height = SplashHeight + 2
	// Too short a terminal for the natural block: keep a stable, non-scrolling blank
	// block (the boot gates still settle on their own timers) rather than a clipped mark.
	if rows > 0 && rows-1 < height {
		h := rows - 1
		if h < 1 {
			h = 1
		}
		return strings.Repeat("\n", h-1)
	}
	if s.tooSmall || columns <= SplashWidth {
		return strings.Repeat("\n", height-1) // stable blank block (no clipped mark)
	}
	avail := columns - 1
	idx := s.frame
	if idx < 0 {
		idx = 0
	}
	if idx >= len(splashFrames) {
		idx = len(splashFrames) - 1
	}
	lines := make([]string, 0, height)
	lines = append(lines, "", "") // marginTop={2}
	for i, line := range strings.Split(splashFrames[idx], "\n") {
		styled := line
		if th.Mode.Colorize() {
			styled = lipglossFg(th, theme.SplashRowColor(i, SplashHeight), line)
		}
		lines = append(lines, centerLine(styled, line, avail))
	}
	return strings.Join(lines, "\n") // exactly `height` lines
}

// centerLine centers `styled` (whose visible width equals plain's) within w cells.
func centerLine(styled, plain string, w int) string {
	pad := (w - cellWidth(plain)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + styled
}
