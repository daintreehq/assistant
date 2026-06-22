package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// flush.go implements the INCREMENTAL SCROLLBACK FLUSH of the active turn. The
// inline cockpit anchors its View() at the bottom; in Bubble Tea inline mode a View
// taller than the terminal scrolls its own top into the host's native scrollback —
// freezing a PARTIAL copy of the still-streaming turn there (the caret "▌" and all),
// which the user sees as the response "stop midway then repeat from the top". The
// ONLY robust fix is to keep the live View ~CONSTANT-HEIGHT: commit each completed
// row of the active turn to scrollback via tea.Println the instant it goes FINAL,
// and render only the live tail in the footer.
//
// Mechanism: activeTurnRows renders the active turn in the SAME canonical form the
// seal commits (leading blank separator + LeftPad indent), so flush, the footer
// (liveCellsView) and the seal (sealedBlock) all index into ONE row sequence by
// TurnCell.FlushedRows. flushActiveTurn emits a tea.Println for each newly-final row
// and advances FlushedRows; liveCellsView skips the first FlushedRows rows; the seal
// renders only rows >= FlushedRows. Result: each completed line is tea.Println'd
// exactly once and the footer stays ~[live tail + status + composer] tall.

// activeTurnRows renders the live TurnCell to the canonical row slice that the flush,
// the footer tail, and the seal all index by FlushedRows. Row 0 is the leading blank
// separator (the cell owns the blank line above it, §3); rows 1.. are the rendered
// body, UN-indented. The LeftPad inset is applied at COMMIT time (flushActiveTurn /
// sealedBlock) — NOT here — so the footer can render the live tail in the same
// un-indented form as the rest of liveCellsView (indenting doesn't change the row
// COUNT, so FlushedRows stays valid for both forms). Returns nil when the turn renders
// to nothing.
func (m *Model) activeTurnRows(t *TurnCell) []string {
	w := m.chromeW()
	cw := m.contentW()
	body := renderTurn(m.theme, m.md, t, w, cw, m.expanded, m.spinnerFrame, domain.NowMS())
	if strings.TrimSpace(stripAnsi(body)) == "" {
		return nil
	}
	return strings.Split("\n"+body, "\n")
}

// turnHasActiveTools reports whether the turn has any tool step that is not yet in a
// terminal state. Tool rows MUTATE while active (spinner, progress line, settle), so
// the conservative flush rule refuses to flush at all while a tool is in flight — a
// flushed tool row could change after it was frozen. Those turns carry little prose,
// so the cost is acceptable (the footer just holds the small tool tree for a moment).
func turnHasActiveTools(t *TurnCell) bool {
	for _, s := range t.Steps {
		if s.Kind == StepTool && s.Activity != nil {
			switch s.Activity.State {
			case ActDone, ActFailed:
				// terminal — stable
			default:
				return true
			}
		}
	}
	return false
}

// flushableRows decides how many leading rows of the active turn are FINAL (safe to
// commit to scrollback because they will never change again), given the current
// rows. The SAFE, conservative rule:
//
//   - If the turn has any active (not-done) tool: flush NOTHING. Tool rows mutate.
//   - Else (pure prose / settled tools): everything EXCEPT the LAST row is final.
//     Appending tokens only extends/re-wraps the LAST visual line (greedy left-to-
//     right wrap keeps earlier wrapped lines stable), and renderProse renders the
//     trailing in-progress line raw with a "▌" caret — so the last row is the one
//     still in flux. Flushing all-but-last keeps the footer to ~[last prose line +
//     status + composer] during a long prose stream.
//
// While the turn is still TurnActive we never flush the very last row (it carries the
// live caret / live status); on seal the caller flushes the rest via the seal path.
func flushableRows(t *TurnCell, rows []string) int {
	if t.State != TurnActive {
		return len(rows) // sealed: every row is final (the seal path commits the tail)
	}
	if len(rows) == 0 {
		return 0
	}
	if turnHasActiveTools(t) {
		return 0 // tool rows still mutate — hold the whole turn in the footer
	}
	// All-but-last: the last row is the in-progress (re-wrapping, caret-bearing) line.
	return len(rows) - 1
}

// flushActiveTurn commits the active turn's newly-FINAL rows (those beyond
// FlushedRows up to flushableRows) to native scrollback via tea.Println, then
// advances FlushedRows. Returns nil when there is nothing new to flush.
//
// Ordering: this runs from afterStateChange, AFTER scheduleCommit. It must never
// print ABOVE the masthead, so it is gated on the masthead having committed
// (headerDone) — until then the active turn stays whole in the footer and the queue
// commits the masthead first. tea.Println prints above the live program in emit
// order, so once the masthead is down these flushes land beneath it, in row order.
func (m *Model) flushActiveTurn() tea.Cmd {
	if !m.commitArmed || !m.queue.headerDone {
		// The masthead has not committed yet — do not print prose above it.
		return nil
	}
	t := m.activeTurnCell()
	if t == nil {
		return nil
	}
	rows := m.activeTurnRows(t)
	target := flushableRows(t, rows)
	if target <= t.FlushedRows {
		return nil
	}
	// REFLOW guard: the already-flushed rows must still render identically in this
	// fresh frame, or splicing rows[FlushedRows:target] would emit MISALIGNED content
	// beneath the prefix already in scrollback. Markdown (glamour) reflows a multi-line
	// stable block as it grows (e.g. a numbered list joins into one re-wrapping
	// paragraph); when that happens we HOLD this frame (the seal later commits the whole
	// remaining tail consistently). Greedy wrapCells prose — the common case — never
	// diverges, so it always advances.
	if t.FlushedRows > 0 {
		prefix := strings.Join(rows[:t.FlushedRows], "\n")
		if prefix != t.flushedRowsText {
			return nil
		}
	}
	// The newly-final rows are [FlushedRows, target). Indent to the committed LeftPad
	// form and commit them as ONE Println block (contiguous, cheaper than per-row).
	chunk := indentLines(strings.Join(rows[t.FlushedRows:target], "\n"), LeftPad)
	t.FlushedRows = target
	t.flushedRowsText = strings.Join(rows[:target], "\n")
	return tea.Println(chunk)
}

// sealTail returns the portion of the SEALED turn's canonical rows (un-indented) that
// is NOT yet in native scrollback, given the exact flushed text already committed.
// The turn re-renders on seal (prose finalizes to markdown), so its rows can differ
// from the streaming-time rows we flushed. We strip the flushed prefix at a row
// boundary: if the final rows lead with exactly the flushed text, return the clean
// remainder; otherwise (a reflow changed the prefix wrap) fall back to dropping the
// flushed ROW COUNT — content-equivalent, best-effort alignment. Returns "" when
// nothing remains.
func sealTail(rows []string, flushed string) string {
	if len(rows) == 0 {
		return ""
	}
	flushedN := 0
	if flushed != "" {
		flushedN = strings.Count(flushed, "\n") + 1
	}
	// Exact, row-aligned prefix match — the common, clean case.
	if flushedN > 0 && flushedN <= len(rows) {
		if strings.Join(rows[:flushedN], "\n") == flushed {
			if flushedN >= len(rows) {
				return ""
			}
			return strings.Join(rows[flushedN:], "\n")
		}
	}
	// Reflow fallback: drop the flushed row count (clamped).
	if flushedN >= len(rows) {
		return ""
	}
	if flushedN < 0 {
		flushedN = 0
	}
	return strings.Join(rows[flushedN:], "\n")
}
