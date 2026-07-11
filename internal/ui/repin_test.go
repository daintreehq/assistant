package ui

import (
	"testing"
)

// repin_test.go covers the footer re-pin ledger (repin.go) headlessly: accrual on
// rendered shrink, repayment by print credit and growth, forgiveness of large
// shrinks, barrier discipline, and the heal payload. The end-to-end geometry (the
// composer actually landing on the terminal's bottom row) is locked by the PTY
// harness (internal/e2e TestPTYTurnEndFooterAtViewportBottom).

// repinModel is a steady-state home-view model with a rendered footer height the
// ledger can reconcile against.
func repinModel(t *testing.T, rendered int) Model {
	t.Helper()
	m := testModel(100)
	m.footerRows = new(int)
	*m.footerRows = rendered
	m.commitArmed = true
	m.queue.headerDone = true
	if !m.settleFooterDebt() {
		t.Fatal("steady-state model must be eligible for re-pin")
	}
	if m.footerDebt != 0 {
		t.Fatalf("initial settle accrued debt %d, want 0", m.footerDebt)
	}
	return m
}

func TestFooterRepin_ShrinkSchedulesOneBarrierAndHeals(t *testing.T) {
	m := repinModel(t, 10)

	*m.footerRows = 8 // the run-status chrome left the footer (rendered shrink of 2)
	cmd := m.scheduleFooterRepin()
	if cmd == nil {
		t.Fatal("a small rendered shrink must arm the re-pin barrier")
	}
	if m.footerDebt != 2 || !m.repinPending {
		t.Fatalf("debt=%d pending=%v, want 2/true", m.footerDebt, m.repinPending)
	}
	// The barrier is single-flight: another pass must not arm a second one.
	if extra := m.scheduleFooterRepin(); extra != nil {
		t.Fatal("a pending barrier must suppress a second schedule")
	}

	next, heal := m.Update(footerRepinMsg{Nonce: m.repinNonce})
	nm := next.(Model)
	if heal == nil {
		t.Fatal("the barrier must heal outstanding debt with a blank print")
	}
	if nm.footerDebt != 0 || nm.repinPending {
		t.Fatalf("after heal: debt=%d pending=%v, want 0/false", nm.footerDebt, nm.repinPending)
	}
	// A stale nonce (superseded barrier) must be inert.
	if _, cmd := nm.Update(footerRepinMsg{Nonce: nm.repinNonce - 1}); cmd != nil {
		t.Fatal("a stale re-pin barrier must not print")
	}
}

func TestFooterRepin_HeightChangeDuringBarrierReArms(t *testing.T) {
	m := repinModel(t, 10)
	*m.footerRows = 8
	if cmd := m.scheduleFooterRepin(); cmd == nil {
		t.Fatal("shrink must arm the barrier")
	}
	// The footer shrinks AGAIN while the barrier waits: the new frame needs its own
	// settle delay, so the closing tick must re-arm rather than print immediately.
	*m.footerRows = 7
	nonce := m.repinNonce
	next, cmd := m.Update(footerRepinMsg{Nonce: nonce})
	nm := next.(Model)
	if cmd == nil {
		t.Fatal("the re-arm must schedule a fresh barrier")
	}
	if !nm.repinPending || nm.repinNonce == nonce {
		t.Fatalf("pending=%v nonce=%d, want a re-armed barrier under a new nonce", nm.repinPending, nm.repinNonce)
	}
	if nm.footerDebt != 3 {
		t.Fatalf("debt=%d, want 3 (both shrinks) carried into the fresh barrier", nm.footerDebt)
	}
	// The stable frame closes the fresh barrier and heals everything.
	next, heal := nm.Update(footerRepinMsg{Nonce: nm.repinNonce})
	if heal == nil {
		t.Fatal("the fresh barrier must heal once the footer is stable")
	}
	if fm := next.(Model); fm.footerDebt != 0 {
		t.Fatalf("debt=%d after heal, want 0", fm.footerDebt)
	}
}

func TestFooterRepin_GrowthRepaysDebt(t *testing.T) {
	m := repinModel(t, 10)
	*m.footerRows = 8
	if cmd := m.scheduleFooterRepin(); cmd == nil {
		t.Fatal("shrink must arm the barrier")
	}
	// The footer grows back (a new turn's chrome) before the barrier closes: the
	// growth physically reclaims the dead rows, so the heal must print nothing.
	*m.footerRows = 10
	next, heal := m.Update(footerRepinMsg{Nonce: m.repinNonce})
	if heal != nil {
		t.Fatal("growth repaid the debt — the barrier must not print blank rows")
	}
	if nm := next.(Model); nm.footerDebt != 0 {
		t.Fatalf("debt=%d after growth, want 0", nm.footerDebt)
	}
}

func TestFooterRepin_PrintCreditCoversShrink(t *testing.T) {
	m := repinModel(t, 10)
	// A commit print (or progressive flush) laid 3 rows into the viewport; the footer
	// then rendered 3 rows shorter (the printed rows left the live tail). Net debt:
	// zero — the insertAbove already slid the footer over those rows.
	m.creditRepinRows(3)
	*m.footerRows = 7
	if cmd := m.scheduleFooterRepin(); cmd != nil {
		t.Fatal("a shrink fully covered by print credit must not arm the barrier")
	}
	if m.footerDebt != 0 || m.repinCredit != 0 {
		t.Fatalf("debt=%d credit=%d, want 0/0", m.footerDebt, m.repinCredit)
	}
}

func TestFooterRepin_CreditSurvivesQuietPassesUntilShrink(t *testing.T) {
	m := repinModel(t, 10)
	m.creditRepinRows(2)
	// Passes with an unchanged footer must not leak the credit away — the matching
	// shrink renders one frame after the print was emitted.
	if cmd := m.scheduleFooterRepin(); cmd != nil || m.repinCredit != 2 {
		t.Fatalf("cmd=%v credit=%d, want nil/2 (credit held for the coming shrink)", cmd, m.repinCredit)
	}
	*m.footerRows = 8
	if cmd := m.scheduleFooterRepin(); cmd != nil || m.footerDebt != 0 {
		t.Fatalf("cmd=%v debt=%d, want nil/0 — the held credit covers the shrink", cmd, m.footerDebt)
	}
}

func TestFooterRepin_LargeShrinkIsForgiven(t *testing.T) {
	m := repinModel(t, 38)
	*m.footerRows = 8 // an ops/help deck or approval sheet closed: 30 freed rows
	if cmd := m.scheduleFooterRepin(); cmd != nil {
		t.Fatal("a shrink beyond repinDebtCap must be forgiven, not healed with a blank band")
	}
	if m.footerDebt != 0 {
		t.Fatalf("debt=%d after forgiveness, want 0", m.footerDebt)
	}
}

func TestFooterRepin_InFlightCommitDefersHealing(t *testing.T) {
	m := repinModel(t, 10)
	m.queue.inFlight = true
	*m.footerRows = 8
	if cmd := m.scheduleFooterRepin(); cmd != nil {
		t.Fatal("an in-flight commit must defer healing to its own print")
	}
	if m.footerDebt != 2 {
		t.Fatalf("debt=%d, want 2 carried until the commit acks", m.footerDebt)
	}
}

func TestFooterRepin_ResetInvalidatesPendingBarrier(t *testing.T) {
	m := repinModel(t, 10)
	*m.footerRows = 8
	if cmd := m.scheduleFooterRepin(); cmd == nil {
		t.Fatal("shrink must arm the barrier")
	}
	nonce := m.repinNonce
	m.resetRepinLedger() // /clear or a nuclear resize redraw landed
	if m.footerDebt != 0 || m.repinCredit != 0 || m.repinPending {
		t.Fatalf("ledger not cleared: debt=%d credit=%d pending=%v", m.footerDebt, m.repinCredit, m.repinPending)
	}
	if _, cmd := m.Update(footerRepinMsg{Nonce: nonce}); cmd != nil {
		t.Fatal("a tick armed before the reset must be inert after it")
	}
}

func TestRepinText(t *testing.T) {
	// One display row must be non-empty (Bubble Tea drops an empty Println); N rows
	// are N-1 newlines (lineCount semantics: "\n" == 2 rows).
	if got := repinText(1); got != " " {
		t.Fatalf("repinText(1) = %q, want a single visually-blank row", got)
	}
	for n := 2; n <= repinDebtCap; n++ {
		if got := lineCount(repinText(n)); got != n {
			t.Fatalf("repinText(%d) renders %d rows, want %d", n, got, n)
		}
	}
}
