package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// flush.go implements the INCREMENTAL SCROLLBACK FLUSH of the active turn. The inline
// cockpit anchors its View() at the bottom; in Bubble Tea inline mode tea.Println prints
// above the live program, but if the live View is TALL when that happens Bubble Tea's
// insertAbove uses a stale/large cell-buffer height and dumps the whole footer into native
// scrollback (a frozen partial copy — charmbracelet/bubbletea#1613). The user sees the
// response "stop midway then re-show the function calls". The fix has two halves:
//
//  1. Keep completed content OUT of the footer: flush each IMMUTABLE block (a closed tool
//     group, a finished prose step, a completed markdown PARAGRAPH of the live prose step)
//     to scrollback the instant it can no longer change, so the footer never accumulates.
//  2. Keep the in-flight remainder SHORT: prose commits PARAGRAPH BY PARAGRAPH and the
//     still-growing final paragraph is WITHHELD from the footer entirely (render_turn.go),
//     so the live View is only [open tool group / live status / composer] — never tall.
//
// Two correctness rules make the flushed bytes match the seal's render exactly (no dup):
//   - Live-ness rides ONLY the position of the last step (render_turn.go); an earlier prose
//     step renders as final markdown, so the flush never freezes a half-rendered paragraph.
//   - A prose paragraph is committed only once COMPLETE (after a "\n\n"); CommonMark joins
//     single-newline lines into one reflowing paragraph, so a blank line is the only safe
//     commit boundary. Completed paragraphs render as settled markdown — byte-identical to
//     the prefix the seal will emit — and sealTail strips them exactly.
//
// LeftPad is applied at COMMIT (here) and at footer-assembly (view.go), never inside the
// row builders, so a flushed row in scrollback lines up column-for-column with the live
// tail: consistent left inset while streaming AND after seal.

// finalizedStepCount returns how many LEADING steps of the active turn are IMMUTABLE (safe
// to commit because they will never change again):
//
//   - A prose / note step is final once it is no longer the turn's last step.
//   - A contiguous tool run is final only when every activity in it is terminal AND it is
//     CLOSED (a non-tool step follows it) — so the whole branch tree commits atomically and
//     is never split across the flush frontier.
//   - The last step is never final while the turn is active (it is the live one — its
//     completed paragraphs flush separately, see activeTurnFinalRows).
//
// A sealed turn returns len(Steps). Scanning stops at the first non-final step.
func finalizedStepCount(t *TurnCell) int {
	n := len(t.Steps)
	if t.State != TurnActive {
		return n
	}
	if n == 0 {
		return 0
	}
	k := 0
	i := 0
	for i < n-1 { // never finalize the live last step
		s := t.Steps[i]
		if s.Kind == StepTool {
			// Scan the whole contiguous tool run [i:j).
			j := i
			allTerminal := true
			for j < n && t.Steps[j].Kind == StepTool {
				a := t.Steps[j].Activity
				if a == nil || (a.State != ActDone && a.State != ActFailed) {
					allTerminal = false
				}
				j++
			}
			// Flushable only if fully terminal AND closed by a following non-tool step
			// (j <= n-1 guarantees the run is not the live tail).
			if allTerminal && j <= n-1 {
				k = j
				i = j
				continue
			}
			break // an open / mutating run — nothing past it is final
		}
		k = i + 1 // prose / note that is not the last step
		i++
	}
	return k
}

// activeTurnFinalRows renders the IMMUTABLE prefix of the active turn — the canonical row
// form (leading blank separator + body, UN-indented) that the flush commits. It is a
// strict prefix of activeTurnRows: the head (YOU card + settled marker), then the finalized
// steps [0:finalizedStepCount), then — when the immutable prefix reaches the live last
// prose step — that step's COMPLETED paragraphs (its still-growing final paragraph and the
// live status are dropped). Every row it returns is byte-identical to what the seal will
// commit for that row, so a flushed row is never re-committed. Returns nil for an empty prefix.
func (m *Model) activeTurnFinalRows(t *TurnCell) []string {
	w := m.chromeW()
	cw := m.contentW()

	// The immutable step frontier. When every step before the last is finalized AND the last
	// step is the live prose, EXTEND the range over it too: renderTurnSteps with liveLast=true
	// renders only that step's COMPLETED paragraphs (its still-growing final paragraph is
	// withheld), so its settled prefix flushes while only the live status stays in the footer.
	// Rendering the whole prefix through one renderTurnSteps call (the same one the footer uses)
	// keeps the flush a byte-exact PREFIX of the footer render — including blank-after-tool spacing.
	k := finalizedStepCount(t)
	last := len(t.Steps) - 1
	if t.State == TurnActive && k == last && last >= 0 && t.Steps[last].Kind == StepProse {
		k = len(t.Steps)
	}

	var parts []string
	if pre := renderTurnPreamble(m.theme, t, w, false, true); pre != "" {
		parts = append(parts, pre)
	}
	if k > 0 {
		// liveLast=true: the flush commits ONLY completed paragraphs; the still-growing one is
		// withheld everywhere (render_turn.go renderProse) so scrollback never gets a
		// half-paragraph that would later be re-committed as markdown.
		if body := renderTurnSteps(m.theme, m.md, t, 0, k, w, cw, m.expanded, m.spinnerFrame, domain.NowMS(), true); body != "" {
			parts = append(parts, body)
		}
	}

	body := strings.Join(parts, "\n")
	if strings.TrimSpace(stripAnsi(body)) == "" {
		return nil
	}
	return strings.Split("\n"+body, "\n")
}

// activeTurnRows renders the FULL live TurnCell to the canonical row slice the footer tail
// and the seal index by FlushedRows. Row 0 is the leading blank separator; rows 1.. are the
// rendered body, UN-indented. The flush commits a PREFIX of these (activeTurnFinalRows).
// Returns nil for an empty turn.
func (m *Model) activeTurnRows(t *TurnCell) []string {
	w := m.chromeW()
	cw := m.contentW()
	body := renderTurn(m.theme, m.md, t, w, cw, m.expanded, m.spinnerFrame, domain.NowMS())
	if strings.TrimSpace(stripAnsi(body)) == "" {
		return nil
	}
	return strings.Split("\n"+body, "\n")
}

// flushActiveTurn commits the active turn's newly-IMMUTABLE rows (activeTurnFinalRows beyond
// FlushedRows) to native scrollback via tea.Println, indented to LeftPad, then advances
// FlushedRows. Completed blocks therefore flow into scrollback as they stream (auto-scroll),
// the footer never accumulates them, and nothing is duplicated at seal. Returns nil when
// there is nothing new to flush.
//
// Gated on commitArmed && headerDone so a row can never print ABOVE the masthead; runs from
// afterStateChange AFTER scheduleCommit so the masthead (queue) is ordered ahead of any
// prose Println.
func (m *Model) flushActiveTurn() tea.Cmd {
	if !m.commitArmed || !m.queue.headerDone {
		return nil
	}
	t := m.activeTurnCell()
	if t == nil {
		return nil
	}
	final := m.activeTurnFinalRows(t)
	target := len(final)
	if target <= t.FlushedRows {
		return nil
	}
	// REFLOW guard: the already-flushed rows must still render identically this frame, or
	// splicing final[FlushedRows:target] would emit MISALIGNED content beneath the prefix
	// already in scrollback. Settled-paragraph markdown is append-only, so this holds; a
	// rare markdown re-wrap makes us HOLD this frame and let the seal commit the tail.
	if t.FlushedRows > 0 {
		if t.FlushedRows > len(final) || strings.Join(final[:t.FlushedRows], "\n") != t.flushedRowsText {
			return nil
		}
	}
	start := t.FlushedRows
	if start == 0 && m.activeTurnIsFirstTranscriptCell(t.ID) {
		// Row 0 is the leading separator in the canonical turn rows. For transcript
		// cell 0, the masthead already reserved that spacer, so count it as flushed
		// without printing it again.
		start = 1
	}
	t.FlushedRows = target
	t.flushedRowsText = strings.Join(final[:target], "\n")
	if start >= target {
		return nil
	}
	chunk := indentLines(strings.Join(final[start:target], "\n"), LeftPad)
	return tea.Println(chunk)
}

// resetFlushState forgets every per-turn "already printed to host scrollback" cursor.
// Call this whenever host scrollback itself is wiped and the transcript will be
// re-committed from the model. Without it, a resize redraw can clear the flushed
// prefix from the terminal, then skip that same prefix during replay because the
// model still believes those rows are already present.
func (m *Model) resetFlushState() {
	for i := range m.transcript {
		if t := m.transcript[i].Turn; t != nil {
			t.FlushedRows = 0
			t.flushedRowsText = ""
		}
	}
}

func (m *Model) activeTurnIsFirstTranscriptCell(id string) bool {
	return len(m.transcript) > 0 && m.transcript[0].Turn != nil && m.transcript[0].Turn.ID == id
}

// sealTail returns the portion of the SEALED turn's canonical rows (un-indented) NOT yet in
// native scrollback, given the exact flushed text already committed. The turn re-renders on
// seal (prose finalizes to markdown), so we strip the flushed prefix at a row boundary —
// exact match in the clean case (the flushed prefix is a byte-exact prefix of the final
// render), else fall back to dropping the flushed ROW COUNT. Returns "" when nothing remains.
func sealTail(rows []string, flushed string) string {
	if len(rows) == 0 {
		return ""
	}
	flushedN := 0
	if flushed != "" {
		flushedN = strings.Count(flushed, "\n") + 1
	}
	if flushedN > 0 && flushedN <= len(rows) {
		if strings.Join(rows[:flushedN], "\n") == flushed {
			if flushedN >= len(rows) {
				return ""
			}
			return strings.Join(rows[flushedN:], "\n")
		}
	}
	if flushedN >= len(rows) {
		return ""
	}
	if flushedN < 0 {
		flushedN = 0
	}
	return strings.Join(rows[flushedN:], "\n")
}
