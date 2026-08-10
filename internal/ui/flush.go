package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/domain"
)

// flush.go implements the INCREMENTAL SCROLLBACK FLUSH of the active turn. The inline
// cockpit anchors its View() at the bottom; in Bubble Tea inline mode tea.Println prints
// above the live program, but if the live View is TALL when that happens Bubble Tea's
// insertAbove uses a stale/large cell-buffer height and dumps the whole footer into native
// scrollback (a frozen partial copy — charmbracelet/bubbletea#1613). The user sees the
// response "stop midway then re-show the function calls". The fix has two halves:
//
//  1. Keep completed content OUT of the footer: flush each IMMUTABLE block (a closed tool
//     group, a finished prose step, a settled wrapped LINE of the live prose step) to
//     scrollback the instant it can no longer change, so the footer never accumulates.
//  2. Keep the in-flight remainder SHORT: a plain growing paragraph COMMITS line by line —
//     all but its still-mutable last visual row flush as they settle (render_turn.go
//     renderProse), so prose flows into scrollback token by token and the footer holds only the
//     partial last line + the live status. (A markdown-risky tail falls back to paragraph-level
//     commit and rides the view.go lastLines(budget) cap, which is also the height backstop so
//     the live View is never tall.)
//
// Two correctness rules make the flushed bytes match the seal's render exactly (no dup):
//   - Live-ness rides ONLY the position of the last step (render_turn.go); an earlier prose
//     step renders as final markdown, so the flush never freezes a half-rendered paragraph.
//   - A row of the live last step is committed only once IMMUTABLE: for a PLAIN growing
//     paragraph that is every wrapped row but the still-mutable last one (greedy word-wrap
//     closes earlier rows); for a MARKDOWN-risky one it is only the completed "\n\n"-terminated
//     paragraphs (render_turn.go renderProse / proseTailCommittable). Either way the committed rows
//     render byte-identically to the prefix the seal will emit, and sealTail strips them exactly.
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
				// settledForFlush is the single source of truth for "this row can
				// never change again" (defined next to the ActivityState consts so a
				// new state must decide its flush semantics there).
				if a == nil || !a.State.settledForFlush() {
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
// prose step — that step's SETTLED rows (a plain growing paragraph contributes all but its
// mutable last visual row; the live status is always dropped). Every row it returns is
// byte-identical to what the seal will commit for that row, so a flushed row is never
// re-committed. Returns nil for an empty prefix.
func (m *Model) activeTurnFinalRows(t *TurnCell) []string {
	w := m.chromeW()
	cw := m.contentW()

	// The immutable step frontier. When every step before the last is finalized AND the last
	// step is the live prose, EXTEND the range over it too: renderTurnSteps with
	// withholdGrowingLast=true renders that step's SETTLED rows (renderProse drops only the
	// growing paragraph's still-mutable tail — its last visual row for a plain tail, the whole
	// paragraph for a markdown one), so the settled prefix flushes while only the mutable tail
	// stays live in the footer. This render is a byte-exact PREFIX of the footer's full render
	// (renderProse trims the SAME md.Render output) — including blank-after-tool spacing.
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
		// withholdGrowingLast=true: the flush commits the growing paragraph's SETTLED rows only
		// (render_turn.go renderProse drops its mutable tail), so scrollback never gets a row that
		// later reflows. The mutable tail still renders live in the footer — see liveCellsView.
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
	// HOLD until the commit queue's frontier has reached this turn: EVERY transcript cell
	// ahead of the active turn must already be in scrollback. The queue commits sealed cells
	// ONE per ack (async), while this flush fires on every afterStateChange — so a still-
	// uncommitted note sitting between the committed cursor and the turn would be LEAPFROGGED:
	// the turn's preamble (YOU card + ◆ DAINTREE marker) prints first, then the queue commits
	// the note BELOW the marker. That is exactly how the one-time "Connected to Daintree MCP"
	// boot banner (added under the still-empty transcript, then immediately followed by the
	// user's first turn) ended up mid-turn under ◆ DAINTREE instead of under the masthead.
	// Holding while committed < activeTurnIndex keeps the active turn the HEAD of the live
	// region, so its rows can never print ahead of a pending earlier cell. (committed can't run
	// PAST idx here: nextCommit stops at the first non-sealed cell, i.e. this active turn.) No
	// deadlock: each commit ack advances the cursor and re-runs afterStateChange, so the queue
	// drains the earlier notes first, then this flush proceeds.
	if idx := m.activeTurnIndex(); idx < 0 || m.queue.committed < idx {
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
	// Credit the ledger (repin.go): these rows leave the live tail on the very next
	// render, and this print slides the footer over that shrink physically — without
	// the credit, a stream pause right after a settle would heal the same rows AGAIN
	// with blank lines mid-turn.
	m.creditRepinRows(lineCount(chunk))
	// Commit in viewport-sized slices: a SINGLE tea.Println taller than the screen
	// corrupts the inline render (see scrollbackChunkRows + splitRowChunks). A large
	// PASTE makes the YOU card hundreds of rows, so this is the one place an immutable
	// block can be arbitrarily tall in one shot.
	return tea.Sequence(chunkPrintlns(chunk, m.scrollbackChunkRows())...)
}

// scrollbackChunkRows is the maximum display rows a single tea.Println may carry to native
// scrollback at the current terminal height — the rows ABOVE the live footer. Bubble Tea
// v2's insertAbove (cursed_renderer.go) scrolls the screen by the printed line count, then
// moves the cursor UP by that count PLUS the live-footer height; when the printed block is
// taller than the rows above the footer, that CursorUp CLAMPS at the top of the viewport and
// the geometry desyncs — freezing a copy of the footer into scrollback with a gap of blank
// rows (the charmbracelet/bubbletea#1613 failure class, triggered when a large paste makes
// the YOU card hundreds of rows). Reserving the footer height — the live tail (capped at
// maxLiveRows) plus the real bottom band — keeps every insertAbove within one screen. Floored
// at 1 so a tiny terminal still makes progress (just many small prints).
//
// Residual (both far outside normal use, neither is the reported bug): if the live footer
// fills the ENTIRE viewport — only when the terminal is exactly bottomBand+2 rows tall (~8
// rows), so view.go's budget floors the tail to 1 and h == m.rows — NO insert fits and even a
// 1-row print can clip by a row (a Bubble Tea insertAbove limitation, not closeable by chunk
// size). And a commit fired in the single frame of an ops→home view toggle can momentarily see
// the prior, taller footer; the maxLiveRows cap absorbs the common lag but not a shrinking
// bottom band.
func (m Model) scrollbackChunkRows() int {
	var reserve int
	if m.footerRows != nil && *m.footerRows > 0 {
		// The footer's ACTUAL last-rendered height (View writes it). It lags m's state by one
		// frame — exactly the cell-buffer height insertAbove uses for a commit fired in the next
		// Update — so reserving it (plus a 1-row safety margin) keeps every chunk's CursorUp at or
		// below rows-1 even when the footer grew tall to hold a withheld block. This is what lets
		// the live footer fill the screen (view.go) without #1613: prose keeps the footer short →
		// large chunks; a tall withheld block → small chunks, but only while one is on screen.
		reserve = *m.footerRows + 1
	} else {
		// Fallback before the first render (or in headless tests, which never call View): the
		// former static bound. Ops/help decks replace the bottom band with a near-full-height
		// scroll window, so a commit then must print one row at a time.
		switch m.view {
		case viewOperations, viewHelp:
			reserve = m.rows - 1
		default:
			reserve = maxLiveRows + 1 + lineCount(m.bottomBand(m.contentW()))
		}
	}
	if r := m.rows - reserve; r >= 1 {
		return r
	}
	return 1
}

// chunkPrintlns renders text as a SEQUENCE of tea.Println commands, each at most maxRows
// display rows, so no single insertAbove exceeds the viewport (see scrollbackChunkRows).
// One "\n"-line is one display row — committed rows are pre-wrapped to the content width and
// LeftPad-indented to within the terminal width — so splitting on newlines bounds the
// on-screen height exactly. (The codebase-wide invariant that a committed line fits the width
// can only break on a sub-~12-column terminal, where renderUserMessage's inner>=10 floor
// outruns the content width; at that size the cockpit is already unusable.)
func chunkPrintlns(text string, maxRows int) []tea.Cmd {
	chunks := splitRowChunks(text, maxRows)
	cmds := make([]tea.Cmd, len(chunks))
	for i, c := range chunks {
		cmds[i] = tea.Println(c)
	}
	return cmds
}

// splitRowChunks splits text into substrings of at most maxRows "\n"-delimited lines,
// preserving order and content: joining the result with "\n" reproduces text exactly. A pure
// helper so the row-budget math is unit-testable without the tea runtime. Returns a single
// chunk (the whole text) when it already fits, and never returns an empty slice.
func splitRowChunks(text string, maxRows int) []string {
	if maxRows < 1 {
		maxRows = 1
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= maxRows {
		return []string{text}
	}
	chunks := make([]string, 0, (len(lines)+maxRows-1)/maxRows)
	for i := 0; i < len(lines); i += maxRows {
		end := i + maxRows
		if end > len(lines) {
			end = len(lines)
		}
		chunks = append(chunks, strings.Join(lines[i:end], "\n"))
	}
	return chunks
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
