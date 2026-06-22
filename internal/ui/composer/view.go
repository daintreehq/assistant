package composer

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ViewParams carries the per-frame chrome state the parent owns (it is NOT in
// the composer's own state because it is derived from the controller each
// frame): the busy stage label, queue depth, the cancellable/attention flags,
// and the right-aligned context hint (ui-input.md §1.11).
type ViewParams struct {
	Width       int    // total cell width available (terminal cols minus insets)
	Stage       string // live stage label while busy (NEVER "Thinking"; "Processing…" fallback)
	QueueDepth  int    // user follow-ups queued behind the in-flight turn
	Cancellable *bool  // whether the in-flight turn can be aborted; nil → defaults to busy
	Attention   bool   // actionable attention pending (promotes ^O)
	ContextHint string // right-aligned session summary
	Placeholder string // shown when the buffer is empty
}

// promptGlyph is the single input marker. Daintree is already named in the
// header, so the composer line just shows a caret-style prompt (U+203A + space,
// ASCII fallback "> "). Ported from Composer.tsx `prompt={set.active === "◌" ? "› " : "> "}`.
const promptGlyph = "› "
const promptGlyphASCII = "> "

// palettePadLeft mirrors the original palette box `paddingLeft={2}`: the slash
// suggestions hang two cells in from the chrome edge so they read as a sub-list
// under the input rather than aligning with it. Ported from Composer.tsx.
const palettePadLeft = 2

// View renders the composer as a string block (palette rows, the input line(s)
// with the caret, the busy cue, and the hint row). Everything is measured by
// terminal CELL width via ansi.StringWidth so wide runes and combining marks
// don't misplace the caret or overflow (ui-input.md §0; the non-negotiable
// cell-measurement rule). The caller is responsible for the left inset / right
// gutter; Width is the usable content width.
func (m *Model) View(p ViewParams) string {
	var b strings.Builder

	// --- slash palette (above the input) ---
	// Ported from Composer.tsx: a column of up to 5 rows, `paddingLeft={2}` and
	// `marginBottom={1}` (a blank line BELOW the block, before the rule). Each row
	// is the command name in info cyan padded to 14 cells, then the dim description.
	if sugg := m.activeSuggestions(); len(sugg) > 0 {
		pad := strings.Repeat(" ", palettePadLeft)
		for _, c := range sugg {
			name := m.theme.Info().Render(padCells(c.Name, 14))
			desc := m.theme.Dim().Render(c.Desc)
			row := truncateCells(name+desc, p.Width-palettePadLeft)
			b.WriteString(pad + row)
			b.WriteByte('\n')
		}
		// marginBottom={1}: a blank line separates the palette from the rule.
		b.WriteByte('\n')
	}

	// --- rule ABOVE the input (Composer.tsx `<Divider />`) ---
	// Brackets the input top so the field reads unmistakably as the place text
	// goes. Full chrome width, muted/dim — matches the masthead's closing rule.
	b.WriteString(m.renderRule(p.Width))
	b.WriteByte('\n')

	// --- the input line(s) with an explicit cell-placed caret ---
	b.WriteString(m.renderInput(p))

	// --- queued follow-ups cue (under the input) ---
	// The LIVE run status (Generating / tool tree …) belongs in the TRANSCRIPT
	// under the DAINTREE marker, NOT here under the input. The input stays clean;
	// we only surface silently-queued follow-ups so they aren't invisible (#95).
	// Ported verbatim from Composer.tsx: `busy && queueDepth > 0 ? "N queued"`.
	if m.busy && p.QueueDepth > 0 {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim().Render(truncateCells(itoa(p.QueueDepth)+" queued", p.Width)))
	}

	// --- rule BELOW the input (Composer.tsx second `<Divider />`) ---
	// Brackets the input bottom; the hints below sit OUTSIDE the rule.
	b.WriteByte('\n')
	b.WriteString(m.renderRule(p.Width))

	// --- adaptive hint row ---
	b.WriteByte('\n')
	b.WriteString(m.renderHints(p))

	// --- context line (Composer.tsx trailing `{contextHint}`) ---
	// Dim session summary ("agents N · tmr M" when connected, else "MCP degraded").
	if p.ContextHint != "" {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim().Render(truncateCells(p.ContextHint, p.Width)))
	}

	return b.String()
}

// renderRule draws the full-width horizontal rule that brackets the input top and
// bottom (Composer.tsx `<Divider />`). It mirrors the masthead's closing rule:
// a run of the rule glyph in the muted/dim tone, spanning the whole chrome width
// (render_chrome.go uses `th.Muted().Render(strings.Repeat(g.Rule, width))`).
func (m *Model) renderRule(width int) string {
	if width < 1 {
		width = 1
	}
	return m.theme.Muted().Render(strings.Repeat(m.theme.Glyphs.Rule, width))
}

// promptStr resolves the prompt glyph for the active glyph set, mirroring
// Composer.tsx `set.active === "◌" ? "› " : "> "`: the unicode set carries the
// chevron, the ASCII fallback the plain ">".
func (m *Model) promptStr() string {
	if m.theme.Glyphs.Active == "◌" {
		return promptGlyph
	}
	return promptGlyphASCII
}

// renderInput draws the buffer with the caret rendered as an inverse cell at the
// cursor. The prompt glyph leads the first logical line; continuation lines are
// indented to align under it. When the buffer is empty the placeholder is shown
// dimmed with the caret at the start.
func (m *Model) renderInput(p ViewParams) string {
	rs := runesOf(m.buffer)
	prompt := m.promptStr()

	if len(rs) == 0 {
		// Empty buffer. Ported from MultilineInput.tsx's `value.length === 0` branch:
		//   focused + placeholder → the placeholder's FIRST char is the inverse
		//     (block) caret, the REST is dim. The caret IS the first glyph, not a
		//     separate cell before the text.
		//   focused + no placeholder → a single inverse space (the bare caret).
		//   unfocused → the whole placeholder dim, no caret.
		// The placeholder is truncated to the usable width (minus the prompt glyph)
		// so a long prompt never overflows on a narrow terminal.
		if !m.focus {
			if p.Placeholder == "" {
				return prompt
			}
			avail := p.Width - ansi.StringWidth(prompt)
			return prompt + m.theme.Dim().Render(truncateCells(p.Placeholder, avail))
		}
		if p.Placeholder == "" {
			return prompt + m.caretCell(' ')
		}
		avail := p.Width - ansi.StringWidth(prompt)
		ph := truncateCells(p.Placeholder, avail)
		phr := []rune(ph)
		first := m.caretCell(phr[0])
		rest := m.theme.Dim().Render(string(phr[1:]))
		return prompt + first + rest
	}

	// Split into logical lines, then WORD-WRAP each one to the available content
	// width so a long line stays fully visible instead of running off the right
	// edge. The prompt glyph leads the very first visual row; every continuation
	// row (whether from a hard '\n' or a soft wrap) is indented to align under it,
	// measured by the prompt's cell width. We place the caret on whichever VISUAL
	// row holds the cursor rune and let the terminal position the inverse cell, so
	// no cell arithmetic is needed for wide runes here.
	//
	// Wrapping MUST happen inside the composer: footer() bounds the live region by
	// counting "\n"-delimited rows as display rows (view.go lineCount), so an
	// unwrapped over-wide line would undercount and corrupt that budget.
	lines := splitLines(rs)
	indent := strings.Repeat(" ", ansi.StringWidth(prompt))
	avail := p.Width - ansi.StringWidth(prompt)
	if avail < 1 {
		avail = 1
	}
	curRow, curCol := locate(rs, m.cursor)

	var out strings.Builder
	firstRow := true
	trailingCaret := false
	for row, line := range lines {
		segs := wrapSegments(line, avail)
		// Resolve the caret to a (segment, sub-column) only on the line it sits on.
		caretSeg, caretSub := -1, 0
		if row == curRow {
			caretSeg, caretSub = locateSegment(segs, curCol)
			// A caret past the last rune of the last segment on the LAST logical line
			// has no glyph to invert — defer it to an explicit trailing cell below.
			if row == len(lines)-1 && caretSeg == len(segs)-1 && caretSub >= len(segs[caretSeg]) {
				trailingCaret = true
			}
		}
		for s, seg := range segs {
			if firstRow {
				out.WriteString(prompt)
				firstRow = false
			} else {
				out.WriteByte('\n')
				out.WriteString(indent)
			}
			out.WriteString(m.renderLine(seg, s == caretSeg, caretSub))
		}
	}
	if trailingCaret {
		out.WriteString(m.caretCell(' '))
	}
	return out.String()
}

// wrapSegments word-wraps a single logical line into visual segments each at most
// `width` cells wide. Concatenating the segments reproduces the line EXACTLY (no
// runes dropped, no spaces collapsed) so the cursor column maps cleanly onto a
// segment. Breaks prefer the last space at/under the width boundary, keeping that
// space at the END of the current segment so continuation rows never start with a
// stray space; a single word longer than the width is hard-broken. An empty line
// yields one empty segment so it still occupies a visual row.
func wrapSegments(line []rune, width int) [][]rune {
	if width < 1 {
		width = 1
	}
	if len(line) == 0 {
		return [][]rune{{}}
	}
	var segs [][]rune
	for i := 0; i < len(line); {
		// Greedily take as many runes as fit in `width` cells (always at least one,
		// so a single over-wide rune still makes progress).
		w, j := 0, i
		for j < len(line) {
			rw := ansi.StringWidth(string(line[j]))
			if j > i && w+rw > width {
				break
			}
			w += rw
			j++
		}
		end := j
		if j < len(line) {
			// Broke mid-line: prefer a space boundary — break AFTER the last space in
			// (i, j) so the space trails the current segment rather than leading the next.
			for k := j - 1; k > i; k-- {
				if isSpace(line[k]) {
					end = k + 1
					break
				}
			}
		}
		segs = append(segs, line[i:end])
		i = end
	}
	return segs
}

// locateSegment maps a column within a logical line to its (segment index, column
// within that segment) for the wrapped layout. Segments concatenate to the whole
// line, so the mapping is exact. A column at a segment boundary resolves to the
// START of the next segment; a column at the line's end resolves to one past the
// end of the LAST segment (the caller draws a trailing caret there).
func locateSegment(segs [][]rune, col int) (seg, sub int) {
	acc := 0
	for i, s := range segs {
		if col < acc+len(s) {
			return i, col - acc
		}
		acc += len(s)
	}
	last := len(segs) - 1
	if last < 0 {
		return 0, 0
	}
	return last, col - (acc - len(segs[last]))
}

// renderLine renders one logical line; when withCaret, the rune at col is drawn
// as an inverse caret cell (or a trailing caret when col is at/after EOL — that
// trailing case is handled by the caller for the last line).
func (m *Model) renderLine(line []rune, withCaret bool, col int) string {
	if !withCaret || col >= len(line) {
		return string(line)
	}
	var b strings.Builder
	b.WriteString(string(line[:col]))
	b.WriteString(m.caretCell(line[col]))
	b.WriteString(string(line[col+1:]))
	return b.String()
}

// caretCell renders a single rune as an inverse (reverse-video) caret cell so
// the cursor is visible on the normal screen buffer (we never capture the real
// terminal cursor). A space caret shows a solid block-like reverse cell.
func (m *Model) caretCell(r rune) string {
	if r == 0 {
		r = ' '
	}
	return m.theme.Body().Reverse(true).Render(string(r))
}

// renderHints draws the adaptive hint row. cancelActive defaults to busy when
// the caller doesn't distinguish turn kinds (Cancellable == nil), per §1.11. Each
// key is info cyan and its action dim, joined by a dim " · " separator. Ported
// from Composer.tsx: keys in `ui.color.info`, actions + separators in DIM. The
// context hint lives on its OWN line below (handled by View), never inline here.
func (m *Model) renderHints(p ViewParams) string {
	cancelActive := m.busy
	if p.Cancellable != nil {
		cancelActive = *p.Cancellable
	}
	leadWithOps := p.Attention && !cancelActive
	hints := m.keys.hintRow(cancelActive, leadWithOps)

	g := m.theme.Glyphs
	// Build the row span-by-span so only the keys carry the info color while the
	// action labels and the " · " separators stay dim (matching the original's
	// per-<span> styling, where a flat Dim() wrap would also dim the cyan keys).
	var row strings.Builder
	sep := m.theme.Dim().Render(" " + g.Bullet + " ")
	for i, h := range hints {
		if i > 0 {
			row.WriteString(sep)
		}
		row.WriteString(m.theme.Info().Render(h.Key))
		row.WriteString(m.theme.Dim().Render(" " + h.Action))
	}
	return truncateCells(row.String(), p.Width)
}

// padCells right-pads s with spaces to at least w cells (cell-measured).
func padCells(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncateCells trims s to at most w cells, appending an ellipsis when cut. Uses
// ansi.Truncate so wide runes and any SGR are respected.
func truncateCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// itoa is a tiny int→string for the queue badge (avoids importing strconv just
// for one call site).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
