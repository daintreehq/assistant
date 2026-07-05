package ui

import (
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_question.go renders the multiple-choice question sheet (user.askMultipleChoice)
// — a card directly above the composer that REPLACES it while a decision is pending,
// mirroring the approval sheet. The highlighted option renders inverse; the user moves
// it with ↑/↓ (or Home/End) or answers directly by typing an option letter.

// maxQuestionOptionRows bounds how many option rows the sheet shows at once, so a
// 26-option question can't make the fixed bottom band taller than the terminal. With
// more options, a scroll window keyed to the selection shows a slice framed by ↑/↓ cues.
const maxQuestionOptionRows = 8

// maxQuestionRows caps the wrapped question height (the tool already bounds the text to
// 500 runes; this keeps a long one from dominating the sheet).
const maxQuestionRows = 3

// renderQuestion draws the sheet from the pending question state at content width.
func renderQuestion(th theme.Theme, q *pendingQuestion, width int) string {
	req := q.req
	var b strings.Builder

	// Title — Daintree's own voice (accent), led by the decision glyph.
	b.WriteString(th.Accent().Render(truncateCells(th.Glyphs.Approval+" Daintree needs a decision", width)))
	b.WriteByte('\n')

	// The question itself, wrapped and capped.
	b.WriteString(th.Body().Render(capWrap(req.Question, width, maxQuestionRows)))
	b.WriteByte('\n')

	// Options, windowed to keep the sheet bounded. The selected row is inverse; a "›"
	// caret marks it (so the choice is still legible with color stripped).
	start, end := optionWindow(len(req.Options), q.selected, maxQuestionOptionRows)
	if start > 0 {
		b.WriteString(th.Dim().Render(fmt.Sprintf("  ↑ %d more", start)))
		b.WriteByte('\n')
	}
	for i := start; i < end; i++ {
		opt := req.Options[i]
		row := opt.Label + ". " + opt.Text
		if i == q.selected {
			b.WriteString(th.Body().Reverse(true).Render(truncateCells("› "+row, width)))
		} else {
			b.WriteString(th.Body().Render(truncateCells("  "+row, width)))
		}
		b.WriteByte('\n')
	}
	if end < len(req.Options) {
		b.WriteString(th.Dim().Render(fmt.Sprintf("  ↓ %d more", len(req.Options)-end)))
		b.WriteByte('\n')
	}

	// Hint row — spells the answerable letter range so "A–<last>" is always accurate.
	last := req.Options[len(req.Options)-1].Label
	hint := "↑/↓ select · A–" + last + " answer · Enter confirm · Esc cancel"
	b.WriteString(th.Dim().Render(truncateCells(hint, width)))
	return b.String()
}

// optionWindow returns the [start,end) slice of n options to show (at most max rows),
// scrolled to keep `selected` visible and roughly centered. n<=max ⇒ the whole list.
func optionWindow(n, selected, max int) (int, int) {
	if n <= max {
		return 0, n
	}
	start := selected - max/2
	if start < 0 {
		start = 0
	}
	if start+max > n {
		start = n - max
	}
	return start, start + max
}

// capWrap wraps s to width and keeps at most maxRows rows, ellipsising the last one when
// the text overflows the budget.
func capWrap(s string, width, maxRows int) string {
	if maxRows < 1 {
		maxRows = 1
	}
	lines := strings.Split(wrapCells(s, width), "\n")
	if len(lines) > maxRows {
		lines = lines[:maxRows]
		lines[maxRows-1] = truncateCells(lines[maxRows-1]+" …", width)
	}
	return strings.Join(lines, "\n")
}
