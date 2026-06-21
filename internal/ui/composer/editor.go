// Package composer is the dedicated cockpit input editor: an explicit
// buffer/cursor/kill-ring/history model that satisfies the full key contract in
// docs/port/ui-input.md §1. It deliberately does NOT wrap Bubbles' textarea —
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
// input, paths, and pasted Unicode never corrupt the cursor (ui-input.md §1.1
// NOTE on units). We materialize the buffer as a []rune at every edit and work
// in rune space, converting back to string only at the boundary.

// runesOf returns the rune slice of s (the working representation for editing).
func runesOf(s string) []rune { return []rune(s) }

// locate maps a flat rune offset to its logical {row, col}. row = count of
// newlines before the offset; col = distance from the start of that line.
// Ported from MultilineInput.tsx `locate`.
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
// behavior (ui-input.md §1.1). Ported from MultilineInput.tsx `offsetOf`.
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

// lineStartOf returns the offset just after the previous '\n' (or 0). Ported
// from MultilineInput.tsx `lineStartOf`.
func lineStartOf(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs)) - 1
	for ; i >= 0; i-- {
		if rs[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// lineEndOf returns the offset of the next '\n' (or len). Ported from
// MultilineInput.tsx `lineEndOf`.
func lineEndOf(rs []rune, offset int) int {
	for i := clampInt(offset, 0, len(rs)); i < len(rs); i++ {
		if rs[i] == '\n' {
			return i
		}
	}
	return len(rs)
}

// isSpace mirrors the TS `/\s/` test: any Unicode whitespace is a word boundary.
func isSpace(r rune) bool { return unicode.IsSpace(r) }

// prevWord moves one word left: skip trailing whitespace, then the word itself.
// Whitespace-delimited (a word = a maximal run of non-whitespace), so it is
// predictable for prose, paths, and identifiers. Ported verbatim from
// MultilineInput.tsx `prevWord`.
func prevWord(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs))
	for i > 0 && isSpace(rs[i-1]) {
		i--
	}
	for i > 0 && !isSpace(rs[i-1]) {
		i--
	}
	return i
}

// nextWord moves one word right: skip leading whitespace, then the word itself.
// Ported verbatim from MultilineInput.tsx `nextWord`.
func nextWord(rs []rune, offset int) int {
	i := clampInt(offset, 0, len(rs))
	for i < len(rs) && isSpace(rs[i]) {
		i++
	}
	for i < len(rs) && !isSpace(rs[i]) {
		i++
	}
	return i
}

// isPrintable rejects control characters so a raw escape sequence (arrows,
// PgUp, F-keys) can never leak into the buffer (ui-input.md §1.8). Defense in
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
// breaks land as a single buffer newline (ui-input.md §1.8).
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
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
