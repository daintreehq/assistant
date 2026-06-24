// Package composer is the dedicated cockpit input editor: an explicit
// buffer/cursor/kill-ring/history model that satisfies the full key contract. It
// deliberately does NOT wrap Bubbles' textarea —
// the contract (logical-line arrow nav with column memory + history recall at
// line edges, a single-slot kill-ring with Ctrl-Y yank, the trailing-backslash
// newline fallback, modifier+Enter newline, verbatim bracketed paste, slash
// palette + Tab completion, "Esc clears / Esc-empty-while-busy cancels") is more
// than the stock widget can express. It reuses only Bubbles' key/help primitives
// (keymap.go) and the shared theme package.
package composer

import (
	"strings"
	"unicode"
)

// All offsets in this package are RUNE offsets into the buffer, not byte
// offsets. TS indexed by UTF-16 code unit; Go iterates runes so multi-byte
// input, paths, and pasted Unicode never corrupt the cursor. We materialize the
// buffer as a []rune at every edit and work
// in rune space, converting back to string only at the boundary.

// runesOf returns the rune slice of s (the working representation for editing).
func runesOf(s string) []rune { return []rune(s) }

// locate maps a flat rune offset to its logical {row, col}. row = count of
// newlines before the offset; col = distance from the start of that line.
func locate(rs []rune, offset int) (row, col int) {
	clamped := clampInt(offset, 0, len(rs))
	lineStart := 0
	for i := 0; i < clamped; i++ {
		if rs[i] == '\n' {
			row++
			lineStart = i + 1
		}
	}
	return row, clamped - lineStart
}

// offsetOf returns the flat rune offset of a given logical line/column. The
// column is CLAMPED to the destination line's length — this is exactly what
// gives up/down its "keep the column, snap to EOL if the line is shorter"
// behavior.
func offsetOf(lines [][]rune, row, col int) int {
	r := clampInt(row, 0, len(lines)-1)
	off := 0
	for i := 0; i < r; i++ {
		off += len(lines[i]) + 1 // +1 for each "\n"
	}
	return off + minInt(col, len(lines[r]))
}

// splitLines splits the rune buffer into logical lines on '\n'. A trailing
// newline yields a trailing empty line (matching strings.Split semantics), so
// row/col math stays consistent.
func splitLines(rs []rune) [][]rune {
	lines := [][]rune{{}}
	for _, r := range rs {
		if r == '\n' {
			lines = append(lines, []rune{})
			continue
		}
		last := len(lines) - 1
		lines[last] = append(lines[last], r)
	}
	return lines
}

// lineStartOf returns the offset just after the previous '\n' (or 0).
func lineStartOf(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs)) - 1
	for ; i >= 0; i-- {
		if rs[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// lineEndOf returns the offset of the next '\n' (or len).
func lineEndOf(rs []rune, offset int) int {
	for i := clampInt(offset, 0, len(rs)); i < len(rs); i++ {
		if rs[i] == '\n' {
			return i
		}
	}
	return len(rs)
}

// isSpace reports whether r is whitespace: any Unicode whitespace is a word boundary.
func isSpace(r rune) bool { return unicode.IsSpace(r) }

// isWordRune reports whether r is a "word" character (letter / digit / underscore). CURSOR
// motion (Alt-B/F, Ctrl/Alt+arrows) and Alt-D delete-word move by WORD with this definition,
// so they STOP at punctuation (`/ . : - ?`): you can edit one directory of a path or one
// part of a URL without retyping the whole token (GNU readline's default word semantics).
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// prevWord moves one word left by WORD semantics: skip non-word runs, then the word run, so
// it stops at the nearest punctuation OR whitespace boundary.
func prevWord(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs))
	for i > 0 && !isWordRune(rs[i-1]) {
		i--
	}
	for i > 0 && isWordRune(rs[i-1]) {
		i--
	}
	return i
}

// nextWord moves one word right by WORD semantics (the mirror of prevWord).
func nextWord(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs))
	for i < len(rs) && !isWordRune(rs[i]) {
		i++
	}
	for i < len(rs) && isWordRune(rs[i]) {
		i++
	}
	return i
}

// prevBigWord moves one WORD left by WHITESPACE semantics (a WORD = a maximal run of
// non-whitespace), used by Ctrl-W so a single kill wipes a whole path / URL back to the
// preceding space — GNU readline's unix-word-rubout, deliberately more aggressive than the
// punctuation-stopping word motion above.
func prevBigWord(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs))
	for i > 0 && isSpace(rs[i-1]) {
		i--
	}
	for i > 0 && !isSpace(rs[i-1]) {
		i--
	}
	return i
}

// vrow is one VISUAL row: the rune slice it renders plus the buffer rune offset of its first
// rune. The whole buffer's visual rows are each logical line word-wrapped to the width.
type vrow struct {
	start int
	runes []rune
}

// buildVisualRows splits the buffer into visual rows (logical lines soft-wrapped to width).
// width < 1 (the width is not known yet) collapses to one visual row per LOGICAL line, so
// motion stays logical until the first real render width arrives. The offset accounting adds
// one for each inter-line '\n' so cursor↔row mapping is exact.
func buildVisualRows(rs []rune, width int) []vrow {
	if width < 1 {
		width = 1 << 30 // effectively no wrap → visual rows == logical lines
	}
	lines := splitLines(rs)
	var rows []vrow
	off := 0
	for li, line := range lines {
		for _, seg := range wrapSegments(line, width) {
			rows = append(rows, vrow{start: off, runes: seg})
			off += len(seg)
		}
		if li < len(lines)-1 {
			off++ // the '\n' between logical lines
		}
	}
	return rows
}

// cursorVisual maps a flat rune offset to its (visualRow, visualCol).
func cursorVisual(rows []vrow, cursor int) (vr, vc int) {
	for i := len(rows) - 1; i >= 0; i-- {
		if cursor >= rows[i].start {
			return i, cursor - rows[i].start
		}
	}
	return 0, 0
}

// offsetAtVisual returns the flat rune offset of (visualRow, visualCol), clamping the column
// to the destination row's length — that is what gives ↑/↓ their "keep the column, snap to
// EOL if the row is shorter" behavior.
func offsetAtVisual(rows []vrow, vr, vc int) int {
	if len(rows) == 0 {
		return 0
	}
	vr = clampInt(vr, 0, len(rows)-1)
	row := rows[vr]
	return row.start + clampInt(vc, 0, len(row.runes))
}

// isPrintable rejects control characters so a raw escape sequence (arrows,
// PgUp, F-keys) can never leak into the buffer. Defense in
// depth on top of the key-type gate in the Update routing.
func isPrintable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// normalizeNewlines collapses CRLF / lone CR to '\n' so pasted or typed line
// breaks land as a single buffer newline.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// largePasteLineThreshold / largePasteCharThreshold set when a bracketed paste is
// "large" enough to stash behind a one-line placeholder instead of inserting it
// verbatim. Rendered height is the real driver: a paste taller than a few rows
// pushes the composer past the terminal height, and the fixed bottom band then
// collapses to the one-line "terminal too small" fallback (internal/ui/view.go) —
// the input becomes unusable. A very long SINGLE line soft-wraps to the same
// effect, so the rune count is a second, defensive trip-wire.
const (
	largePasteLineThreshold = 5
	largePasteCharThreshold = 500
)

// pasteLineCount counts logical lines in an already-newline-normalized string
// (one more than the number of '\n').
func pasteLineCount(s string) int { return strings.Count(s, "\n") + 1 }

// isLargePaste reports whether a normalized paste should be stashed behind a
// placeholder rather than inserted verbatim.
func isLargePaste(s string) bool {
	return pasteLineCount(s) >= largePasteLineThreshold || len([]rune(s)) >= largePasteCharThreshold
}

// largePastePlaceholder is the single-line stand-in shown in the buffer for a
// stashed large paste (input must be normalized). It MUST contain no '\n' so the
// composer renders as one row and never trips the too-small fallback. A multi-line
// paste reports its line count; a single very long line reports its rune count
// instead, so the summary reads "12 lines" or "640 chars" and never the
// nonsensical "1 lines".
func largePastePlaceholder(s string) string {
	if n := pasteLineCount(s); n > 1 {
		return "[pasted " + itoa(n) + " lines]"
	}
	return "[pasted " + itoa(len([]rune(s))) + " chars]"
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
