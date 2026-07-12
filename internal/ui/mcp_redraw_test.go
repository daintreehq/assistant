package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// mcp_redraw_test.go locks the one-shot "baseline" repaint fired after the MCP
// connection first settles: Daintree's request for a guaranteed clean "starting point"
// once the link is up. The in-program boot lifecycle is vestigial (the splash plays
// BEFORE the program, so booting is always false and completeBoot never runs), so this
// is the only post-connect repaint and fires on EVERY launch. It is a NON-destructive
// soft repaint (softBaselineRedraw — no host wipe, no masthead re-commit), latched to
// fire at most once, deferred to a settled frame, and it NEVER discards the draft the
// user is typing. (The full nuclearRedraw is deliberately not used — it reintroduces
// the blank-footer / duplicate-masthead regressions; the e2e PTY contract proves it.)
//
// Harness note: harnessModel/bootToSteadyState open with booting=false (the production
// shape), and bootToSteadyState's own MCPConnectedMsg therefore arms the redraw once
// (nonce 1) and drains the masthead commit — the steady state a real launch reaches.

// The first MCP resolution arms the baseline redraw exactly once and latches it.
func TestMcpBaselineRedraw_ArmsOnceAfterConnect(t *testing.T) {
	m := harnessModel()
	next, _ := step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	mm := asModel(t, next)
	if !mm.mcpBaselineRedrawDone {
		t.Fatal("MCP resolving must latch mcpBaselineRedrawDone so it can never fire twice")
	}
	if mm.mcpRedrawPending != 1 {
		t.Fatalf("MCP resolving must arm the baseline redraw once, got pending=%d", mm.mcpRedrawPending)
	}
}

// On a settled steady-state frame the armed one-shot fires a NON-destructive soft
// repaint: it returns a command (the ClearScreen repaint) but must NOT wipe scrollback,
// re-commit the masthead, bump the redraw nonce, or disarm commits — and it must not be
// the defer path (no extra try, no re-arm). That distinction is what keeps the every-
// launch baseline from reintroducing the blank-footer / duplicate-masthead regressions
// (locked end-to-end by the e2e PTY startup contract).
func TestMcpBaselineRedraw_FiresOnQuietFrame(t *testing.T) {
	m := bootToSteadyState(t) // its MCPConnectedMsg armed the redraw (nonce 1)
	if !m.mcpBaselineRedrawDone || m.mcpRedrawPending != 1 {
		t.Fatalf("boot should have armed the baseline redraw once: done=%v pending=%d",
			m.mcpBaselineRedrawDone, m.mcpRedrawPending)
	}
	if !m.readyForBaselineRedraw() {
		t.Fatal("precondition: steady state should be a settled frame")
	}
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if cmd == nil {
		t.Fatal("firing the baseline repaint must return the repaint command")
	}
	// Non-destructive: none of the nuclear-redraw side effects may occur.
	if mm.redrawNonce != beforeNonce {
		t.Fatal("the soft baseline repaint must NOT bump redrawNonce (no masthead re-commit)")
	}
	if !mm.queue.headerDone {
		t.Fatal("the soft baseline repaint must NOT reset the commit queue")
	}
	if !mm.commitArmed {
		t.Fatal("the soft baseline repaint must NOT disarm commits")
	}
	// Fired, not deferred: the defer path would bump the nonce and count a try.
	if mm.mcpRedrawTries != 0 || mm.mcpRedrawPending != 1 {
		t.Fatalf("a settled-frame fire must not re-arm: tries=%d pending=%d", mm.mcpRedrawTries, mm.mcpRedrawPending)
	}
}

// A streaming turn owns the footer; the redraw must DEFER (re-arm) rather than wipe the
// transcript out from under it, then fire once the turn settles.
func TestMcpBaselineRedraw_DefersWhileTurnInFlight(t *testing.T) {
	m := bootToSteadyState(t)
	m.inFlight = true
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if mm.redrawNonce != beforeNonce {
		t.Fatal("a redraw must NOT fire while a turn is in flight")
	}
	if cmd == nil {
		t.Fatal("a deferred baseline redraw must re-arm itself")
	}
	if mm.mcpRedrawTries != 1 {
		t.Fatalf("a deferred redraw must count the try, got %d", mm.mcpRedrawTries)
	}
	if mm.mcpRedrawPending != 2 {
		t.Fatalf("re-arming must bump the nonce so the fresh tick is accepted, got %d", mm.mcpRedrawPending)
	}

	mm.inFlight = false
	pendingBeforeFire := mm.mcpRedrawPending
	next2, cmd2 := step(t, mm, mcpRedrawMsg{Nonce: mm.mcpRedrawPending})
	mm2 := asModel(t, next2)
	if cmd2 == nil {
		t.Fatal("the re-armed repaint must fire once the turn settles")
	}
	// Fired (soft repaint), not deferred again: the nonce must not have advanced.
	if mm2.mcpRedrawPending != pendingBeforeFire {
		t.Fatalf("settling should fire the repaint, not re-arm: pending %d→%d", pendingBeforeFire, mm2.mcpRedrawPending)
	}
}

// A scrollback commit draining (queue.inFlight) must defer the redraw: wiping the host
// mid-commit would race an already-released tea.Println that generations can't retract.
func TestMcpBaselineRedraw_DefersWhileCommitInFlight(t *testing.T) {
	m := bootToSteadyState(t)
	m.queue.inFlight = true
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if mm.redrawNonce != beforeNonce {
		t.Fatal("a redraw must NOT fire while a scrollback commit is draining")
	}
	if cmd == nil {
		t.Fatal("a commit-blocked baseline redraw must re-arm itself")
	}
}

// A pending resize redraw owns the next wipe; the baseline redraw must defer so the two
// don't issue independent destructive host clears in the same window.
func TestMcpBaselineRedraw_DefersWhileRedrawPending(t *testing.T) {
	m := bootToSteadyState(t)
	m.redrawPending = true
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if mm.redrawNonce != beforeNonce {
		t.Fatal("a redraw must NOT fire while a resize redraw is pending")
	}
	if cmd == nil {
		t.Fatal("a resize-blocked baseline redraw must re-arm itself")
	}
}

// booting is production-unreachable but kept in the readiness guard for robustness: a
// booting frame must still defer rather than wipe.
func TestMcpBaselineRedraw_DefersWhileBooting(t *testing.T) {
	m := bootToSteadyState(t)
	m.booting = true
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if mm.redrawNonce != beforeNonce {
		t.Fatal("a redraw must NOT fire while booting")
	}
	if cmd == nil {
		t.Fatal("a boot-blocked baseline redraw must re-arm itself")
	}
}

// The deferral is bounded: a cockpit that never goes quiet stops re-arming instead of
// polling forever. Giving up is safe — a later resize/Ctrl+L still recovers the frame.
func TestMcpBaselineRedraw_DeferralIsBounded(t *testing.T) {
	m := bootToSteadyState(t)
	m.inFlight = true
	m.mcpRedrawTries = mcpRedrawMaxTries
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if cmd != nil {
		t.Fatal("past the retry cap the redraw must give up, not re-arm")
	}
	if mm.redrawNonce != beforeNonce {
		t.Fatal("past the retry cap the redraw must not fire")
	}
}

// A degraded→connected pair (or any reconnect) must schedule the redraw at most once.
func TestMcpBaselineRedraw_FiresOnlyOnce(t *testing.T) {
	m := harnessModel()
	m, _ = step(t, m, MCPDegradedMsg{})
	if m.mcpRedrawPending != 1 {
		t.Fatalf("first resolve must arm the redraw, got pending=%d", m.mcpRedrawPending)
	}
	m, _ = step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	if m.mcpRedrawPending != 1 {
		t.Fatalf("a second resolve must not re-arm (latched), got pending=%d", m.mcpRedrawPending)
	}
}

// A stale-nonce tick (superseded by a re-arm) is ignored.
func TestMcpBaselineRedraw_StaleNonceIgnored(t *testing.T) {
	m := bootToSteadyState(t)
	beforeNonce := m.redrawNonce
	next, _ := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending - 1})
	mm := asModel(t, next)
	if mm.redrawNonce != beforeNonce {
		t.Fatal("a stale-nonce baseline redraw must be ignored")
	}
}

// The repaint tag (mcpRepaintViewMsg, the second half of softBaselineRedraw) changes
// View.Content's identity — the lever that forces the post-ClearScreen flush to emit —
// WITHOUT changing the rendered height or the visible cells.
func TestMcpBaselineRedraw_RepaintTagChangesContentNotHeight(t *testing.T) {
	m := bootToSteadyState(t)
	before := m.View().Content
	beforeRows := lineCount(before)

	next, _ := step(t, m, mcpRepaintViewMsg{})
	mm := asModel(t, next)
	if !mm.mcpRepaintView {
		t.Fatal("mcpRepaintViewMsg must set the durable repaint tag")
	}
	after := mm.View().Content
	if after == before {
		t.Fatal("the repaint tag must change View.Content identity (else the unchanged-frame flush is skipped)")
	}
	if lineCount(after) != beforeRows {
		t.Fatalf("the repaint tag must not change footer height: %d → %d", beforeRows, lineCount(after))
	}
	if stripAnsi(after) != stripAnsi(before) {
		t.Fatal("the repaint tag must be visually inert (no cell change)")
	}
}

// A quitting/empty footer must stay exactly "" even with the tag set — the cleared-View
// contract, so the tag never resurrects content into a teardown frame.
func TestMcpBaselineRedraw_RepaintTagSkipsEmptyFooter(t *testing.T) {
	m := bootToSteadyState(t)
	m, _ = step(t, m, mcpRepaintViewMsg{})
	m.quitting = true
	if got := m.View().Content; got != "" {
		t.Fatalf("empty footer must stay empty even with the repaint tag: %q", got)
	}
}

// The hard requirement: the baseline repaint NEVER touches the draft the user is
// typing (the composer is live-footer model state, untouched by a repaint).
func TestMcpBaselineRedraw_PreservesComposerDraft(t *testing.T) {
	m := bootToSteadyState(t)
	for _, r := range "draft in progress" {
		m, _ = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := m.composer.Value(); got != "draft in progress" {
		t.Fatalf("precondition: composer draft not captured, got %q", got)
	}
	next, _ := step(t, m, mcpRedrawMsg{Nonce: m.mcpRedrawPending})
	mm := asModel(t, next)
	if got := mm.composer.Value(); got != "draft in progress" {
		t.Fatalf("baseline redraw discarded the user's draft: got %q", got)
	}
}
