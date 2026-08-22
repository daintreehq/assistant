// Package terminal holds the TTY-gated raw escape sequences the attached session writes
// straight to the host terminal, OUTSIDE Bubble Tea's managed render path. These
// are reserved for the two operations that must touch the host's own
// screen+scrollback (the `/clear` wipe and the resize "nuclear redraw"); they are
// NEVER used for ordinary redraw, resize reflow, or per-frame painting (Bubble
// Tea owns that). Every writer is guarded so a non-TTY stdout is a no-op and a
// failed write never propagates — an out-of-band cue must never break the program.
// This is the ONLY path that clears host scrollback.
package terminal

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// HostTerminalClear is the host screen+scrollback wipe:
//
//	\x1b[2J  erase the entire viewport
//	\x1b[3J  erase the SCROLLBACK buffer (the one that actually matters here)
//	\x1b[H   move the cursor home
//
// It deliberately does NOT touch the alternate screen buffer (\x1b[?1049h): the
// attached session lives on the normal buffer so the host keeps wheel/selection/copy-paste.
const HostTerminalClear = "\x1b[2J\x1b[3J\x1b[H"

// WindowTitlePrefix is the OSC-2 set-window-title introducer; the title text and
// the BEL terminator bracket it. (We emit titles via Bubble Tea's View.WindowTitle
// in the live path; this is kept for the exit-time clean-title restore.)
const (
	oscTitleStart = "\x1b]2;"
	bel           = "\x07"
)

// isTTY reports whether w is a terminal we may write raw escapes to. A non-TTY
// (pipe, file, test buffer) must never receive control bytes.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}

// ClearHost writes the host screen+scrollback wipe to w when w is a TTY. It is a
// no-op on a non-TTY and swallows any write error (an out-of-band cue must never
// break the attached session). This is the `/clear` step-2 wipe and the resize-redraw wipe;
// it is the ONLY sanctioned host-scrollback clear.
func ClearHost(w io.Writer) {
	if !isTTY(w) {
		return
	}
	// Best effort: a partial/failed write is harmless and must not surface.
	_, _ = io.WriteString(w, HostTerminalClear)
}

// SetTitle writes an OSC-2 window-title escape to w (TTY-only, errors swallowed).
// The live session sets the title through Bubble Tea's View.WindowTitle; this
// helper restores a clean title on attached session EXIT (Bubble Tea has already released
// the terminal by then, so the title must be written directly).
func SetTitle(w io.Writer, title string) {
	if !isTTY(w) {
		return
	}
	_, _ = io.WriteString(w, oscTitleStart+title+bel)
}

// Bell writes a BEL (\x07) to w when it is a TTY (the fresh-attention ding). The
// live path emits the BEL as part of a Bubble Tea command; this direct helper is
// available for the rare callback that can't route through Update. No-op off-TTY.
func Bell(w io.Writer) {
	if !isTTY(w) {
		return
	}
	_, _ = io.WriteString(w, bel)
}
