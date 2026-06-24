package ui

import (
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_turn.go renders a TurnCell (and its user message) to a styled string from
// its ordered Steps. The renderer
// takes width + expanded + now and is pure given (cell, theme, md, width, ...) so
// the scrollback queue can re-render frozen blocks fresh on resize.

// renderUserMessage renders the "YOU" card: a dim+bold "YOU"
// label on its OWN line, then the wrapped message body with one left accent bar
// (▏, U+258F) per row. A system-origin turn (UserText == "") renders nothing.
func renderUserMessage(th theme.Theme, text string, width int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	g := th.Glyphs
	// The bar carries the visual weight; the dim+bold "YOU" label is a quiet
	// who-said-what anchor above it. The bar color comes from
	// the theme's UserMessageSurface (a cool neutral gray, NOT accent green — green
	// is reserved for Daintree's identity).
	surface := th.UserMessageSurface()
	barStyle := th.Muted()
	if surface.Bar != nil {
		barStyle = th.Dim().Foreground(surface.Bar)
	}
	bar := barStyle.Render(g.Bar)
	// Body text color: the surface text color, falling back to plain body fg.
	textStyle := th.Body()
	if surface.Text != nil {
		textStyle = textStyle.Foreground(surface.Text)
	}
	var b strings.Builder
	// Quiet anchor on its own line — dim so it never competes with the bar.
	b.WriteString(th.Dim().Bold(true).Render("YOU"))
	// Reserve the bar column (1) + the gap/padding (1) + a right breathing margin
	// (inner = max(10, width-4)); one bar per wrapped row.
	inner := width - 4
	if inner < 10 {
		inner = 10
	}
	// Wrap each explicit paragraph (hard \n breaks preserved, matching wrapText), one
	// bar per visual row so the gutter stays aligned with whatever we show.
	for _, para := range strings.Split(text, "\n") {
		wrapped := wrapCells(para, inner)
		for _, line := range strings.Split(wrapped, "\n") {
			b.WriteByte('\n')
			b.WriteString(bar + " " + textStyle.Render(truncateCells(line, inner)))
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
	// liveLast = active: while the turn is active its genuine LAST prose step renders only its
	// COMPLETED paragraphs — the still-growing final paragraph is WITHHELD (renderProse), so
	// neither the footer nor the flush ever shows a half-paragraph. A sealed turn (active=false)
	// renders every paragraph as full markdown.
	if body := renderTurnSteps(th, md, t, 0, -1, width, contentW, expanded, spinnerFrame, now, active); body != "" {
		parts = append(parts, body)
	}
	if ls := renderLiveStatus(th, t, spinnerFrame, now); ls != "" {
		parts = append(parts, ls)
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
// one branch tree, notes inline. liveLast governs whether the turn's genuine LAST prose
// step (global index len(Steps)-1) is treated as LIVE — committed paragraph by paragraph
// with its still-growing final paragraph WITHHELD entirely (renderProse). It applies ONLY
// to that last step and ONLY when liveLast is set, so an earlier prose step that streamed
// before a tool batch renders as FINAL markdown (its whole text), even though its sticky
// Streaming flag is still true. The flush and the footer both pass liveLast=true so they
// agree row-for-row on the withheld paragraph; the seal renders with liveLast=false so the
// withheld paragraph commits once as full markdown.
//
// Tool grouping is computed over the sub-range; the incremental flush only ever passes a
// range that begins and ends on a tool-group boundary (see finalizedStepCount), so a
// branch tree is never split across the flush frontier.
func renderTurnSteps(th theme.Theme, md *markdown.Renderer, t *TurnCell, from, to, width, contentW int, expanded bool, spinnerFrame int, now int64, liveLast bool) string {
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
			live := liveLast && g == lastIdx
			if rendered := renderProse(md, step, contentW, live); rendered != "" {
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

// renderProse renders one prose step. When NOT live (a sealed turn, or an earlier prose
// step that a later tool batch has since closed) it renders the whole step as settled
// markdown.
//
// When live (the genuinely-streaming LAST step of an active turn) it commits PARAGRAPH BY
// PARAGRAPH: only the text up to the last blank line ("\n\n") is settled, and that renders
// as markdown (it flushes to scrollback — flush.go). The still-growing final paragraph is
// WITHHELD entirely — it appears NOWHERE (not in the footer, not in scrollback) until it
// completes, at which point it becomes a settled paragraph and renders as full markdown. So
// prose surfaces one finished, fully-parsed markdown paragraph at a time rather than streaming
// token by token: no live token caret, no max-height dim preview block that fills then
// truncates, no raw→markdown reflow when a paragraph seals. The "⠋ Writing" live status
// (renderLiveStatus) is the only motion between committed paragraphs.
//
// `live` is decided by POSITION (is this the turn's last step), never by the step's sticky
// Streaming flag, so a half-rendered paragraph can never be frozen mid-turn into scrollback.
// "\n\n" is the only safe boundary because CommonMark joins single-newline lines into one
// reflowing paragraph; md.Render(settled-prefix) is a byte-exact prefix of the eventual full
// render, so the flush (which commits completed paragraphs) and the seal agree row-for-row.
func renderProse(md *markdown.Renderer, step TurnStep, contentW int, live bool) string {
	if step.Text == "" {
		return ""
	}
	if !live {
		return strings.TrimRight(md.Render(step.Text, contentW, false).ANSI, "\n")
	}
	// Live last step: render only the COMPLETED paragraphs (everything up to the final blank
	// line). The still-growing paragraph after it is withheld until it seals — both the footer
	// and the flush call this with the same `live`, so they agree on exactly what is held back.
	idx := strings.LastIndex(step.Text, "\n\n")
	if idx < 0 {
		return "" // no completed paragraph yet — the whole step is still growing, so withhold it
	}
	return strings.TrimRight(md.Render(step.Text[:idx], contentW, false).ANSI, "\n")
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
