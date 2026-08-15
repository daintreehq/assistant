package composer

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ViewParams carries the per-frame chrome state the parent owns (it is NOT in
// the composer's own state because it is derived from the controller each
// frame): the busy stage label, queue depth, the cancellable/attention flags,
// and the right-aligned context hint.
type ViewParams struct {
	Width       int    // total cell width available (terminal cols minus insets)
	Stage       string // live stage label while busy (NEVER "Thinking"; "Processing…" fallback)
	QueueDepth  int    // messages typed while busy, buffered to fold into the running turn
	Cancellable *bool  // whether the in-flight turn can be aborted; nil → defaults to busy
	// Cancelling marks the in-flight turn as already tearing down (PhaseCancelling).
	// Submitted text can no longer join it — the Session drains buffered injections into
	// the NEXT turn — so the submit verb must not claim otherwise.
	Cancelling  bool
	Attention   bool   // actionable attention pending (promotes ^O)
	ContextHint string // right-aligned session summary
	Placeholder string // shown when the buffer is empty
	MCPStatus   MCPStatus
	// Cost is the pre-rendered session-spend figure ("$0.0123" / "≥ $0.0123"), shown
	// right-aligned on the MCP row, or "" to omit it. Pre-rendered by the caller because
	// the honest phrasing depends on the ledger's lower-bound state, which the composer
	// has no business knowing about.
	Cost string
}

type MCPStatus int

const (
	MCPHidden MCPStatus = iota
	MCPConnecting
	MCPConnected
	MCPDegraded
)

// promptGlyph is the single input marker. Daintree is already named in the
// header, so the composer line just shows a caret-style prompt (U+203A + space,
// ASCII fallback "> ").
const promptGlyph = "› "
const promptGlyphASCII = "> "

// palettePadLeft is the palette box left padding: the slash suggestions hang two
// cells in from the chrome edge so they read as a sub-list under the input rather
// than aligning with it.
const palettePadLeft = 2

// View renders the composer as a string block (palette rows, the input line(s)
// with the caret, the busy cue, and the hint row). Everything is measured by
// terminal CELL width via ansi.StringWidth so wide runes and combining marks
// don't misplace the caret or overflow (the non-negotiable
// cell-measurement rule). The caller is responsible for the left inset / right
// gutter; Width is the usable content width.
func (m *Model) View(p ViewParams) string {
	var b strings.Builder

	// --- slash palette (above the input) ---
	// A column of up to 5 rows, with two cells of left padding and a blank line
	// BELOW the block (before the rule). Each row is the command name in info cyan
	// padded to 14 cells, then the dim description.
	if sugg := m.activeSuggestions(); len(sugg) > 0 {
		pad := strings.Repeat(" ", palettePadLeft)
		sel := clampInt(m.paletteSel, 0, len(sugg)-1)
		visible, visibleSel := paletteWindow(sugg, sel)
		for i, c := range visible {
			marker := "  "
			nameStyle := m.theme.Info()
			if i == visibleSel {
				// Highlight the selected row so Tab's target is visible (the audit gap).
				marker = m.theme.Info().Render("› ")
				nameStyle = m.theme.Body().Reverse(true)
			}
			name := nameStyle.Render(padCells(c.Name, 14))
			desc := m.theme.Dim().Render(c.Desc)
			row := marker + truncateCells(name+desc, p.Width-palettePadLeft-2)
			b.WriteString(pad + row)
			b.WriteByte('\n')
		}
		// Inline usage + accept hint for the highlighted command (surfaces the arg form).
		// "Enter run" is only true while the newline fallback is unarmed: a trailing
		// backslash makes handleKey insert a line break BEFORE it would accept this
		// suggestion, so the palette must not keep promising to run the command.
		enter := "Enter run"
		if m.newlineArmed() {
			enter = "Enter newline"
		}
		hint := "Tab complete · ↑↓ history · " + enter
		if len(sugg) > 1 {
			hint = "Tab complete · ↑↓ move · " + enter
		}
		if len(sugg) > paletteCap {
			hint = itoa(sel+1) + "/" + itoa(len(sugg)) + " · " + hint
		}
		if syn := sugg[sel].Syntax; syn != "" {
			hint = syn + "   " + hint
		}
		b.WriteString(pad + m.theme.Dim().Render(truncateCells(hint, p.Width-palettePadLeft)))
		b.WriteByte('\n')
		// marginBottom={1}: a blank line separates the palette from the rule.
		b.WriteByte('\n')
	}

	// --- rule ABOVE the input ---
	// Brackets the input top so the field reads unmistakably as the place text
	// goes. Full chrome width, muted/dim — matches the masthead's closing rule.
	b.WriteString(m.renderRule(p.Width))
	b.WriteByte('\n')

	// --- the input line(s) with an explicit cell-placed caret ---
	b.WriteString(m.renderInput(p))

	// --- queued follow-ups cue (under the input) ---
	// The LIVE run status (Generating / tool tree …) belongs in the TRANSCRIPT
	// under the DAINTREE marker, NOT here under the input. The input stays clean;
	// we only surface messages buffered to fold into the running turn so they aren't
	// invisible. queuedFollowupLabel owns the grammar and the width fallback.
	if m.busy && p.QueueDepth > 0 {
		if label := queuedFollowupLabel(p.QueueDepth, p.Width); label != "" {
			b.WriteByte('\n')
			b.WriteString(m.theme.Dim().Render(label))
		}
	}

	// --- rule BELOW the input ---
	// Brackets the input bottom; the hints below sit OUTSIDE the rule.
	b.WriteByte('\n')
	b.WriteString(m.renderRule(p.Width))

	// --- adaptive hint row ---
	// renderHints returns "" during reverse-i-search; skip the leading newline too so
	// the search line isn't trailed by a stray blank row.
	if hints := m.renderHints(p); hints != "" {
		b.WriteByte('\n')
		b.WriteString(hints)
	}

	// --- context line (trailing context hint) ---
	// Dim session summary ("agents N · tmr M" when connected, else "MCP degraded").
	if p.ContextHint != "" {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim().Render(truncateCells(p.ContextHint, p.Width)))
	}

	return b.String()
}

// renderRule draws the full-width horizontal rule that brackets the input top and
// bottom. It mirrors the masthead's closing rule:
// a run of the rule glyph in the muted/dim tone, spanning the whole chrome width
// (render_chrome.go uses `th.Muted().Render(strings.Repeat(g.Rule, width))`).
func (m *Model) renderRule(width int) string {
	if width < 1 {
		width = 1
	}
	return m.theme.Muted().Render(strings.Repeat(m.theme.Glyphs.Rule, width))
}

// newlineArmed reports whether the rune immediately left of the cursor is a backslash —
// the state in which Enter inserts a NEWLINE rather than submitting. handleKey checks this
// fallback BEFORE it accepts a palette suggestion or submits, so while it holds it beats
// every other claim anyone could make about Enter.
func (m *Model) newlineArmed() bool {
	rs := runesOf(m.buffer)
	return m.cursor > 0 && m.cursor <= len(rs) && rs[m.cursor-1] == '\\'
}

// isCommandDraft reports whether a submit would be routed as a slash COMMAND rather than
// as prose. It mirrors onSubmit, which tests the TRIMMED submit text for a leading "/".
func isCommandDraft(buffer string) bool {
	return strings.HasPrefix(trim(buffer), "/")
}

// newlineCue is the key cue for the TERMINAL-INDEPENDENT newline: a trailing backslash
// then Enter. Modifier+Enter inserts one too, but only where the terminal actually reports
// the modifier (the kitty keyboard protocol and friends), so advertising Shift+Enter as
// though it were universal would be a promise the row cannot keep. /help documents both.
func (m *Model) newlineCue() string {
	if m.theme.Glyphs.Active == "◌" {
		return `\↵`
	}
	return `\Enter`
}

// promptStr resolves the prompt glyph for the active glyph set
// (`set.active === "◌" ? "› " : "> "`): the unicode set carries the
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
	// Reverse-i-search replaces the normal input line while Ctrl-R is active.
	if m.searching {
		return m.renderSearch(p)
	}

	rs := runesOf(m.buffer)
	prompt := m.promptStr()

	// Width floor: the prompt glyph is 2 cells and the caret another 1, so on a pane
	// narrower than that the input line overran its budget no matter which branch below
	// ran — and an overrun line is soft-wrapped by the host, inflating the fixed band.
	// Nothing useful is renderable at 1-2 cells; emit a bounded caret and stop.
	if p.Width < ansi.StringWidth(prompt)+1 {
		return truncateCells(m.caretCell(' '), p.Width)
	}

	if len(rs) == 0 {
		// Empty buffer:
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
		if len(phr) == 0 {
			// Very narrow terminal: the prompt fills the width (avail <= 0) or the first
			// placeholder rune is too wide to fit, so there is no glyph to host the inverse
			// caret. Show a bare caret instead of indexing phr[0] (which would panic).
			return prompt + m.caretCell(' ')
		}
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
	for row, line := range lines {
		segs := wrapSegments(line, avail)
		// Resolve the caret to a (segment, sub-column) only on the line it sits on.
		caretSeg, caretSub := -1, 0
		if row == curRow {
			caretSeg, caretSub = locateSegment(segs, curCol)
		}
		for s, seg := range segs {
			if firstRow {
				out.WriteString(prompt)
				firstRow = false
			} else {
				out.WriteByte('\n')
				out.WriteString(indent)
			}
			// renderLine now draws a trailing caret on ANY visual row whose caret lands at/
			// after its end, so the cursor stays visible at the EOL of an interior wrapped or
			// logical line — not only on the buffer's last line.
			out.WriteString(m.renderLine(seg, s == caretSeg, caretSub))
		}
	}
	return out.String()
}

// wrapWidth is the cell width the input text wraps at: the content width minus the prompt
// glyph (continuation rows are indented under it). It MUST match the render so visual-row
// vertical motion lands on the same rows the user sees. A 0/unknown width means "no wrap"
// (logical-line motion) until the first real render width arrives via SetWidth.
func (m *Model) wrapWidth() int {
	if m.lastWidth <= 0 {
		return 1 << 30
	}
	w := m.lastWidth - ansi.StringWidth(m.promptStr())
	if w < 1 {
		w = 1
	}
	return w
}

// renderSearch draws the reverse-i-search line in place of the normal input while Ctrl-R is
// active: a "(reverse-i-search)`query`:" prompt followed by the current match (the buffer).
func (m *Model) renderSearch(p ViewParams) string {
	label := "(reverse-i-search)`" + m.searchQuery + "`: "
	if m.searchHit < 0 && m.searchQuery != "" {
		label = "(failed reverse-i-search)`" + m.searchQuery + "`: "
	}
	avail := p.Width - ansi.StringWidth(label)
	if avail < 1 {
		avail = 1
	}
	return m.theme.Dim().Render(label) + m.theme.Body().Render(truncateCells(m.buffer, avail))
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

// renderLine renders one visual row; when withCaret, the rune at col is drawn as an inverse
// caret cell. A caret at/after the row's end draws a trailing caret cell INLINE on this row,
// so the cursor stays visible at the EOL of any wrapped/interior line (not just the last).
func (m *Model) renderLine(line []rune, withCaret bool, col int) string {
	if !withCaret {
		return string(line)
	}
	if col >= len(line) {
		return string(line) + m.caretCell(' ')
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
// the caller doesn't distinguish turn kinds (Cancellable == nil). Each key is info
// cyan and its action dim, joined by a dim " · " separator: keys in the info color,
// actions + separators in DIM. The context hint lives on its OWN line below
// (handled by View), never inline here.
func (m *Model) renderHints(p ViewParams) string {
	// Reverse-i-search owns the whole composer band: renderInput already swaps the
	// input line for renderSearch, so the normal hint row (which advertises "/" and
	// "↑" as live) is stale and misleading while Ctrl-R is active. Suppress it.
	if m.searching {
		return ""
	}
	cancelActive := m.busy
	if p.Cancellable != nil {
		cancelActive = *p.Cancellable
	}
	// An UNFOCUSED composer gets no key hints at all. The approval sheet renders ABOVE
	// the composer rather than replacing it, and onKey routes every key to the approval
	// first — so "/ commands", "↑ history" and the Escape action all belong to a surface
	// that is not going to receive them (there, Escape DECLINES the tool). The connection
	// light below is not a shortcut, so it stays.
	var hints []Hint
	if m.focus {
		// With a draft in the buffer the row switches from DISCOVERY ("what can I do here")
		// to DISPATCH ("how do I send this, how do I add a line"). Multiline input has always
		// worked; nothing advertised it, so it may as well not have.
		drafting := !m.trimEmpty()
		st := hintState{
			escape:      m.escapeState(p),
			discovery:   !drafting,
			leadWithOps: p.Attention && !cancelActive,
		}
		// An open slash palette carries its own "Enter run" cue directly above the input;
		// a second, differently-worded claim on Enter one row below it is the same
		// contradiction the queue cue used to make about Escape.
		if drafting && len(m.activeSuggestions()) == 0 {
			switch {
			case m.newlineArmed():
				// A trailing backslash ARMS the newline fallback, and handleKey checks it
				// before it submits — so right now Enter breaks the line, whatever the buffer
				// would otherwise do. Say that, and drop the separate cue for a fallback that
				// is no longer a fallback.
				st.submit = "newline"
			case isCommandDraft(m.buffer):
				// onSubmit routes a leading slash as a COMMAND before it ever considers the
				// running turn, so this is "run" — never "send" or "add". It matters most
				// mid-turn, where the palette is suppressed and this row is the only cue.
				st.submit = "run"
				st.newlineCue = m.newlineCue()
			case cancelActive && !p.Cancelling:
				// Submitting mid-turn folds the text into the RUNNING turn at its next safe
				// boundary; it does not start a second one, so "send" would overstate it.
				st.submit = "add"
				st.newlineCue = m.newlineCue()
			default:
				// Idle, or a turn already tearing down — a cancelling turn cannot take the
				// text, so it rides the next one, which is what "send" means everywhere else
				// in this row.
				st.submit = "send"
				st.newlineCue = m.newlineCue()
			}
		}
		hints = m.keys.hintRow(st)
	}

	g := m.theme.Glyphs
	// Build the row span-by-span so only the keys carry the info color while the
	// action labels and the " · " separators stay dim (matching the original's
	// per-<span> styling, where a flat Dim() wrap would also dim the cyan keys).
	//
	// Fitting is done per HINT, not by truncating the finished row: hintRow returns
	// priority order, so once one entry won't fit, every lower-priority entry behind it
	// is dropped whole. Truncating the joined string instead would leave a half-word
	// ("^O inspect o…") in the row, and — worse, on a very narrow pane — could eat the
	// state-specific Escape action while a generic "/ commands" survived ahead of it.
	var row strings.Builder
	sep := m.theme.Dim().Render(" " + g.Bullet + " ")
	sepW := ansi.StringWidth(sep)
	used := 0
	for i, h := range hints {
		span := m.theme.Info().Render(h.Key) + m.theme.Dim().Render(" "+h.Action)
		if i == 0 {
			// The lead hint is the highest-priority one; if even it overflows, show as much
			// of it as fits rather than rendering nothing at all.
			lead := truncateCells(span, p.Width)
			row.WriteString(lead)
			used = ansi.StringWidth(lead)
			continue
		}
		w := ansi.StringWidth(span)
		if used+sepW+w > p.Width {
			break
		}
		row.WriteString(sep)
		row.WriteString(span)
		used += sepW + w
	}
	out := row.String()

	// The connection light gets its OWN row rather than riding the end of the key hints,
	// where it was the first thing truncation ate on a narrow pane — and it is the one
	// fact down here that is not about keyboard shortcuts: whether the assistant can act
	// at all.
	//
	// The session bill shares that row, pushed RIGHT. It belongs beside the connection
	// state (both are "what is true right now" rather than "what you can press") and it
	// is a number, which reads better against a hard edge than trailing a label. A third
	// row for one short figure would spend more of a cramped band than it is worth.
	if p.MCPStatus != MCPHidden || p.Cost != "" {
		var left string
		if p.MCPStatus != MCPHidden {
			dot := g.Async
			label := " MCP"
			switch p.MCPStatus {
			case MCPConnected:
				left = m.theme.Body().Foreground(m.theme.Color.Accent).Render(dot + label)
			case MCPDegraded:
				left = m.theme.Danger().Render(dot + label)
			default:
				left = m.theme.Warning().Render(dot + label)
			}
		}
		// Join defensively: with no hints (unfocused) or a width too small to render the
		// status row, a bare "out + \n + row" would contribute an EMPTY row — and empty
		// rows still count against the fixed-height footer budget.
		if status := m.statusRow(left, p.Cost, p.Width); status != "" {
			if out == "" {
				out = status
			} else {
				out += "\n" + status
			}
		}
	}
	return out
}

// statusRow lays out one row with `left` anchored and `right` pushed to the far edge.
//
// Right-alignment is measured in CELLS, not bytes: both sides carry SGR sequences and
// may hold wide runes, so len() would misplace the gap by however many escape bytes the
// theme emitted. When the two cannot both fit, the right side is dropped rather than
// wrapped or squeezed — the left side is the connection state, which is the more
// important of the two on a pane too narrow for both.
func (m *Model) statusRow(left, right string, width int) string {
	if right == "" {
		return truncateCells(left, width)
	}
	styledRight := m.theme.Dim().Render(right)
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(styledRight)
	// One space minimum between them, or they read as a single token.
	if lw+rw+1 > width {
		return truncateCells(left, width)
	}
	return left + strings.Repeat(" ", width-lw-rw) + styledRight
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
