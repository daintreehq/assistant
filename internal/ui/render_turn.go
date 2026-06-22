package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_turn.go renders a TurnCell (and its user message) to a styled string from
// its ordered Steps (_interaction-ux.md §5, ui-transcript.md §3-§5). The renderer
// takes width + expanded + now and is pure given (cell, theme, md, width, ...) so
// the scrollback queue can re-render frozen blocks fresh on resize.

// renderUserMessage renders the "YOU" card (UserMessageCard.tsx): a dim+bold "YOU"
// label on its OWN line, then the wrapped message body with one left accent bar
// (▏, U+258F) per row. A system-origin turn (UserText == "") renders nothing.
func renderUserMessage(th theme.Theme, text string, width int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	g := th.Glyphs
	// The bar carries the visual weight; the dim+bold "YOU" label is a quiet
	// who-said-what anchor above it (UserMessageCard.tsx). The bar color comes from
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
	// (UserMessageCard.tsx: inner = max(10, width-4)); one bar per wrapped row.
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
// while phase == received (ui-transcript.md §5).
func renderMarker(th theme.Theme, phase domain.RunPhase, active bool) string {
	g := th.Glyphs
	marker := th.Accent().Render(g.Brand + " DAINTREE")
	if active && phase == domain.PhaseReceived {
		marker += th.Dim().Render(" · received")
	}
	return marker
}

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
	// LiveRunStatus.tsx: the spinner (ThinkingDot) is a PLAIN <text> (terminal
	// default fg, no tone), and the label + elapsed are DIM (attribute-only faint),
	// NOT muted gray. Elapsed shows only once ≥300ms (elapsedToken).
	return th.Body().Render(spin) + th.Dim().Render(" "+label+elapsedToken(t.PhaseStartedAt, now))
}

// renderTurn renders the full turn cell: user message, marker, ordered steps
// (prose as markdown / activity tree / notes), then the live status. md is the
// theme-bound markdown renderer; width is the content width; now drives live
// elapsed; expanded reveals raw tool detail.
func renderTurn(th theme.Theme, md *markdown.Renderer, t *TurnCell, width, contentW int, expanded bool, spinnerFrame int, now int64) string {
	var b strings.Builder

	// The cell owns the single blank line ABOVE it (shared layout rule §3).
	if um := renderUserMessage(th, t.UserText, width); um != "" {
		b.WriteString(um)
		// UserMessageCard.tsx marginBottom={1}: a blank line separates the YOU card
		// from the ◆ DAINTREE marker so the exchange breathes.
		b.WriteString("\n\n")
	}

	active := t.State == TurnActive
	// The marker shows once the turn is active OR has said anything.
	if active || hasProse(t) {
		b.WriteString(renderMarker(th, t.Phase, active))
		b.WriteByte('\n')
	}

	// Ordered steps: prose as styled markdown, tools as branch rows, notes inline.
	toolGroup := collectToolGroups(t)
	gi := 0
	for _, step := range t.Steps {
		switch step.Kind {
		case StepProse:
			if rendered := renderProse(md, step, contentW); rendered != "" {
				b.WriteString(rendered)
				if !strings.HasSuffix(rendered, "\n") {
					b.WriteByte('\n')
				}
			}
		case StepTool:
			// A contiguous run of tool steps renders as one branch tree; only the
			// FIRST step of a group emits the whole group (so last-branch math works).
			if gi < len(toolGroup) && toolGroup[gi].first == step.Activity {
				b.WriteString(renderToolGroup(th, toolGroup[gi].acts, expanded, spinnerFrame, now, width))
				b.WriteByte('\n')
				gi++
			}
		case StepNote:
			if step.Note != nil {
				b.WriteString(renderInlineNote(th, *step.Note, width))
				b.WriteByte('\n')
			}
		}
	}

	if ls := renderLiveStatus(th, t, spinnerFrame, now); ls != "" {
		b.WriteString(ls)
		b.WriteByte('\n')
	}

	out := strings.TrimRight(b.String(), "\n")
	return out
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

// renderProse renders one prose step. A streaming step splits at the last newline:
// the stable block renders as styled markdown, the trailing in-progress line as raw
// text + a dim caret. A finalized step renders the whole thing as markdown.
func renderProse(md *markdown.Renderer, step TurnStep, contentW int) string {
	if step.Text == "" {
		return ""
	}
	if !step.Streaming {
		return strings.TrimRight(md.Render(step.Text, contentW, false).ANSI, "\n")
	}
	// Streaming: split at the last newline.
	idx := strings.LastIndexByte(step.Text, '\n')
	if idx < 0 {
		// Single in-progress line: raw text + caret (no markdown re-parse per token).
		// Wrap to the content width so the live line never overflows the gutter.
		return wrapCells(step.Text+" ▌", contentW)
	}
	stable := step.Text[:idx]
	pending := step.Text[idx+1:]
	var b strings.Builder
	b.WriteString(strings.TrimRight(md.Render(stable, contentW, false).ANSI, "\n"))
	if pending != "" {
		b.WriteByte('\n')
		b.WriteString(wrapCells(pending+" ▌", contentW))
	}
	return b.String()
}

// renderInlineNote renders a SystemNote attached to a turn.
func renderInlineNote(th theme.Theme, n SystemNote, width int) string {
	g := th.Glyphs
	glyph, tone := noteGlyph(th, n.Level)
	// TurnCellView.tsx note row: a single toned span carries the continuation spine
	// + the toned glyph + a separating space ("│ " already ends in a space), then the
	// note text renders at BODY color (a bare child — NOT dim). The spine tone is
	// green info / yellow warn / red error.
	spine := styleFor(th, tone, g.Continuation+glyph+" ")
	return truncateCells(spine+th.Body().Render(n.Text), width)
}

// noteGlyph maps a note level to (glyph, tone), mirroring TurnCellView.tsx:
// error → failed glyph + danger, warn → attention "!" + warning, else → bullet "·"
// + the "active" tone (cyan info — NOT accent green).
func noteGlyph(th theme.Theme, level NoteLevel) (string, string) {
	g := th.Glyphs
	switch level {
	case NoteError:
		return g.Failed, "danger"
	case NoteWarn:
		// theme.ts `attention` glyph is "!" in BOTH the unicode and ASCII sets.
		return "!", "warning"
	default:
		return g.Bullet, "info"
	}
}

// toolGroup is a contiguous run of StepTool activities rendered as one branch tree.
type toolGroup struct {
	first *Activity
	acts  []Activity
}

// collectToolGroups walks the steps and groups contiguous StepTool runs.
func collectToolGroups(t *TurnCell) []toolGroup {
	var groups []toolGroup
	var cur []Activity
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, toolGroup{first: findFirstActivity(t, cur[0].ID), acts: cur})
			cur = nil
		}
	}
	for _, s := range t.Steps {
		if s.Kind == StepTool && s.Activity != nil {
			cur = append(cur, *s.Activity)
		} else {
			flush()
		}
	}
	flush()
	return groups
}

// findFirstActivity returns the live Activity pointer for an id (so group matching
// uses the actual step pointer the renderer iterates over).
func findFirstActivity(t *TurnCell, id string) *Activity {
	for i := range t.Steps {
		if t.Steps[i].Kind == StepTool && t.Steps[i].Activity != nil && t.Steps[i].Activity.ID == id {
			return t.Steps[i].Activity
		}
	}
	return nil
}

// renderToolGroup renders a contiguous activity run as a branch tree.
func renderToolGroup(th theme.Theme, acts []Activity, expanded bool, spinnerFrame int, now int64, width int) string {
	var b strings.Builder
	for i, a := range acts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderActivityRow(th, a, i == len(acts)-1, expanded, spinnerFrame, now, width))
	}
	return b.String()
}
