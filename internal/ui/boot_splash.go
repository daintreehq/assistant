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

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

const (
	// splashViewportReset clears only the visible viewport and homes the cursor.
	// Deliberately omit ESC[3J: startup must preserve the host terminal's native
	// scrollback. Explicit user-requested clear/redraw actions own any scrollback wipe.
	splashViewportReset = "\x1b[2J\x1b[H"
	splashAbortCleanup  = "\x1b[?25h" + splashViewportReset
	// Daintree's renderer reconciliation watchdog runs every three seconds. On a
	// cold assistant mount it can discover that xterm's provisional grid differs
	// from the already-correct PTY grid, then reflow xterm without sending SIGWINCH
	// (the PTY resize is deduplicated because its dimensions did not change). An
	// inline renderer cannot observe that cursor move. Keep the directly-painted
	// splash up through the first sweep, plus a small scheduling margin, so Bubble
	// Tea establishes its relative cursor origin only after host geometry settles.
	embeddedHostGeometrySettle = 3500 * time.Millisecond
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
// When the full animation completes, onComplete is called with the freshly measured
// terminal dimensions, then the viewport is cleared for Bubble Tea. The cockpit is not
// pre-painted here: the embedded Daintree host can reflow xterm during project load, and
// an absolute cursor park would leave Bubble Tea tracking a stale inline origin.
//
// The terminal size is re-measured EVERY frame, not once: an embedded pane (the
// Daintree sidebar) can be resized by its host mid-animation — layout hydration,
// project switch-back reveal — and a frame rendered for the old width autowraps in
// the new one, stranding mis-wrapped rows the inline renderer can never repaint.
// Each frame is a full clear+repaint, so rendering against the current width makes
// the animation self-healing.
func playBootSplash(
	ctx context.Context,
	w io.Writer,
	th theme.Theme,
	onComplete func(cols, rows int),
) {
	if os.Getenv("DAINTREE_ASSISTANT_NO_SPLASH") != "" {
		return
	}
	cols, rows, ok := terminalSize(w)
	if !ok || cols <= SplashWidth || rows < SplashHeight+2 {
		return // too small / not a real terminal — skip rather than clip the mark
	}

	// stdin is still cooked here (BT hasn't taken the terminal), so a Ctrl-C delivers
	// SIGINT to the process. Catch it (and ctx cancellation) so we always restore the
	// cursor before the program tears down.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_, _ = io.WriteString(w, "\x1b[?25l") // hide cursor for the draw
	defer func() { _, _ = io.WriteString(w, splashAbortCleanup) }()

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
			return
		}
		_, _ = io.WriteString(w, renderSplashFrame(th, i, cols))
		if wait := time.Until(start.Add(time.Duration(i+1) * frameDelay)); wait > 0 {
			select {
			case <-sigCtx.Done():
				return
			case <-time.After(wait):
			}
		} else if sigCtx.Err() != nil {
			return // behind schedule: still honor an abort between frames
		}
	}
	// Hold the finished logo (the linger) before handing off to the cockpit; the hold
	// ends at the same absolute origin (start + draw + linger) so the whole splash
	// keeps its designed duration.
	lingerUntil := start.Add(bootSplashDuration())
	if wait := time.Until(lingerUntil); wait > 0 {
		select {
		case <-sigCtx.Done():
			return
		case <-time.After(wait):
		}
	} else if sigCtx.Err() != nil {
		return // behind schedule: still honor an abort before splash completion
	}
	// Measure once more after the linger, the longest write-quiet window in boot, so
	// Bubble Tea starts from the host's latest dimensions.
	if c, r, mok := terminalSize(w); mok {
		cols, rows = c, r
	}
	if onComplete != nil {
		onComplete(cols, rows)
	}
}

// bootSplashDuration keeps ordinary terminal startup at the designed ~740ms,
// extending only launches inside Daintree's embedded terminal. DAINTREE_WINDOW_ID
// is injected by that host and already controls other embedded-pane behavior.
func bootSplashDuration() time.Duration {
	d := time.Duration(SplashFrames)*(time.Second/time.Duration(splashFPS)) +
		time.Duration(lingerMs)*time.Millisecond
	if os.Getenv("DAINTREE_WINDOW_ID") != "" && d < embeddedHostGeometrySettle {
		return embeddedHostGeometrySettle
	}
	return d
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
