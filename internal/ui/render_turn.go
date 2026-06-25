package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_turn.go renders a TurnCell (and its user message) to a styled string from
// its ordered Steps. The renderer
// takes width + expanded + now and is pure given (cell, theme, md, width, ...) so
// the scrollback queue can re-render frozen blocks fresh on resize.

// renderUserMessage renders the "YOU" card: a quiet "YOU" label on its OWN line,
// then the wrapped message body as a contiguous fill BLOCK that butts up against a
// left accent bar (▏, U+258F), one bar+block per row. The fill (a near-background
// tint from the theme's UserMessageSurface) is what makes the human's own words
// read as a distinct surface rather than another bar'd list like the tool tree — a
// lone bar read too much like one. A system-origin turn (UserText == "") renders
// nothing.
func renderUserMessage(th theme.Theme, text string, width int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	// Trailing newlines are noise here: they would inflate the logical-line count (a
	// paste ending in "\n" would collapse one line sooner than the same paste without
	// it) and leave a stray blank fill row at the bottom of the card.
	text = strings.TrimRight(text, "\n")
	g := th.Glyphs
	// Colors come from the theme's UserMessageSurface (a cool neutral gray, NOT accent
	// green — green is reserved for Daintree's identity).
	surface := th.UserMessageSurface()
	barStyle := th.Muted()
	if surface.Bar != nil {
		barStyle = th.Dim().Foreground(surface.Bar)
	}
	// When there's a fill, the bar shares the fill background so the block reaches
	// all the way to the accent line — the line becomes the block's left edge rather
	// than a separate spine with an unfilled seam beside it.
	if surface.Fill != nil {
		barStyle = barStyle.Background(surface.Fill)
	}
	bar := barStyle.Render(g.Bar)
	// Body text color: the surface text color, falling back to plain body fg.
	textStyle := th.Body()
	if surface.Text != nil {
		textStyle = textStyle.Foreground(surface.Text)
	}
	// The "YOU" anchor is a notch LIGHTER than the body — the fill block below now
	// carries the "this is yours" signal, so the label recedes (faint, NOT bold).
	labelStyle := th.Dim()
	if surface.Label != nil {
		labelStyle = th.Body().Foreground(surface.Label)
	}

	var b strings.Builder
	b.WriteString(labelStyle.Render("YOU"))

	// Per-row budget: every rendered row must stay within `width` (the chrome width
	// chromeW = columns - gutter - LeftPad). The card is LeftPad-indented at commit,
	// and the gutter reserves the host autowrap column, so a row <= width can never
	// wrap a frozen scrollback line. rowBudget = width-1 keeps one extra column spare.
	// Deriving the geometry from rowBudget (NOT a fixed `inner >= 10` floor, which
	// could push a sub-14-col row past width and wrap) makes every width safe.
	rowBudget := width - 1
	if rowBudget < 1 {
		rowBudget = 1
	}
	// Uniform geometry in EVERY mode: reserve bar(1) + gap(1) + a 1-col right margin,
	// so the text wrap width is identical whether or not a fill is drawn (the fill
	// just paints that right margin; the fallback leaves it as spare). Keeping `inner`
	// mode-independent makes the rendered row count stable across themes.
	inner := rowBudget - 3
	if inner < 1 {
		inner = 1
	}
	// Fill needs room for the whole block (bar + gap + text + margin); below that, fall
	// back to the plain bar.
	useFill := surface.Fill != nil && rowBudget >= 5
	blockW := inner + 2 // gap + text + right margin; bar + blockW == rowBudget
	// writeRow emits one body row: bar + (fill block | bar-only fallback). Factored
	// out so the head, the tail, and the middle trim rule all share the exact same
	// geometry — every row is bar + blockW == rowBudget cells, with the fill (or the
	// lone bar) carrying the cue.
	writeRow := func(content string, style lipgloss.Style) {
		b.WriteByte('\n')
		if useFill {
			// Fixed-width fill: a leading space is the gap after the bar; lipgloss
			// pads the remainder of blockW with the background, producing a clean
			// rectangle that meets the bar with no unfilled seam.
			block := style.Background(surface.Fill).Width(blockW).
				Render(" " + truncateCells(content, inner))
			b.WriteString(bar + block)
		} else {
			// No fill (ansi/none, or a terminal too narrow for a block): the bar
			// alone carries the cue.
			b.WriteString(bar + " " + style.Render(truncateCells(content, inner)))
		}
	}
	// writeParagraph wraps one explicit paragraph (hard \n breaks preserved, matching
	// wrapText) to inner, one bar + block per visual row so the gutter stays aligned.
	writeParagraph := func(para string) {
		for _, line := range strings.Split(wrapCells(para, inner), "\n") {
			writeRow(line, textStyle)
		}
	}

	// A very long paste is shown as head + a "N lines hidden" rule + tail instead of
	// in full, so it can't bury the conversation in scrollback (the committed YOU card
	// is otherwise as tall as the paste — see flush.go's chunked commit). Trimming is
	// by LOGICAL line — what the human actually pasted — and the split deliberately
	// favors the TAIL: a pasted log or stack trace usually carries its payoff at the
	// bottom, while the head only has to be enough to recognize what was pasted. We
	// collapse only when it hides at least 2 lines (len > head+tail+1) — replacing a
	// single hidden line with a one-row rule would save nothing. The rule itself rides
	// the same fill block (renderHiddenRule), so the card stays one contiguous surface.
	lines := strings.Split(text, "\n")
	if len(lines) > userMsgHeadLines+userMsgTailLines+1 {
		for _, para := range lines[:userMsgHeadLines] {
			writeParagraph(para)
		}
		// The rule recedes to chrome: the faint Label tone (the same quiet hue as the
		// "YOU" anchor), or a plain dim attribute where the theme has no Label color.
		ruleStyle := th.Dim()
		if surface.Label != nil {
			ruleStyle = th.Body().Foreground(surface.Label)
		}
		hidden := len(lines) - userMsgHeadLines - userMsgTailLines
		writeRow(renderHiddenRule(g, hidden, inner), ruleStyle)
		for _, para := range lines[len(lines)-userMsgTailLines:] {
			writeParagraph(para)
		}
	} else {
		for _, para := range lines {
			writeParagraph(para)
		}
	}
	return b.String()
}

// userMsgHeadLines / userMsgTailLines bound a long YOU-card paste: a message of more
// than head+tail+1 LOGICAL lines is collapsed to its first head lines, a middle "N
// lines hidden" rule, and its last tail lines. The tail is the larger share because a
// pasted log/stack-trace's payoff is usually at the bottom.
const (
	userMsgHeadLines = 8
	userMsgTailLines = 12
)

// renderHiddenRule builds the trim row's content: a horizontal rule exactly `width`
// cells wide with a centered "N lines hidden" count, e.g. "──── 47 lines hidden ────".
// The dashes are the theme's width-1 rule unit (g.Rule: ─, ASCII "-"), and an
// ultra-narrow card collapses to just the (clipped) label. Width is exact so the
// caller's fill block (Width(blockW)) pads it like any other body row.
func renderHiddenRule(g theme.GlyphSet, hidden, width int) string {
	if width < 1 {
		width = 1
	}
	label := fmt.Sprintf(" %d lines hidden ", hidden) // breathing space around the count
	lw := cellWidth(label)
	if lw >= width {
		// Too narrow for any rule dashes — just the (clipped) label.
		return truncateCells(label, width)
	}
	// g.Rule is width-1, so the dashes either side of the label sum to exactly the
	// remaining cells (pad), keeping the row exactly `width` cells wide.
	pad := width - lw
	left := pad / 2
	return strings.Repeat(g.Rule, left) + label + strings.Repeat(g.Rule, pad-left)
}

// renderMarker renders the "◆ DAINTREE" marker line, with a dim "· received" only
// while phase == received.
func renderMarker(th theme.Theme, phase domain.RunPhase, active bool) string {
	g := th.Glyphs
	marker := th.Accent().Render(g.Brand + " DAINTREE")
	if active && phase == domain.PhaseReceived {
		marker += th.Dim().Render(" · received")
	}
	return marker
}

// stallThresholdMs is how long the active turn can go with no streamed token / tool
// event before the live status flips to a "still working" cue — distinguishing a slow
// model/tool from a hung one (the liveness gap).
const stallThresholdMs = 5000

// renderLiveStatus renders the LiveRunStatus line ("⠋ Analyzing request · 0.4s")
// for the silent-work phases ONLY (driven by the explicit phase, never emptiness).
// Returns "" when the phase is self-evident.
func renderLiveStatus(th theme.Theme, t *TurnCell, spinnerFrame int, now int64) string {
	if t.State != TurnActive {
		return ""
	}
	label := liveStatusLabel(t.Phase)
	if label == "" {
		return ""
	}
	g := th.Glyphs
	spin := g.Active
	if len(g.Spinner) > 0 {
		spin = g.Spinner[spinnerFrame%len(g.Spinner)]
	}
	// Elapsed is CUMULATIVE over the whole turn (t.StartedAt), not the current phase, so it
	// doesn't reset to 0 on every transition. Shown only once ≥300ms (elapsedToken).
	elapsed := elapsedToken(t.StartedAt, now)
	// Stalled: nothing has streamed for stallThresholdMs — surface "still working" in the
	// warning tone so a slow model/tool reads differently from a hung one.
	if t.LastActivityAt > 0 && now-t.LastActivityAt > stallThresholdMs {
		return th.Body().Render(spin) + th.Warning().Render(" "+label+" · still working"+elapsed)
	}
	// The spinner (ThinkingDot) is a PLAIN <text> (terminal default fg,
	// no tone), and the label + elapsed are DIM (attribute-only faint), NOT muted gray.
	return th.Body().Render(spin) + th.Dim().Render(" "+label+elapsed)
}

// renderTurn renders the full turn cell: user message, marker, ordered steps
// (prose as markdown / activity tree / notes), then the live status. md is the
// theme-bound markdown renderer; width is the content width; now drives live
// elapsed; expanded reveals raw tool detail. It composes the same preamble +
// step-range pieces the incremental flush and seal use (renderTurnPreamble /
// renderTurnSteps), so a turn renders byte-identically whether it streams through
// the live footer or commits to scrollback.
//
// The live-status line is appended only while the turn is active (renderLiveStatus
// returns "" for a sealed turn), so the flush's immutable prefix (preamble + finalized
// steps via renderTurnSteps) is always a row-exact PREFIX of this full render.
func renderTurn(th theme.Theme, md *markdown.Renderer, t *TurnCell, width, contentW int, expanded bool, spinnerFrame int, now int64) string {
	active := t.State == TurnActive
	var parts []string
	// The marker shows once the turn is active OR has produced ANY output. It must be
	// hasResponded (any step), NOT hasProse: the incremental flush ALWAYS commits the
	// marker (flush.go renderTurnPreamble showMarker=true while the turn is active), so a
	// SEALED tool-only / note-only turn (no prose) must still render the marker — otherwise
	// the sealed render is one row shorter than the flushed prefix and sealTail's row-count
	// fallback drops the turn's first tool/note row from scrollback.
	if pre := renderTurnPreamble(th, t, width, active, active || hasResponded(t)); pre != "" {
		parts = append(parts, pre)
	}
	// withholdGrowingLast=false: the footer (and the seal) render the LAST prose step in FULL —
	// including its still-growing final paragraph, re-parsed as markdown every frame so prose
	// streams smoothly token by token. Only the still-MUTABLE tail of that paragraph lives in
	// the un-flushed footer: liveCellsView slices off FlushedRows, and the FLUSH
	// (activeTurnFinalRows) passes withholdGrowingLast=true so renderProse commits the settled
	// row prefix — line by line for a plain tail, paragraph by paragraph for a markdown one — and
	// never freezes a row that later reflows. With a plain tail the footer therefore holds only
	// the partial last line + the live status; the lastLines(budget) cap in view.go is a height
	// backstop (it still bounds a withheld markdown paragraph) against bubbletea#1613.
	hasBody := false
	if body := renderTurnSteps(th, md, t, 0, -1, width, contentW, expanded, spinnerFrame, now, false); body != "" {
		parts = append(parts, body)
		hasBody = true
	}
	if ls := renderLiveStatus(th, t, spinnerFrame, now); ls != "" {
		// A blank line sets the live status apart from the response above it, so the
		// "⠋ Writing · …" thinking cue reads as a distinct status indicator instead of
		// glued to the last line of prose. The blank lives ONLY in the live tail: the
		// status itself is dropped the instant the turn seals (renderLiveStatus returns
		// "" for a non-active turn) and is never part of activeTurnFinalRows, so this
		// extra row can't disturb the byte-exact flush↔seal prefix reconciliation.
		//
		// Gate the blank on a non-empty body: when the turn has produced no rendered step
		// yet (e.g. the silent "⠋ Analyzing request" gap right after submit), there is only
		// the bare "◆ DAINTREE" marker above, and a blank between the marker and the status
		// reads as an empty hole — so glue the status to the marker until real content lands.
		if hasBody {
			parts = append(parts, "", ls)
		} else {
			parts = append(parts, ls)
		}
	}
	return strings.Join(parts, "\n")
}

// renderTurnPreamble renders the immutable head of a turn: the "YOU" card (when the
// turn has user text) and the "◆ DAINTREE" marker. markerActive drives the live
// "· received" suffix (only meaningful while phase == received); showMarker gates the
// marker line. The result has NO leading/trailing blank — callers join it with the
// step body. This is the unit the incremental flush commits ONCE (it never changes
// after the turn starts responding), so the live footer can stop re-rendering it.
func renderTurnPreamble(th theme.Theme, t *TurnCell, width int, markerActive, showMarker bool) string {
	var b strings.Builder
	if um := renderUserMessage(th, t.UserText, width); um != "" {
		b.WriteString(um)
		// A blank line separates the YOU card
		// from the ◆ DAINTREE marker so the exchange breathes.
		b.WriteString("\n\n")
	}
	if showMarker {
		b.WriteString(renderMarker(th, t.Phase, markerActive))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTurnSteps renders the ordered steps in the half-open range [from, to) of the
// turn (to < 0 means "to the end"): prose as styled markdown, contiguous tool runs as
// one branch tree, notes inline. withholdGrowingLast governs ONLY the turn's genuine LAST
// prose step (global index len(Steps)-1): when set, that step renders its IMMUTABLE row
// prefix — all but the still-mutable tail of the growing paragraph (renderProse settles a
// plain tail line by line, a markdown tail paragraph by paragraph). This is the COMMIT-bound
// flush render — it must never freeze a row that later reflows. When unset (the footer and the
// seal) the last step renders in FULL, so the live footer reprocesses the growing paragraph as
// markdown each frame. It applies ONLY to that last step, so an earlier prose step that streamed
// before a tool batch renders as FINAL markdown regardless of its sticky Streaming flag. The
// flush's render is a row-exact PREFIX of the footer's full render — renderProse derives it from
// the SAME md.Render call and trims only the mutable tail — so the un-flushed remainder sits in
// the footer tail until it settles and is never double-committed.
//
// Tool grouping is computed over the sub-range; the incremental flush only ever passes a
// range that begins and ends on a tool-group boundary (see finalizedStepCount), so a
// branch tree is never split across the flush frontier.
func renderTurnSteps(th theme.Theme, md *markdown.Renderer, t *TurnCell, from, to, width, contentW int, expanded bool, spinnerFrame int, now int64, withholdGrowingLast bool) string {
	steps := t.Steps
	if to < 0 || to > len(steps) {
		to = len(steps)
	}
	if from < 0 {
		from = 0
	}
	if from >= to {
		return ""
	}
	sub := steps[from:to]
	lastIdx := len(steps) - 1

	var b strings.Builder
	groups := collectToolGroups(sub)
	gi := 0
	for li := range sub {
		step := sub[li]
		g := from + li
		// A blank line AFTER a tool group: separate the function-call ledger from the prose
		// or note that follows it. The blank rides the FOLLOWING step (a leading blank) so it
		// survives the flush boundary — when the tool group flushes alone, the blank flushes
		// with the prose later, keeping spacing identical across streaming and seal.
		afterTool := g > 0 && steps[g-1].Kind == StepTool
		switch step.Kind {
		case StepProse:
			withhold := withholdGrowingLast && g == lastIdx
			if rendered := renderProse(md, step, contentW, withhold); rendered != "" {
				if afterTool {
					b.WriteByte('\n')
				}
				b.WriteString(rendered)
				if !strings.HasSuffix(rendered, "\n") {
					b.WriteByte('\n')
				}
			}
		case StepTool:
			// A contiguous run of tool steps renders as one branch tree; only the
			// FIRST step of a group emits the whole group (so last-branch math works).
			if gi < len(groups) && groups[gi].first == sub[li].Activity {
				b.WriteString(renderToolGroup(th, groups[gi].acts, expanded, spinnerFrame, now, width))
				b.WriteByte('\n')
				gi++
			}
		case StepNote:
			if step.Note != nil {
				if afterTool {
					b.WriteByte('\n')
				}
				b.WriteString(renderInlineNote(th, *step.Note, width))
				b.WriteByte('\n')
			}
		case StepInterject:
			if rendered := renderInterjection(th, step.Text, width); rendered != "" {
				if afterTool {
					b.WriteByte('\n')
				}
				b.WriteString(rendered)
				b.WriteByte('\n')
			}
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// hasResponded reports whether the turn has produced ANY rendered output (a prose,
// tool, or note step) — i.e. the "◆ DAINTREE" marker belongs above it. Gates the marker
// on the SEALED render so it matches what the incremental flush already committed (see
// the renderTurn marker comment).
func hasResponded(t *TurnCell) bool {
	return len(t.Steps) > 0
}

// hasProse reports whether the turn has produced any prose text yet.
func hasProse(t *TurnCell) bool {
	for _, s := range t.Steps {
		if s.Kind == StepProse && s.Text != "" {
			return true
		}
	}
	return false
}

// renderProse renders one prose step. When withholdGrowing is false (the live footer, a
// sealed turn, or an earlier prose step a later tool batch has since closed) it renders the
// WHOLE step as markdown. In the live footer that means the still-growing final paragraph is
// reprocessed as markdown every frame, so prose streams smoothly token by token. This is the
// ONLY place incomplete markdown is rendered — a half-typed code fence or bold span reflows
// until it closes — but that churn is confined to the EPHEMERAL footer; nothing partial is
// ever committed to scrollback.
//
// When withholdGrowing is true (the COMMIT-bound flush path only) it returns the IMMUTABLE
// row prefix that may flush to native scrollback — everything that can no longer change as
// more tokens arrive. Two regimes, picked by the still-growing final paragraph's source text:
//
//   - PLAIN tail (proseTailIsPlain): the growing paragraph is settling LINE BY LINE. We render
//     the WHOLE step (byte-identical to the footer's render — same md.Render call, same cache
//     entry) and drop only its LAST visual row. Glamour word-wraps greedily, so appending text
//     mutates ONLY the last visual row — every earlier wrapped row is closed and final. So the
//     full render minus its last row is a byte-exact ROW prefix of the footer/seal render, and
//     prose flows into scrollback a line at a time (the footer holds just the partial last line
//
//   - the live status) instead of a paragraph at a time. THIS is what kills the old churn:
//     before, a paragraph taller than the 8-row footer cap (view.go) scrolled its head out of
//     the capped window each token, then jumped in whole on seal. Now nothing accumulates.
//
//   - MARKDOWN-risky tail: a half-typed inline span (**bold**, `code`, [link]), a bare URL that
//     GFM linkify will style, or a block that restyles earlier rows (setext heading, list)
//     means an "earlier" row is NOT final until the construct closes. So we fall back to settling
//     PARAGRAPH BY PARAGRAPH: render only the text up to the last blank line ("\n\n"); the growing
//     paragraph stays live in the footer (rendered in full there) and commits only when it seals.
//     "\n\n" is the only safe boundary because CommonMark joins single-newline lines into one
//     reflowing paragraph. The growing paragraph can still churn inside the footer cap here, but
//     that path is now reserved for the rare markdown-heavy tail rather than every plain paragraph.
//
// withholdGrowing is decided by POSITION + path (is this the turn's last step, in the flush
// render), never by the step's sticky Streaming flag, so a half-rendered paragraph can never
// be frozen mid-turn into scrollback. proseTailIsPlain examines ONLY the growing tail, so a
// committed plain row holds no markdown character that later text could restyle.
//
// LIMITATION (shared with the pre-existing flush — this is NOT new to line-level streaming):
// the byte-exact prefix relies on a settled row rendering independently of later text. The
// proseTailIsPlain guard makes that hold for every INLINE construct (it rejects the tail the
// instant a delimiter / link trigger appears, before that row could settle). The residual gap is
// a RETROACTIVE block built by appending a newline below a plain line we already committed —
// a setext "===" / "---" underline or a definition-list ": def" — which restyles the line above.
// sealTail's row-count fallback absorbs this without dup/loss (the scrollback copy just keeps the
// plain styling), and LLM prose uses ATX "#" headings and blank-line paragraphs, so it is
// vanishingly rare.
func renderProse(md *markdown.Renderer, step TurnStep, contentW int, withholdGrowing bool) string {
	if step.Text == "" {
		return ""
	}
	if !withholdGrowing {
		return strings.TrimRight(md.Render(step.Text, contentW, false).ANSI, "\n")
	}
	// Flush path. The growing tail is the text after the last completed paragraph ("\n\n").
	idx := strings.LastIndex(step.Text, "\n\n")
	tail := step.Text
	if idx >= 0 {
		tail = step.Text[idx+2:]
	}
	if proseTailIsPlain(tail) {
		// Line-level commit: render the FULL step (the exact bytes the footer shows) and drop
		// the last visual row — the one row greedy word-wrap may still mutate. The result is a
		// byte-exact row prefix of the footer render, so a committed line never re-renders.
		full := strings.TrimRight(md.Render(step.Text, contentW, false).ANSI, "\n")
		rows := strings.Split(full, "\n")
		rows = rows[:len(rows)-1] // drop the still-mutable last visual row
		// Drop any trailing BLANK rows the last row left exposed — the paragraph separator that
		// precedes the growing paragraph. It belongs with the still-live tail below it, and the
		// flush↔seal reconciliation forbids the committed prefix ending in a blank (it would be
		// re-committed on seal). It re-settles naturally once the growing paragraph commits a row.
		for len(rows) > 0 && strings.TrimSpace(stripAnsi(rows[len(rows)-1])) == "" {
			rows = rows[:len(rows)-1]
		}
		return strings.Join(rows, "\n") // "" when only a single visual row has formed so far
	}
	// Markdown-risky tail: settle paragraph by paragraph — commit only the completed paragraphs.
	if idx < 0 {
		return "" // no completed paragraph yet — withhold the whole still-growing step from the flush
	}
	return strings.TrimRight(md.Render(step.Text[:idx], contentW, false).ANSI, "\n")
}

// proseTailIsPlain reports whether the still-growing final paragraph `tail` (the raw markdown
// source after the last "\n\n") is plain prose that glamour will only ever APPEND-WRAP — so its
// settled wrapped rows can flush to scrollback line by line without any later token restyling an
// already-committed row (see renderProse). It is deliberately conservative: a single false from a
// borderline tail merely falls back to paragraph-level commit (correct, just less smooth), while a
// wrong true would freeze a row that reflows. So we reject anything that could open an inline span,
// a retroactive block, or a multi-line construct:
//
//   - empty tail — nothing to settle.
//   - any inline/markdown-significant char: an unclosed **bold** / _em_ / `code` / [link] /
//     <autolink> / entity (&) / escape (\) / table pipe (|) / strikethrough (~) restyles earlier
//     bytes when it closes; "#" / ">" anywhere are cheapest to reject wholesale.
//   - a GFM autolink trigger ("://", "www.", "@"): glamour enables GFM linkify, so a bare URL or
//     email gets link styling — and because it styles the WHOLE token (an OSC-8 target can even
//     embed the full URL on every wrapped row), extending it as it streams rewrites earlier rows.
//     A partial URL stays on the still-live last row until a trigger appears, so rejecting on the
//     trigger keeps any part of the link out of the committed prefix.
//   - any newline or tab: a soft break can become a setext underline, a hard break, a definition
//     list, or list/blockquote continuation; a tab is block indentation (indented code) — all
//     re-wrap or re-style the lines above.
//   - a leading block opener ("- ", "+ ", "N. ", "N) ", or a >=4-space indent = indented code):
//     these render the whole tail as a list item / code block, not a paragraph.
func proseTailIsPlain(tail string) bool {
	if tail == "" {
		return false
	}
	// Inline spans and retroactive blocks all announce themselves with one of these runes; a
	// committed row containing none of them is pure text nothing downstream can restyle.
	if strings.ContainsAny(tail, "*_`[]<>&\\|~#") {
		return false
	}
	// GFM linkify: a bare URL / email restyles its whole token as it grows, so reject the tail the
	// moment a scheme ("://"), a "www." host, or an email "@" appears.
	if strings.Contains(tail, "://") || strings.Contains(tail, "www.") || strings.Contains(tail, "@") {
		return false
	}
	if strings.ContainsAny(tail, "\n\t") {
		return false
	}
	// Leading block markers (the tail is a single line, so position 0 is the only line start).
	t := strings.TrimLeft(tail, " ")
	if len(tail)-len(t) >= 4 {
		return false // >=4-space indent → indented code block
	}
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "+ ") {
		return false // bullet list item
	}
	// Ordered-list opener: one or more digits then "." or ")" then a space.
	d := 0
	for d < len(t) && t[d] >= '0' && t[d] <= '9' {
		d++
	}
	if d > 0 && d < len(t) && (t[d] == '.' || t[d] == ')') {
		if d+1 < len(t) && t[d+1] == ' ' {
			return false
		}
	}
	return true
}

// renderInlineNote renders a SystemNote attached to a turn.
func renderInlineNote(th theme.Theme, n SystemNote, width int) string {
	g := th.Glyphs
	glyph, tone := noteGlyph(th, n.Level)
	// Note row: a single toned span carries the continuation spine
	// + the toned glyph + a separating space ("│ " already ends in a space), then the
	// note text renders at BODY color (a bare child — NOT dim). The spine tone is
	// green info / yellow warn / red error.
	spine := styleFor(th, tone, g.Continuation+glyph+" ")
	return truncateCells(spine+th.Body().Render(n.Text), width)
}

// renderInterjection renders a message the user typed mid-turn as an inline aside in
// the running turn: the continuation spine + the user accent bar (▏, the same cue the
// YOU card uses) + the wrapped text, one spine+bar per visual row. It reuses the
// user-message surface so a mid-turn steer reads unmistakably as the human's, distinct
// from the model's prose or a system note. Text is fixed once folded in, so the row
// count is stable across streaming and seal (the flush boundary relies on that).
func renderInterjection(th theme.Theme, text string, width int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	g := th.Glyphs
	surface := th.UserMessageSurface()
	barStyle := th.Muted()
	if surface.Bar != nil {
		barStyle = th.Dim().Foreground(surface.Bar)
	}
	textStyle := th.Body()
	if surface.Text != nil {
		textStyle = textStyle.Foreground(surface.Text)
	}
	prefix := th.Dim().Render(g.Continuation) + barStyle.Render(g.Bar) + " "
	// Reserve the spine (2) + bar (1) + gap (1); one prefix per wrapped row.
	inner := width - 4
	if inner < 10 {
		inner = 10
	}
	var b strings.Builder
	first := true
	for _, para := range strings.Split(text, "\n") {
		for _, line := range strings.Split(wrapCells(para, inner), "\n") {
			if !first {
				b.WriteByte('\n')
			}
			first = false
			b.WriteString(prefix + textStyle.Render(truncateCells(line, inner)))
		}
	}
	return b.String()
}

// noteGlyph maps a note level to (glyph, tone):
// error → failed glyph + danger, warn → attention mark + warning, else → bullet "·"
// + the "active" tone (cyan info — NOT accent green).
func noteGlyph(th theme.Theme, level NoteLevel) (string, string) {
	g := th.Glyphs
	switch level {
	case NoteError:
		return g.Failed, "danger"
	case NoteWarn:
		// Attention mark from the GlyphSet (unicode "»" / ASCII "!").
		return g.Attention, "warning"
	case NoteSuccess:
		// A filled connection dot in accent green (the "● Connected to Daintree MCP"
		// status line). ASCII fallback uses the done glyph so it never renders blank.
		if g.Brand == "#" { // ASCII glyph set
			return g.Done, "success"
		}
		return "●", "success"
	default:
		return g.Bullet, "info"
	}
}

// toolGroup is a contiguous run of StepTool activities rendered as one branch tree.
type toolGroup struct {
	first *Activity
	acts  []Activity
}

// collectToolGroups walks a step slice and groups contiguous StepTool runs. `first`
// holds the LIVE Activity pointer of each group's first step (pointer-identity matched
// by the render loop), so the slice passed in must be a sub-slice of the turn's Steps
// (not a copy) for the identity check to hold.
func collectToolGroups(steps []TurnStep) []toolGroup {
	var groups []toolGroup
	var cur []Activity
	var firstPtr *Activity
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, toolGroup{first: firstPtr, acts: cur})
			cur = nil
			firstPtr = nil
		}
	}
	for i := range steps {
		s := steps[i]
		if s.Kind == StepTool && s.Activity != nil {
			if firstPtr == nil {
				firstPtr = steps[i].Activity
			}
			cur = append(cur, *s.Activity)
		} else {
			flush()
		}
	}
	flush()
	return groups
}

// renderToolGroup renders a contiguous activity run as a branch tree. In the DEFAULT
// (collapsed) view a FINISHED homogeneous read/inspect batch compacts to one summary row
// (ui-transcript.md §4 / _interaction-ux.md §6: "✓ Inspected 6 files · 412ms"); ^X /
// expanded reveals the individual rows again. Compaction is a pure function of
// (expanded, activities) so the incremental flush and the seal render it identically.
func renderToolGroup(th theme.Theme, acts []Activity, expanded bool, spinnerFrame int, now int64, width int) string {
	if !expanded {
		if label, total, ok := compactableBatch(acts); ok {
			return renderBatchSummaryRow(th, label, total, width)
		}
	}
	var b strings.Builder
	for i, a := range acts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderActivityRow(th, a, i == len(acts)-1, expanded, spinnerFrame, now, width))
	}
	return b.String()
}

// batchSummaryTemplate maps a compactable read/inspect tool to its "%d"-pluralized batch
// label. ONLY the high-frequency, read-only tools collapse — every other batch renders
// each row so distinct verbs/targets stay visible. "" means "never compact this tool".
func batchSummaryTemplate(name string) string {
	switch name {
	case "fs.read":
		return "Inspected %d files"
	case "fs.list":
		return "Listed %d directories"
	case "fs.search":
		return "Searched %d times"
	case "terminal.read":
		return "Read %d terminals"
	default:
		return ""
	}
}

// compactableBatch decides whether a contiguous tool run collapses to one summary row. It
// qualifies only when the run has ≥2 calls that are ALL the same compactable tool AND ALL
// succeeded — a single call is already minimal, and a failure/cancel must stay expanded so
// its outcome is never hidden behind a tidy summary. Returns the rendered label and the
// batch's wall-clock duration (first start → last end).
func compactableBatch(acts []Activity) (label string, totalMs int64, ok bool) {
	if len(acts) < 2 {
		return "", 0, false
	}
	tmpl := batchSummaryTemplate(acts[0].Name)
	if tmpl == "" {
		return "", 0, false
	}
	var minStart, maxEnd int64
	for _, a := range acts {
		if a.Name != acts[0].Name || a.State != ActDone {
			return "", 0, false
		}
		if a.StartedAt > 0 && (minStart == 0 || a.StartedAt < minStart) {
			minStart = a.StartedAt
		}
		if a.EndedAt > maxEnd {
			maxEnd = a.EndedAt
		}
	}
	if minStart > 0 && maxEnd >= minStart {
		totalMs = maxEnd - minStart
	}
	return fmt.Sprintf(tmpl, len(acts)), totalMs, true
}

// renderBatchSummaryRow renders the collapsed batch row "<branch> ✓ <label> <total>", with
// the duration right-aligned into the same gutter the per-row tree uses so a compacted and
// an expanded batch share one column grid. The batch is a closed group → the last branch.
func renderBatchSummaryRow(th theme.Theme, label string, totalMs int64, width int) string {
	g := th.Glyphs
	var b strings.Builder
	b.WriteString(th.Muted().Render(g.BranchLast))
	b.WriteByte(' ')
	b.WriteString(styleFor(th, "success", g.Done))
	b.WriteByte(' ')
	b.WriteString(th.Body().Render(label))
	if totalMs > 0 {
		right := formatDuration(totalMs)
		used := cellWidth(b.String())
		if p := width - cellWidth(right) - used; p > 0 {
			b.WriteString(strings.Repeat(" ", p))
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(th.Dim().Render(right))
	}
	return truncateCells(b.String(), width)
}
