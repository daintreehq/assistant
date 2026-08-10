package ui

import (
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/daintreehq/assistant/internal/ui/theme"
)

const (
	// splashViewportReset clears only the visible viewport and homes the cursor.
	// Deliberately omit ESC[3J: startup must preserve the host terminal's native
	// scrollback. Explicit user-requested clear/redraw actions own any scrollback wipe.
	splashViewportReset = "\x1b[2J\x1b[H"
	splashAbortCleanup  = "\x1b[?25h" + splashViewportReset
)

// boot_splash.go plays the startup animation DIRECTLY to the terminal, BEFORE the
// Bubble Tea program starts. This is the idiomatic pattern for an inline (non-alt-
// screen) Bubble Tea app, and it is the fix for a whole class of rendering bugs we hit
// trying to animate the splash through View():
//
//   - A tall, fully-changing View() desyncs BT v2's inline diff renderer (it repaints
//     with relative cursor moves) — the mark smeared across the top of the screen.
//   - Worse, BT v2 DEFERS its cell-buffer resize to the FPS-ticker flush (commit
//     f19cb68, "defer resize and draw until flush"). So tea.Println's insertAbove,
//     which runs synchronously and reads s.cellbuf.Height(), used the STALE tall boot
//     height on the boot→footer hand-off — its erase wiped the footer / masthead
//     (GitHub charmbracelet/bubbletea#1613, #1666).
//
// Playing the animation ourselves and only THEN starting BT — whose View() is from
// then on a small, fixed-height footer — sidesteps every one of those. The terminal
// host owns the splash exactly as it will own scrolling; nothing touches the alt
// screen.
//
// It is a no-op on a non-TTY, when DAINTREE_ASSISTANT_NO_SPLASH is set, or when the
// terminal is too small for the 48×18 mark. Ctrl-C / ctx cancellation aborts cleanly,
// restoring the cursor and leaving a clean screen for BT.
//
// When the full animation completes, handoffFrame is called with the freshly measured
// terminal dimensions and written immediately under synchronized output. That makes the
// last logo frame transition directly to the complete cockpit — masthead, logging badge,
// composer, and pending MCP indicator — instead of exposing Bubble Tea's footer-only
// bootstrap frame while its scrollback commit barrier prepares the masthead. The return
// value reports whether the hand-off was painted and at which dimensions.
//
// The terminal size is re-measured EVERY frame, not once: an embedded pane (the
// Daintree sidebar) can be resized by its host mid-animation — layout hydration,
// project switch-back reveal — and a frame rendered for the old width autowraps in
// the new one, stranding mis-wrapped rows the inline renderer can never repaint.
// Each frame is a full clear+repaint, so rendering against the current width makes
// the animation self-healing; the hand-off frame gets the same treatment.
func playBootSplash(
	ctx context.Context,
	w io.Writer,
	th theme.Theme,
	handoffFrame func(cols, rows int) string,
) (painted bool, paintedCols, paintedRows int) {
	if os.Getenv("DAINTREE_ASSISTANT_NO_SPLASH") != "" {
		return false, 0, 0
	}
	cols, rows, ok := terminalSize(w)
	if !ok || cols <= SplashWidth || rows < SplashHeight+2 {
		return false, 0, 0 // too small / not a real terminal — skip rather than clip the mark
	}

	// stdin is still cooked here (BT hasn't taken the terminal), so a Ctrl-C delivers
	// SIGINT to the process. Catch it (and ctx cancellation) so we always restore the
	// cursor before the program tears down.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	handoffPainted := false
	_, _ = io.WriteString(w, "\x1b[?25l") // hide cursor for the draw
	defer func() {
		if handoffPainted {
			return
		}
		_, _ = io.WriteString(w, splashAbortCleanup) // restore cursor + clean viewport for BT
	}()

	// Frames are paced against ABSOLUTE deadlines from a single start instant, not a
	// fixed post-write delay: a per-frame `time.After(frameDelay)` stacks the render/
	// write/measure time ON TOP of the delay, so the "1s + linger" animation drifted
	// ~6% long on a fast terminal and arbitrarily longer on a slow one. Anchoring each
	// frame at start+i·frameDelay (and the linger at the same origin) keeps the total
	// splash — the boot's loading budget — at its designed duration regardless of
	// terminal write speed; when a write overruns its slot the next frame just paints
	// immediately (the reveal catches up instead of stretching).
	frameDelay := time.Second / time.Duration(splashFPS)
	start := time.Now()
	for i := 0; i < SplashFrames; i++ {
		// Fresh measurement each frame; on a transient measure failure keep the
		// last known size (the fd was a terminal at entry). A pane shrunk below
		// the mark mid-play aborts cleanly — clipping the mark reads as garbage,
		// and the deferred cleanup leaves a clean slate for the cockpit.
		if c, r, mok := terminalSize(w); mok {
			cols, rows = c, r
		}
		if cols <= SplashWidth || rows < SplashHeight+2 {
			return false, 0, 0
		}
		_, _ = io.WriteString(w, renderSplashFrame(th, i, cols))
		if wait := time.Until(start.Add(time.Duration(i+1) * frameDelay)); wait > 0 {
			select {
			case <-sigCtx.Done():
				return false, 0, 0
			case <-time.After(wait):
			}
		} else if sigCtx.Err() != nil {
			return false, 0, 0 // behind schedule: still honor an abort between frames
		}
	}
	// Hold the finished logo (the linger) before handing off to the cockpit; the hold
	// ends at the same absolute origin (start + draw + linger) so the whole splash
	// keeps its designed duration.
	lingerUntil := start.Add(bootSplashDuration())
	if wait := time.Until(lingerUntil); wait > 0 {
		select {
		case <-sigCtx.Done():
			return false, 0, 0
		case <-time.After(wait):
		}
	} else if sigCtx.Err() != nil {
		return false, 0, 0 // behind schedule: still honor an abort before the hand-off
	}
	// Measure once more after the linger, the longest write-quiet window in boot, so
	// Bubble Tea starts from the host's latest dimensions.
	if c, r, mok := terminalSize(w); mok {
		cols, rows = c, r
	}
	frame := ""
	if handoffFrame != nil {
		frame = handoffFrame(cols, rows)
	}
	if frame == "" {
		return false, 0, 0
	}
	// A frame taller than the terminal scrolls while printing, so the absolute
	// cursor park lands on the wrong physical row and Bubble Tea adopts a bad inline
	// origin. Fall back to the ordinary clean-start path for tiny terminals instead.
	if handoffFrameRows(frame) > rows {
		return false, 0, 0
	}
	if n, err := io.WriteString(w, frame); err != nil || n != len(frame) {
		return false, 0, 0
	}
	handoffPainted = true
	return true, cols, rows
}

// bootSplashDuration is the splash's complete visual budget: the quick vector reveal
// plus a short finished-logo hold. MCP discovery begins underneath it, but never
// extends it—the composer must become interactive even when startup reads are slow.
func bootSplashDuration() time.Duration {
	return time.Duration(SplashFrames)*(time.Second/time.Duration(splashFPS)) +
		time.Duration(lingerMs)*time.Millisecond
}

// terminalSize returns the dimensions of a terminal-backed writer.
func terminalSize(w io.Writer) (cols, rows int, ok bool) {
	f, fileOK := w.(*os.File)
	if !fileOK {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(f.Fd())
	if err != nil {
		return 0, 0, false
	}
	return cols, rows, true
}

// handoffFrameRows is the number of physical terminal rows occupied by a frame
// whose content was pre-wrapped for the freshly measured terminal width.
func handoffFrameRows(frame string) int {
	return strings.Count(frame, "\r\n") + 1
}

// bootHandoffFrame renders the first steady cockpit screen before Bubble Tea starts.
// It uses the production masthead and footer renderers, then parks the physical cursor
// at the footer origin. Bubble Tea therefore adopts only the short footer as its live
// region while the masthead already lives above it in native terminal scrollback.
func (m Model) bootHandoffFrame() string {
	header := m.headerBlock().Rendered
	footer := m.View().Content
	return renderBootHandoffFrame(header, footer)
}

func renderBootHandoffFrame(header, footer string) string {
	if header == "" && footer == "" {
		return ""
	}
	footerTop := 1
	if header != "" {
		footerTop = lineCount(header) + 1
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h") // begin synchronized update
	b.WriteString("\x1b[?25l")   // keep cursor hidden across the BT hand-off
	b.WriteString(splashViewportReset)
	if header != "" {
		b.WriteString(toCRLF(header))
		b.WriteString("\r\n")
	}
	b.WriteString(toCRLF(footer))
	b.WriteString("\x1b[")
	b.WriteString(itoa(footerTop))
	b.WriteString(";1H")
	b.WriteString("\x1b[?2026l") // end synchronized update
	return b.String()
}

func toCRLF(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// renderSplashFrame builds one full frame: a synchronized-output update (so the per-
// frame clear never flickers), the screen cleared + homed, then the mark centered
// horizontally under a 2-row top margin with each visible row tinted by the green
// gradient.
func renderSplashFrame(th theme.Theme, idx, cols int) string {
	if idx < 0 {
		idx = 0
	}
	if idx >= SplashFrames {
		idx = SplashFrames - 1
	}
	avail := cols - 1 // one shy of the edge so the mark never hits the autowrap column
	var b strings.Builder
	b.WriteString("\x1b[?2026h") // begin synchronized update (no flicker)
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\r\n\r\n") // marginTop={2}
	for i, cells := range splashFrameRows(idx) {
		line := splashCellsPlain(cells)
		styled := splashStyledCells(th, i, cells)
		b.WriteString(centerLine(styled, line, avail))
		b.WriteString("\r\n")
	}
	b.WriteString("\x1b[?2026l") // end synchronized update
	return b.String()
}
