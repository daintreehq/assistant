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
// always restoring the cursor and leaving a clean screen for BT.
func playBootSplash(ctx context.Context, w io.Writer, th theme.Theme) {
	if os.Getenv("DAINTREE_ASSISTANT_NO_SPLASH") != "" {
		return
	}
	f, ok := w.(*os.File)
	if !ok {
		return
	}
	cols, rows, err := term.GetSize(f.Fd())
	if err != nil || cols <= SplashWidth || rows < SplashHeight+2 {
		return // too small / not a real terminal — skip rather than clip the mark
	}

	// stdin is still cooked here (BT hasn't taken the terminal), so a Ctrl-C delivers
	// SIGINT to the process. Catch it (and ctx cancellation) so we always restore the
	// cursor before the program tears down.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	_, _ = io.WriteString(w, "\x1b[?25l")                    // hide cursor for the draw
	defer io.WriteString(w, "\x1b[?25h\x1b[2J\x1b[3J\x1b[H") // restore cursor + clean slate for BT

	frameDelay := time.Second / time.Duration(splashFPS)
	for i := range splashFrames {
		_, _ = io.WriteString(w, renderSplashFrame(th, i, cols))
		select {
		case <-sigCtx.Done():
			return
		case <-time.After(frameDelay):
		}
	}
	// Hold the finished logo (the linger) before handing off to the cockpit.
	select {
	case <-sigCtx.Done():
	case <-time.After(time.Duration(lingerMs) * time.Millisecond):
	}
}

// renderSplashFrame builds one full frame: a synchronized-output update (so the per-
// frame clear never flickers), the screen cleared + homed, then the mark centered
// horizontally under a 2-row top margin with each row tinted by the green gradient.
func renderSplashFrame(th theme.Theme, idx, cols int) string {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(splashFrames) {
		idx = len(splashFrames) - 1
	}
	avail := cols - 1 // one shy of the edge so the mark never hits the autowrap column
	var b strings.Builder
	b.WriteString("\x1b[?2026h") // begin synchronized update (no flicker)
	b.WriteString("\x1b[2J\x1b[H")
	b.WriteString("\r\n\r\n") // marginTop={2}
	for i, line := range strings.Split(splashFrames[idx], "\n") {
		styled := line
		if th.Mode.Colorize() {
			styled = lipglossFg(th, theme.SplashRowColor(i, SplashHeight), line)
		}
		b.WriteString(centerLine(styled, line, avail))
		b.WriteString("\r\n")
	}
	b.WriteString("\x1b[?2026l") // end synchronized update
	return b.String()
}
