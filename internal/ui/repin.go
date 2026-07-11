package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// repin.go keeps the live footer pinned to the terminal's BOTTOM row.
//
// Ultraviolet repaints a SHRUNKEN inline view top-anchored: the shorter footer is
// redrawn at the old frame's origin and the freed rows below are erased — but they
// stay in the host terminal's viewport as dead, scrollable blank area BELOW the
// composer. Growth and tea.Println recover it (insertAbove slides the view back down
// into dead rows), so a shrink that is matched by a print self-heals. The one that
// does NOT is the last shrink of a turn: the run-status chrome leaves the footer
// after the final block has already printed (often an EMPTY block — a fully
// line-flushed turn has nothing left to commit), stranding the composer 1-3 rows
// above the bottom with dead scroll area underneath after EVERY response.
//
// The fix is a small ledger reconciled on every state pass:
//   - a rendered footer SHRINK accrues debt — dead rows below the footer;
//   - every insertAbove we emit (queue commit prints, progressive turn flushes)
//     accrues CREDIT first, because its rows slide the footer over the matching
//     shrink physically — debt only accrues for the uncovered remainder;
//   - rendered GROWTH repays debt directly (the view grows down into dead rows) and
//     ends any credit story (the pending shrink it was covering has been netted out);
//   - small leftover debt is healed, after a render-settle barrier, by printing that
//     many blank rows above the footer — insertAbove then slides the footer back onto
//     the bottom row, and the blanks read as natural spacing between turns;
//   - large debts (an ops/help deck or approval sheet closing) are forgiven: dumping
//     a visible blank band into scrollback is worse than the pre-existing strand,
//     and the next real commit heals those organically.

// repinDebtCap bounds what the ledger will heal with blank rows. Turn-end chrome
// frees only a few rows; anything larger is forgiven (see the file comment).
const repinDebtCap = 4

// footerRepinMsg closes a re-pin barrier. Nonce guards against a stale tick: every
// scheduled (or re-armed) barrier bumps repinNonce, and only the newest may act.
type footerRepinMsg struct{ Nonce int }

// creditRepinRows records rows an insertAbove of ours is laying into the viewport;
// settleFooterDebt offsets the next rendered shrink against them before accruing
// debt, so a print-matched shrink never double-heals.
func (m *Model) creditRepinRows(rows int) {
	if rows > 0 {
		m.repinCredit += rows
	}
}

// resetRepinLedger drops all ledger state and invalidates any in-flight re-pin
// barrier. Call whenever host scrollback is wiped or re-laid wholesale (/clear, the
// nuclear resize redraw): stale debt must not survive into the fresh layout, and a
// validated tick must not print blank rows over it.
func (m *Model) resetRepinLedger() {
	m.footerDebt = 0
	m.repinCredit = 0
	m.footerPrevRows = 0
	m.repinPending = false
	m.repinNonce++
}

// settleFooterDebt reconciles the ledger against the last RENDERED footer height and
// resets it in states where the footer is re-laid wholesale (boot, quit, deck views,
// resize redraws — their transitions are not commit-adjacent shrinks). Reports
// whether the cockpit is in a state where healing may act at all.
func (m *Model) settleFooterDebt() bool {
	if m.footerRows == nil { // headless tests never render; the ledger stays inert
		return false
	}
	h := *m.footerRows
	if h > 0 && m.footerPrevRows > 0 {
		switch delta := m.footerPrevRows - h; {
		case delta > 0: // shrink — covered by pending print credit first
			if off := min(delta, m.repinCredit); off > 0 {
				m.repinCredit -= off
				delta -= off
			}
			m.footerDebt += delta
		case delta < 0: // growth — repays dead rows directly and ends the credit story
			m.footerDebt += delta
			if m.footerDebt < 0 {
				m.footerDebt = 0
			}
			m.repinCredit = 0
		}
	}
	if h > 0 {
		m.footerPrevRows = h
	}
	if m.booting || m.quitting || m.view != viewHome || !m.commitArmed || m.redrawPending {
		m.footerDebt = 0
		m.repinCredit = 0
		return false
	}
	if m.footerDebt > repinDebtCap {
		m.footerDebt = 0 // forgive big shrinks (sheet/deck close) — see the file comment
		return false
	}
	return true
}

// scheduleFooterRepin runs the ledger and, when unpaid debt remains and no commit is
// in flight (a commit's own print repays debt more precisely), arms ONE re-pin
// barrier. The barrier spans the renderer settle delay so the shrunken footer has
// physically flushed before the blank rows print above it.
func (m *Model) scheduleFooterRepin() tea.Cmd {
	if !m.settleFooterDebt() {
		return nil
	}
	if m.footerDebt <= 0 || m.queue.inFlight || m.repinPending {
		return nil
	}
	m.repinPending = true
	m.repinArmedRows = *m.footerRows
	return m.repinBarrier()
}

// repinBarrier arms the settle tick under a fresh nonce.
func (m *Model) repinBarrier() tea.Cmd {
	m.repinNonce++
	n := m.repinNonce
	return tea.Tick(rendererSettleDelay, func(time.Time) tea.Msg { return footerRepinMsg{Nonce: n} })
}

// onFooterRepin closes the barrier: re-run the ledger (the footer may have shrunk
// further, or grown and repaid, while the barrier waited), then heal the remaining
// debt with blank rows. A commit that went in flight meanwhile supersedes the heal —
// its ack re-enters scheduleFooterRepin with the residue. A footer whose height
// changed while the barrier waited gets a FRESH barrier instead of an immediate
// print: the new frame needs its own settle delay before an insertAbove can trust
// the renderer's cell-buffer height.
func (m Model) onFooterRepin(msg footerRepinMsg) (tea.Model, tea.Cmd) {
	if msg.Nonce != m.repinNonce {
		return m, nil
	}
	m.repinPending = false
	if !m.settleFooterDebt() || m.footerDebt <= 0 || m.queue.inFlight {
		return m, nil
	}
	if *m.footerRows != m.repinArmedRows {
		m.repinPending = true
		m.repinArmedRows = *m.footerRows
		return m, m.repinBarrier()
	}
	rows := m.footerDebt
	m.footerDebt = 0
	return m, tea.Println(repinText(rows))
}

// repinText renders rows blank display rows for the heal print. A lone "" would be
// dropped by Bubble Tea's insertAbove (empty-string early return), so the single-row
// case prints one space instead — visually blank, geometrically one row.
func repinText(rows int) string {
	if rows <= 1 {
		return " "
	}
	return strings.Repeat("\n", rows-1)
}
