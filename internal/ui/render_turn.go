package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/composer"
	"github.com/daintreehq/assistant/internal/ui/markdown"
	"github.com/daintreehq/assistant/internal/ui/theme"
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
	// Colors come from the theme's UserMessageSurface (a cool neutral gray, NOT accent
	// green — green is reserved for Daintree's identity). The "YOU" anchor sits ABOVE
	// the block (inlineLabel=false): the turn-opening card is the widest thing on the
	// screen, so a floating anchor reads as a heading for the exchange rather than as
	// the card's first row.
	return renderCard(th, th.UserMessageSurface(), "YOU", strings.Split(text, "\n"), width, false)
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

// renderCard draws the cockpit's ONE card idiom: a labelled body where every body row is
// a left accent bar (▏) butted against a contiguous fill block. It backs all three cards —
// the turn-opening "YOU" message, the human's mid-turn message, and the inline "Skill
// loaded" note — so their geometry can never drift apart, and any width fix lands once.
//
// inlineLabel picks the label's home. false puts it on its OWN bare line above the block
// (the turn-opening YOU card: a heading for the whole exchange, faint and unbold, because
// the block below already carries the "this is yours" signal). true makes it the block's
// first row, bold (the compact inline cards folded into a running turn, where a floating
// anchor would read as detached from the round it belongs to).
//
// Every row is bar + blockW == width-1 cells regardless of mode, so a card committed to
// scrollback can never wrap a frozen row when the host shrinks — the geometry is derived
// from the width, never from a fixed minimum that could outrun it.
// cardRowBudget is the total cell width of ONE rendered card row (bar + block). Every
// rendered row must stay within `width` (the chrome width chromeW = columns - gutter -
// LeftPad): the card is LeftPad-indented at commit and the gutter reserves the host
// autowrap column, so a row <= width can never wrap a frozen scrollback line. width-1
// keeps one extra column spare. Deriving the geometry from the width (NOT from a fixed
// minimum, which could outrun it on a narrow terminal and wrap) makes every width safe.
func cardRowBudget(width int) int {
	if width-1 < 1 {
		return 1
	}
	return width - 1
}

// cardInner is the text width inside a card row. Uniform geometry in EVERY mode: reserve
// bar(1) + gap(1) + a 1-col right margin, so the text wrap width is identical whether or
// not a fill is drawn (the fill just paints that right margin; the fallback leaves it as
// spare). Keeping it mode-independent makes the rendered row count stable across themes.
// Exported to the package so a caller that must pre-fit its own text to one row
// (renderQueuedInjections) measures against the same budget the renderer will apply.
func cardInner(width int) int {
	inner := cardRowBudget(width) - 3
	if inner < 1 {
		return 1
	}
	return inner
}

func renderCard(th theme.Theme, surface theme.UserMessageSurface, label string, lines []string, width int, inlineLabel bool) string {
	g := th.Glyphs
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
	labelStyle := th.Dim()
	if surface.Label != nil {
		labelStyle = th.Body().Foreground(surface.Label)
	}
	if inlineLabel {
		// Inside the block the label has to out-rank the body rows beside it.
		labelStyle = labelStyle.Bold(true)
	}

	rowBudget := cardRowBudget(width)
	inner := cardInner(width)
	// Fill needs room for the whole block (bar + gap + text + margin); below that, fall
	// back to the plain bar.
	useFill := surface.Fill != nil && rowBudget >= 5
	blockW := inner + 2 // gap + text + right margin; bar + blockW == rowBudget
	// …and below TWO cells even the plain bar overflows: `inner` floors at 1, so bar + gap +
	// one content cell is 3 cells however narrow the terminal gets. At width 1-2 (chromeWidth
	// floors at 1, so this is reachable) the chrome itself is what breaks the row, and an
	// over-wide row is the one defect that outlives its frame — committed to scrollback it
	// wraps forever. So the chrome goes and the text stays.
	useChrome := rowBudget >= 2

	var b strings.Builder
	// first suppresses the row separator before the very first line written, so the
	// result carries no leading or trailing newline either way.
	first := true
	// writeRow emits one body row: bar + (fill block | bar-only fallback). Factored
	// out so the label, the head, the tail, and the middle trim rule all share the exact
	// same geometry — every row is bar + blockW == rowBudget cells, with the fill (or the
	// lone bar) carrying the cue.
	writeRow := func(content string, style lipgloss.Style) {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		if useFill {
			// Fixed-width fill: a leading space is the gap after the bar; lipgloss
			// pads the remainder of blockW with the background, producing a clean
			// rectangle that meets the bar with no unfilled seam.
			block := style.Background(surface.Fill).Width(blockW).
				Render(" " + truncateCells(content, inner))
			b.WriteString(bar + block)
		} else if useChrome {
			// No fill (ansi/none, or a terminal too narrow for a block): the bar
			// alone carries the cue.
			b.WriteString(bar + " " + style.Render(truncateCells(content, inner)))
		} else {
			// No room for chrome at all — text only, clipped to the row.
			b.WriteString(style.Render(truncateCells(content, rowBudget)))
		}
	}
	// writeParagraph wraps one explicit paragraph (hard \n breaks preserved, matching
	// wrapText) to inner, one bar + block per visual row so the gutter stays aligned.
	writeParagraph := func(para string) {
		for _, line := range strings.Split(wrapCells(para, inner), "\n") {
			writeRow(line, textStyle)
		}
	}

	if inlineLabel {
		writeRow(label, labelStyle)
	} else {
		// The bare label carries no bar or gap, so its budget is the full width — but it
		// still has to be clipped: an un-truncated "YOU" is 3 cells and would overflow a
		// 1-2 column terminal exactly like a body row would.
		b.WriteString(labelStyle.Render(truncateCells(label, width)))
		first = false // the next row still needs its separator
	}

	// A very long paste is shown as head + a "N lines hidden" rule + tail instead of
	// in full, so it can't bury the conversation in scrollback (the committed card
	// is otherwise as tall as the paste — see flush.go's chunked commit). Trimming is
	// by LOGICAL line — what the human actually pasted — and the split deliberately
	// favors the TAIL: a pasted log or stack trace usually carries its payoff at the
	// bottom, while the head only has to be enough to recognize what was pasted. We
	// collapse only when it hides at least 2 lines (len > head+tail+1) — replacing a
	// single hidden line with a one-row rule would save nothing. The rule itself rides
	// the same fill block (renderHiddenRule), so the card stays one contiguous surface.
	if len(lines) > userMsgHeadLines+userMsgTailLines+1 {
		for _, para := range lines[:userMsgHeadLines] {
			writeParagraph(para)
		}
		// The rule recedes to chrome: the faint Label tone (the same quiet hue as the
		// anchor), or a plain dim attribute where the theme has no Label color.
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
	// prevRendered is the kind of the last step that actually PUT SOMETHING on screen, not
	// simply the previous step. A step can render nothing — a whitespace-only prose step is
	// the reachable case (appendProse rejects only "") — and keying the separator off the raw
	// predecessor let such a step stand between a block and its gap, silently eating the
	// blank line. Tracking what was rendered makes the spacing depend on what the reader can
	// actually see. It is derived from steps [from..g) exactly like the raw index was, and
	// every caller renders from step 0, so flush and seal still agree row for row.
	prevRendered := TurnStepKind(-1) // nothing rendered yet
	for li := range sub {
		step := sub[li]
		g := from + li
		// A blank line AFTER a "block" step — a tool group, a skill card, OR the human's
		// mid-turn message — separates the function-call ledger / card from the prose or note
		// that follows it. The blank rides the FOLLOWING step (a leading blank) so it survives
		// the flush boundary: when the block flushes alone, the blank flushes with the prose
		// later, keeping spacing identical across streaming and seal (a TRAILING blank would be
		// stripped by the final TrimRight and lost).
		afterBlock := prevRendered == StepTool || prevRendered == StepSkill || prevRendered == StepInterject
		switch step.Kind {
		case StepProse:
			withhold := withholdGrowingLast && g == lastIdx
			if rendered := renderProse(md, step, contentW, withhold); rendered != "" {
				if afterBlock {
					b.WriteByte('\n')
				}
				b.WriteString(rendered)
				if !strings.HasSuffix(rendered, "\n") {
					b.WriteByte('\n')
				}
				prevRendered = StepProse
			}
		case StepTool:
			// A contiguous run of tool steps renders as one branch tree; only the
			// FIRST step of a group emits the whole group (so last-branch math works).
			if gi < len(groups) && groups[gi].first == sub[li].Activity {
				if afterBlock {
					b.WriteByte('\n')
				}
				b.WriteString(renderToolGroup(th, groups[gi].acts, expanded, spinnerFrame, now, width))
				b.WriteByte('\n')
				gi++
			}
			// Every step of the run counts as rendered: the group's own rows are already on
			// screen, so a step that only folded into it must not reset the separator state.
			prevRendered = StepTool
		case StepNote:
			if step.Note != nil {
				if afterBlock {
					b.WriteByte('\n')
				}
				b.WriteString(renderInlineNote(th, *step.Note, width))
				b.WriteByte('\n')
				prevRendered = StepNote
			}
		case StepInterject:
			if rendered := renderInterjection(th, step.Text, width); rendered != "" {
				// The human's mid-turn message is the ONE step that takes a blank line above it
				// unconditionally — not just after a block (afterBlock), the way a skill card
				// does. A skill card belongs to the round below it, so gluing it to the content
				// above reads correctly; a message typed while the model was streaming is a hard
				// break in the narrative, and butted against the paragraph above it, it read as
				// one more line of the model's own prose.
				//
				// UNCONDITIONAL, including as the turn's first step: an injection folds in at the
				// TOP of a round, so it can land before any prose or tool step exists, and the
				// blank is what then separates it from the "◆ DAINTREE" marker the preamble
				// renders directly above (callers join preamble and body with a single newline,
				// so without this the card butts against the marker). Position-independent, so
				// flush and seal — which both render from step 0 — always agree.
				b.WriteByte('\n')
				b.WriteString(rendered)
				b.WriteByte('\n')
				prevRendered = StepInterject
			}
		case StepSkill:
			if rendered := renderSkillCard(th, step.Text, width); rendered != "" {
				// The card carries the SAME asymmetric spacing as the "YOU" message card it
				// mirrors: a blank line BELOW (afterBlock on the next step) but NONE above, so
				// it reads as linked to whatever precedes it (the ◆ DAINTREE marker, at the
				// start of a turn) rather than floating in its own gap. The only leading blank
				// is the block-separator one a tool group / preceding card needs (afterBlock).
				if afterBlock {
					b.WriteByte('\n')
				}
				b.WriteString(rendered)
				b.WriteByte('\n')
				prevRendered = StepSkill
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

// hasToolStep reports whether the turn ATTEMPTED any tool call. A StepTool is appended
// when the batch is ANNOUNCED (not when it settles), so this is the local equivalent of
// the backend's toolCallCount > 0 — announced-then-failed still counts as action taken,
// because the ledger row is visible in scrollback either way.
func hasToolStep(t *TurnCell) bool {
	for _, s := range t.Steps {
		if s.Kind == StepTool {
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
//   - COMMITTABLE tail (proseTailCommittable: no BLOCK-level construct): the growing paragraph is
//     settling LINE BY LINE. We render the WHOLE step (byte-identical to the footer's render — same
//     md.Render call, same cache entry), drop the still-mutable LAST visual row, then keep only the
//     leading rows that hold NO openable inline delimiter (rowHasOpenableDelimiter). Glamour prints
//     an UNCONSUMED emphasis/code delimiter literally, so a row with none is width-final — a CLOSED
//     **bold**/`code` is pure styling and safe, while an OPEN span shows a literal ` / * / _ on its
//     row and is held back until it closes. So the kept rows are a byte-exact ROW prefix of the
//     footer/seal render, and prose flows into scrollback a line at a time (the footer holds just
//     the open-span tail + partial last line + live status). THIS is what kills the churn: before,
//     the whole growing paragraph was withheld and piled into the 8-row footer cap (view.go),
//     scrolling its head off the top each token, then jumping in whole on seal. Now nothing
//     accumulates — even for markdown-dense prose, because a CLOSED span no longer disqualifies the
//     paragraph and an OPEN one only withholds its own (short) row.
//
//   - WITHHELD tail: the tail holds a BLOCK-level / cross-row construct — a link / bare URL /
//     entity / escape / table / strikethrough / heading, or a newline (setext, list, code fence,
//     definition list). These can restyle EARLIER rows, so we fall back to settling PARAGRAPH BY
//     PARAGRAPH: render only the text up to the last blank line ("\n\n"); the growing paragraph
//     stays live in the footer and commits only when it seals.
//
// withholdGrowing is decided by POSITION + path (is this the turn's last step, in the flush
// render), never by the step's sticky Streaming flag, so a half-rendered paragraph can never be
// frozen mid-turn into scrollback. The commit is sound against any future append — see
// TestFuzz_CommittedRowsNeverChange.
//
// LIMITATION: the byte-exact prefix relies on a committed row rendering independently of later
// text. rowHasOpenableDelimiter guarantees that for code spans and well-formed emphasis. Two
// residual gaps remain, both INHERENT to committing within an unsealed paragraph (they apply to the
// pre-existing plain-prose line-commit too, not just this markdown extension) and both requiring
// syntax LLM prose essentially never streams — it uses ATX "#" headings, blank-line paragraphs, and
// well-formed spans:
//
//	(1) a RETROACTIVE block appended below an already-committed line — a setext "===" / "---"
//	    underline or a definition-list ": def" — re-renders that line as a heading/term with a
//	    different row structure. sealTail then can't strip the committed prefix exactly and its
//	    row-count fallback may RE-EMIT a row (a visible duplicate, not just stale styling). Text is
//	    preserved (no loss); the artifact is a rare duplicated line.
//	(2) PATHOLOGICAL raw emphasis soup ("_a_b_c" mid-stream), where CommonMark re-pairs a consumed
//	    span globally so a styled row later un-styles. flushActiveTurn's reflow guard then HOLDS
//	    further commits; the artifact is one row of cosmetically-stale styling (no dup, no loss).
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
	if proseTailCommittable(tail) {
		// Line-level commit: render the FULL step (the exact bytes the footer shows) and keep only
		// the IMMUTABLE rows.
		full := strings.TrimRight(md.Render(step.Text, contentW, false).ANSI, "\n")
		rows := strings.Split(full, "\n")
		if len(rows) > 0 {
			rows = rows[:len(rows)-1] // drop the still-mutable last visual row (greedy wrap mutates it)
		}
		// STOP at the first row holding an OPENABLE inline delimiter. glamour prints an UNCONSUMED
		// emphasis/code delimiter (`, *, or a boundary _) LITERALLY in its output; a future closer
		// can pair with it and restyle + re-wrap that row. A CLOSED span renders as pure styling
		// (no literal delimiter left), so its rows are width-final and safe. Truncating here makes
		// the committed prefix sound against ANY future append — see TestFuzz_CommittedRowsNeverChange
		// — while still committing all the plain text before an open span.
		safe := 0
		for safe < len(rows) && !rowHasOpenableDelimiter(stripAnsi(rows[safe])) {
			safe++
		}
		rows = rows[:safe]
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
	return renderCompletedBlocks(md, step.Text[:idx], contentW)
}

// renderCompletedBlocks renders the COMPLETED-block prefix `text` (everything up to the last "\n\n")
// for the commit, in a form that is STABLE under appending the blocks that follow. glamour renders
// some blocks differently depending on whether ANOTHER block follows them — most importantly it pads
// a trailing ATX heading ("### Title") to the full content width only when a block follows it, and
// leaves it unpadded when it is the last block. So a heading committed while it is the last completed
// block (unpadded) RE-RENDERS padded the instant the next block (e.g. a bullet list) seals — changing
// an already-committed row, tripping flushActiveTurn's reflow guard, and FREEZING the flush for the
// rest of the turn. Every block after it then piles into the height-capped footer and churns until
// the final seal — the user-reported "title + blank line + bullet list" churn.
//
// The fix renders `text` with a trailing SENTINEL block appended, so every real block is in its
// "followed by content" form (the same form the footer and the seal render it in, since live content
// always follows). The sentinel's own row + its blank separator are then dropped. The result is a
// byte-stable prefix: it no longer changes when the real next block arrives.
func renderCompletedBlocks(md *markdown.Renderer, text string, contentW int) string {
	full := strings.TrimRight(md.Render(text+"\n\nx", contentW, false).ANSI, "\n")
	rows := strings.Split(full, "\n")
	if len(rows) > 0 {
		rows = rows[:len(rows)-1] // drop the sentinel "x" paragraph's row
	}
	// Drop the blank separator the sentinel sat below (and any other trailing blanks), so the
	// committed prefix never ends in a blank (which the flush↔seal reconciliation forbids).
	for len(rows) > 0 && strings.TrimSpace(stripAnsi(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	return strings.Join(rows, "\n")
}

// proseTailCommittable reports whether the still-growing final paragraph `tail` (the raw markdown
// source after the last "\n\n") is free of BLOCK-level / document-global constructs that would force
// withholding the whole paragraph. It does NOT judge inline spans — those are handled per-ROW in
// renderProse via rowHasOpenableDelimiter, so a CLOSED **bold** / `code` in mid-paragraph no longer
// disqualifies everything after it (the bug that made markdown-dense prose churn end to end).
//
// Conservative: any doubt → false → paragraph-level fallback (correct, just less smooth). We reject:
//
//   - empty tail — nothing to settle.
//   - a retroactive / width-changing / OSC-8 construct anywhere: "[" "]" "<" ">" (links/html emit
//     OSC-8 and restyle), "&" (entity &amp;→& shrinks the line), "\\" (escape), "|" (table), "~"
//     (strikethrough), "#" (heading). These can reach back across rows, so reject wholesale.
//   - a GFM autolink trigger ("://", "www.", "@"): a bare URL/email is styled with an OSC-8 target
//     embedded per wrapped row, so growing it rewrites earlier rows.
//   - any newline or tab: a newline can form a setext underline, a hard break, a list, a code
//     fence, or a definition list (all restyle/re-wrap earlier lines); a tab is block indentation.
//   - a leading block opener ("- ", "+ ", "N. ", "N) ", or a >=4-space indent = indented code).
func proseTailCommittable(tail string) bool {
	if tail == "" {
		return false
	}
	// Retroactive / width-changing / OSC-8 / link constructs — can reach back across committed rows.
	if strings.ContainsAny(tail, "[]<>&\\|~#") {
		return false
	}
	// GFM linkify: a bare URL / email restyles its whole token as it grows.
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
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "+ ") || strings.HasPrefix(t, "* ") {
		return false // bullet list item (incl. "* " — a leading "*" + space is a bullet, not emphasis)
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

// rowHasOpenableDelimiter reports whether a RENDERED row (ansi-stripped) holds an inline delimiter
// that a future closer could still pair with — making the row unsafe to commit. glamour prints an
// UNCONSUMED emphasis/code delimiter literally, so any literal "*" or "`" is openable, and an "_" is
// openable UNLESS it is strictly intraword (an ASCII word char on both sides, e.g. snake_case, which
// CommonMark treats as plain text). A CLOSED span leaves NO literal delimiter (it became styling),
// so a row with none is width-final. This is the soundness gate for the line-level commit
// (renderProse).
//
// The intraword test is ASCII-only ON PURPOSE: at the byte level a non-ASCII neighbour is a UTF-8
// lead/continuation byte that could be a LETTER or a PUNCTUATION mark (e.g. an em-dash "—"), and
// CommonMark makes "_" after punctuation an opener. We can't tell which cheaply, so any non-ASCII
// neighbour makes the "_" openable (conservative — withholds the row; never under-withholds).
func rowHasOpenableDelimiter(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '`':
			return true
		case '_':
			leftWord := i > 0 && isAsciiWordByte(s[i-1])
			rightWord := i+1 < len(s) && isAsciiWordByte(s[i+1])
			if !(leftWord && rightWord) {
				return true // not strictly intraword → could open emphasis
			}
		}
	}
	return false
}

// isAsciiWordByte reports whether b is an ASCII word character (letter, digit, or "_"). Non-ASCII
// bytes are deliberately NOT word characters here — see rowHasOpenableDelimiter for why.
func isAsciiWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_'
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

// interjectLabel anchors the mid-turn message card. It leads with the same "YOU" token as
// the turn-opening card (so the human's own words are recognizable at a glance wherever
// they appear) and names WHEN it arrived, because that is the one thing the position alone
// cannot say: the message was typed while the model was working and folded in HERE, at the
// round boundary the model actually read it — not at the top of a new exchange.
const interjectLabel = "YOU · MID-TURN"

// renderInterjection renders a message the user typed mid-turn as an inline card folded
// into the running turn: the interjectLabel anchor over the wrapped text, both riding the
// same accent bar + fill block the YOU card uses (renderCard).
//
// The card, rather than the lone bar this used to draw, is the point. A bar'd row alone
// reads as one more branch of the tool tree or a continuation of the paragraph above it —
// exactly why the YOU card grew a fill in the first place (see theme.UserMessageSurface)
// — so a steer typed mid-stream vanished into the model's prose. The fill makes the
// human's words a distinct surface no matter where in the turn they land; renderTurnSteps
// gives the card a blank line on BOTH sides so it can't be glued to that prose either.
//
// Text is fixed once folded in, so the row count is stable across streaming and seal (the
// flush boundary relies on that).
func renderInterjection(th theme.Theme, text string, width int) string {
	// Trailing newlines would leave a stray blank fill row at the bottom of the card.
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return renderCard(th, th.InterjectionSurface(), interjectLabel, strings.Split(text, "\n"), width, true)
}

// queuedPreviewMax bounds how many queued messages the footer card lists. Beyond it the
// tail collapses to a "+N more" row, so the card is at most 1 anchor + 3 rows however many
// follow-ups are stacked up. The bound is a HEIGHT contract, not a style choice: this card
// rides the fixed bottom band, which is never truncated — an unbounded list would push the
// composer off a short terminal and trip footer()'s too-small fallback. maxRows narrows it
// further on a short terminal (see queuedInjectionsView).
const queuedPreviewMax = 3

// renderQueuedInjections renders the messages typed while the model is working that the
// Session has buffered but not yet folded into the running turn — the footer's "this is
// waiting to go in" card, shown directly above the composer. Returns "" when nothing is
// queued.
//
// It is deliberately the SAME card as the delivered one (renderInterjection): identical
// bar, fill and surface, differing only in the anchor. A queued message and the mid-turn
// card it later becomes are the same object at two stages of its life, so they should look
// like it — the card slides out of the footer and into the transcript when the model
// actually reads it. Showing the TEXT is the point: a bare count said something was waiting
// without showing what, leaving the user to trust that their message had landed at all.
//
// Each message is flattened to ONE row (newlines to spaces, ellipsized past the card width).
// The band is fixed-height, so a queued paste must cost one row, not its full height.
//
// maxRows caps the WHOLE card, anchor included: 1 leaves the anchor alone (the one-row cue
// this replaced), and 0 or less renders nothing. Rows are spent anchor-first, because the
// count is the part that must survive — a preview with no anchor would not say what it is.
func renderQueuedInjections(th theme.Theme, texts []string, width, maxRows int) string {
	if len(texts) == 0 || maxRows < 1 {
		return ""
	}
	inner := cardInner(width)
	// bound is how many BODY rows are available: the standing cap, narrowed by whatever the
	// terminal can spare once the anchor has taken its row.
	bound := queuedPreviewMax
	if r := maxRows - 1; r < bound {
		bound = r
	}
	var shown []string
	switch {
	case bound < 1:
		// Anchor only — the card degrades to exactly the one-row cue it replaced.
	case len(texts) <= bound:
		for _, text := range texts {
			shown = append(shown, truncateCells(flattenToRow(text), inner))
		}
	default:
		// Over the bound, the LAST row is spent on the overflow count instead of a message,
		// so the card holds its height and the count still accounts for every message the
		// preview does not show. At bound 1 that leaves the count alone.
		preview := bound - 1
		for _, text := range texts[:preview] {
			shown = append(shown, truncateCells(flattenToRow(text), inner))
		}
		shown = append(shown, truncateCells(fmt.Sprintf("+%d more", len(texts)-preview), inner))
	}
	// The anchor is the composer's own cue copy (grammar + width fallback), relocated.
	label := composer.QueuedFollowupLabel(len(texts), inner)
	if label == "" {
		return ""
	}
	return renderCard(th, th.InterjectionSurface(), label, shown, width, true)
}

// flattenToRow collapses a message to a single line: every newline (and any run of
// whitespace around it) becomes one space, so a multi-line paste occupies exactly one row
// of the fixed bottom band instead of its full height.
func flattenToRow(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// renderSkillCard renders a server-side skill load as a compact inline card folded into
// the running turn: a quiet "Skill loaded" anchor over the skill's FULL name, both riding
// a pale blue/turquoise fill block that butts up against a left accent bar (▏ — the same
// surface idiom the YOU card uses, so it reads as a distinct, calm capability cue rather
// than the model's prose or a system note). The card is at least two rows (label + name);
// a long name wraps to more rows, each carrying the fill. Like the YOU card, every row is
// a fixed bar+block width so a committed card never wraps a frozen scrollback row on resize.
// Text is fixed once folded in, so the row count is stable across streaming and seal.
func renderSkillCard(th theme.Theme, title string, width int) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	// Row 1 is the "Skill loaded" anchor; row 2+ is the skill's full name, wrapped.
	return renderCard(th, th.SkillLoadedSurface(), "Skill loaded", []string{title}, width, true)
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
		// A filled settled-good dot in accent green. ASCII fallback uses the done
		// glyph so it never renders blank.
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
