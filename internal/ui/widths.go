package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Layout constants. Every committed/live line is
// measured by terminal CELL width (ansi.StringWidth), never runes/bytes, so wide
// glyphs and combining marks can't misplace a caret or overflow into the autowrap
// column.
const (
	// ContentMax caps prose width: a line that runs a maximized window is hard to
	// read, so prose and run cells wrap at a comfortable measure.
	ContentMax = 100
	// LeftPad is the one-column left inset; content never touches the left edge.
	LeftPad = 1
	// LiveChromeMaxWidth caps the compact status rollup so it never wraps to 2 rows.
	LiveChromeMaxWidth = 56
	// maxLiveRows hard-caps the live in-flight tail shown above the composer. Completed
	// blocks (whole paragraphs, closed tool groups) flush to native scrollback as they stream
	// (flush.go) and the still-growing paragraph is WITHHELD (render_turn.go), so the live tail
	// is only an open tool group + the one-line live status — never a streaming prose preview.
	// This cap is a defensive backstop: it keeps the live View short so a flush/commit
	// tea.Println can never dump a tall footer into scrollback (bubbletea#1613).
	maxLiveRows = 8
	// minCockpitRows is the height floor below which the footer collapses to a single
	// "terminal too small" line (see footer()). Below it the fixed bottom band (composer +
	// optional status/approval) can't fit, so rendering it whole would make View() taller
	// than the terminal and scroll a frozen partial copy into native scrollback
	// (bubbletea#1613) — the very corruption the rest of view.go is built to avoid. Nothing
	// is lost: onResize re-commits the transcript and restores the real footer on grow-back.
	minCockpitRows = 4
)

// gutterFor returns the right gutter: 1 normally, raised to 2 when embedded under
// a Daintree xterm (WindowID set) whose overlay scrollbar covers the rightmost
// cells. The gutter reserves the DECAWM autowrap last column so a glyph never
// lands there and triggers a deferred host wrap.
func gutterFor(embedded bool) int {
	if embedded {
		return 2
	}
	return 1
}

// chromeWidth is the usable chrome width for a terminal `columns` wide:
// columns - gutter - LeftPad, floored at 1.
func chromeWidth(columns, gutter int) int {
	w := columns - gutter - LeftPad
	if w < 1 {
		return 1
	}
	return w
}

// contentWidth is min(chromeWidth, ContentMax) — the measure prose wraps at.
func contentWidth(chrome int) int {
	if chrome > ContentMax {
		return ContentMax
	}
	if chrome < 1 {
		return 1
	}
	return chrome
}

// cellWidth measures a string in terminal cells (ANSI-aware).
func cellWidth(s string) int { return ansi.StringWidth(s) }

// stripAnsi removes SGR/escape sequences (used for emptiness checks on styled
// strings so a section with only color codes still counts as blank).
func stripAnsi(s string) string { return ansi.Strip(s) }

// wrapCells hard-wraps a single styled-free line to at most w cells per row,
// breaking on spaces where possible (used for the streaming in-progress prose line,
// which isn't run through glamour's wrapper). Long unbroken tokens are split.
func wrapCells(s string, w int) string {
	if w < 1 || cellWidth(s) <= w {
		return s
	}
	var out []string
	var line strings.Builder
	lineW := 0
	for _, word := range strings.Fields(s) {
		ww := cellWidth(word)
		switch {
		case lineW == 0:
			line.WriteString(word)
			lineW = ww
		case lineW+1+ww <= w:
			line.WriteByte(' ')
			line.WriteString(word)
			lineW += 1 + ww
		default:
			out = append(out, line.String())
			line.Reset()
			line.WriteString(word)
			lineW = ww
		}
		// A single word wider than w: hard-truncate it onto its own row.
		if lineW > w {
			out = append(out, truncateCells(line.String(), w))
			line.Reset()
			lineW = 0
		}
	}
	if line.Len() > 0 {
		out = append(out, line.String())
	}
	return strings.Join(out, "\n")
}

// clockTime formats an epoch-ms timestamp as local HH:MM (the SCHEDULED column).
func clockTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("15:04")
}

// truncateCells trims s to at most w cells, appending an ellipsis when cut. Uses
// ansi.Truncate so SGR and wide runes are respected.
func truncateCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// indentLines prefixes every line of s with `pad` spaces (the LeftPad inset). The
// inset is applied to the FINAL composed block, not per-component, so styling math
// stays edge-agnostic.
func indentLines(s string, pad int) string {
	if pad <= 0 || s == "" {
		return s
	}
	prefix := strings.Repeat(" ", pad)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		// Don't indent an empty line (avoids trailing whitespace in scrollback).
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
