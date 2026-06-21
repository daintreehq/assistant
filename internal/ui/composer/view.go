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
// header, so the composer line just shows a caret-style prompt.
const promptGlyph = "› "

// View renders the composer as a string block (palette rows, the input line(s)
// with the caret, the busy cue, and the hint row). Everything is measured by
// terminal CELL width via ansi.StringWidth so wide runes and combining marks
// don't misplace the caret or overflow (ui-input.md §0; the non-negotiable
// cell-measurement rule). The caller is responsible for the left inset / right
// gutter; Width is the usable content width.
func (m *Model) View(p ViewParams) string {
	var b strings.Builder

	// --- slash palette (above the input) ---
	for _, c := range m.activeSuggestions() {
		// Command name in info cyan (interactive-affordance motif), padded to 14
		// cells; the description stays dim. Truncate the composed row to width.
		name := m.theme.Info().Render(padCells(c.Name, 14))
		desc := m.theme.Dim().Render(c.Desc)
		row := truncateCells(name+desc, p.Width)
		b.WriteString(row)
		b.WriteByte('\n')
	}

	// --- the input line(s) with an explicit cell-placed caret ---
	b.WriteString(m.renderInput(p))

	// --- busy cue (under the input) ---
	if m.busy {
		b.WriteByte('\n')
		b.WriteString(m.renderBusyCue(p))
	}

	// --- adaptive hint row ---
	b.WriteByte('\n')
	b.WriteString(m.renderHints(p))

	return b.String()
}

// renderInput draws the buffer with the caret rendered as an inverse cell at the
// cursor. The prompt glyph leads the first logical line; continuation lines are
// indented to align under it. When the buffer is empty the placeholder is shown
// dimmed with the caret at the start.
func (m *Model) renderInput(p ViewParams) string {
	rs := runesOf(m.buffer)

	if len(rs) == 0 {
		// Empty: caret + dim placeholder. The placeholder is truncated to the
		// usable width (minus the prompt glyph + caret cell) so a long prompt never
		// overflows the gutter on a narrow terminal (the over-width contract).
		caret := m.caretCell(' ')
		ph := ""
		if p.Placeholder != "" {
			avail := p.Width - ansi.StringWidth(promptGlyph) - 1
			ph = m.theme.Dim().Render(truncateCells(p.Placeholder, avail))
		}
		return promptGlyph + caret + ph
	}

	// Split into logical lines, place the caret on its (row,col), and prefix the
	// first line with the prompt, the rest with matching indent so wrapped logical
	// lines stay readable. We measure indent by the prompt's cell width.
	lines := splitLines(rs)
	curRow, curCol := locate(rs, m.cursor)
	indent := strings.Repeat(" ", ansi.StringWidth(promptGlyph))

	var out strings.Builder
	for row, line := range lines {
		if row == 0 {
			out.WriteString(promptGlyph)
		} else {
			out.WriteByte('\n')
			out.WriteString(indent)
		}
		out.WriteString(m.renderLine(line, row == curRow, curCol))
	}
	// If the caret sits at the very end on a final empty position, ensure a caret
	// cell is shown (renderLine handles in-line carets; a caret at EOL of the last
	// line needs an explicit trailing cell).
	if curRow == len(lines)-1 && curCol >= len(lines[curRow]) {
		out.WriteString(m.caretCell(' '))
	}
	return out.String()
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

// renderBusyCue shows the precise stage label + "· N queued" when queued > 0.
// The stage comes from the controller (deriveStage), NEVER a generic "Thinking";
// a blank stage falls back to "Processing…" (ui-input.md §1.11, interaction-ux).
func (m *Model) renderBusyCue(p ViewParams) string {
	g := m.theme.Glyphs
	stage := p.Stage
	if strings.TrimSpace(stage) == "" {
		stage = "Processing…"
	}
	// The spinner base glyph; the animated frame is selected by the parent's tick
	// — here we show the static active glyph so the cue is self-contained.
	dot := g.Active
	cue := dot + " " + stage
	if p.QueueDepth > 0 {
		cue += " " + g.Bullet + " " + itoa(p.QueueDepth) + " queued"
	}
	return m.theme.Muted().Render(truncateCells(cue, p.Width))
}

// renderHints draws the adaptive hint row. cancelActive defaults to busy when
// the caller doesn't distinguish turn kinds (Cancellable == nil), per §1.11.
func (m *Model) renderHints(p ViewParams) string {
	cancelActive := m.busy
	if p.Cancellable != nil {
		cancelActive = *p.Cancellable
	}
	leadWithOps := p.Attention && !cancelActive
	hints := m.keys.hintRow(cancelActive, leadWithOps)

	g := m.theme.Glyphs
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		// Hint keys are info cyan (the "thing you can type" motif shared with code
		// spans, links, and the slash palette); the action label stays dim.
		parts = append(parts, m.theme.Info().Render(h.Key)+" "+h.Action)
	}
	row := strings.Join(parts, " "+g.Bullet+" ")
	if p.ContextHint != "" {
		// Right-align the context hint within the width when there's room.
		left := truncateCells(row, p.Width)
		gap := p.Width - ansi.StringWidth(left) - ansi.StringWidth(p.ContextHint)
		if gap > 1 {
			return m.theme.Dim().Render(left + strings.Repeat(" ", gap) + p.ContextHint)
		}
		return m.theme.Dim().Render(left)
	}
	return m.theme.Dim().Render(truncateCells(row, p.Width))
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
