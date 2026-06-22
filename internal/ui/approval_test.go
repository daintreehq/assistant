package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// approval_test.go locks the hardened approval sheet: debounced
// affirmative, Enter→decline default, bell on unhandled keys, the A session allow-list,
// and typed-confirmation for the highest-risk actions.

func runeKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

func approvalPending(t *testing.T, req tools.ConfirmRequest) (Model, chan bool) {
	t.Helper()
	m := harnessModel()
	ch := make(chan bool, 1)
	next, _ := m.Update(ApprovalRequestedMsg{Request: req, Resolve: ch})
	return asModel(t, next), ch
}

// approvalDecision non-blockingly reads the resolve channel: (value, decided).
func approvalDecision(ch chan bool) (bool, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		return false, false
	}
}

func TestApproval_DebouncesAffirmative(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	// Immediately (inside the debounce window) 'y' must NOT approve — it rings the bell.
	next, cmd := m.onApprovalKey(runeKey('y'))
	mm := asModel(t, next)
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("a typed-ahead affirmative inside the debounce window approved the action")
	}
	if mm.pending == nil {
		t.Fatal("the sheet was dismissed during the debounce window")
	}
	if cmd == nil {
		t.Fatal("a debounced affirmative should ring the bell")
	}
	// Past the window, 'y' approves.
	mm.pending.shownAt = domain.NowMS() - approveDebounceMs - 50
	asModel(t, mustModel(mm.onApprovalKey(runeKey('y'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("post-debounce 'y' did not approve (ok=%v v=%v)", ok, v)
	}
}

func TestApproval_EnterDeclinesByDefault(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	mm := asModel(t, mustModel(m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEnter})))
	if v, ok := approvalDecision(ch); !ok || v {
		t.Fatalf("Enter should trigger the DECLINE default (ok=%v v=%v)", ok, v)
	}
	if mm.pending != nil {
		t.Fatal("the sheet was not dismissed after Enter-declines")
	}
}

func TestApproval_UnhandledKeyRingsBell(t *testing.T) {
	m, _ := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000 // past debounce, so any bell is from the unhandled key
	next, cmd := m.onApprovalKey(runeKey('z'))
	mm := asModel(t, next)
	if cmd == nil {
		t.Fatal("an out-of-vocabulary approval key should ring the bell, not be swallowed")
	}
	if mm.pending == nil {
		t.Fatal("an unhandled key dismissed the sheet")
	}
}

func TestApproval_AllowToolRemembersForSession(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000
	mm := asModel(t, mustModel(m.onApprovalKey(runeKey('a'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("A did not approve (ok=%v v=%v)", ok, v)
	}
	if !mm.approvedTools["terminal.sendInput"] {
		t.Fatal("A did not add the tool to the session allow-list")
	}
	// A subsequent request for the SAME tool auto-approves without a sheet.
	ch2 := make(chan bool, 1)
	mm2 := asModel(t, mustModel(mm.Update(ApprovalRequestedMsg{
		Request: confirmReq("terminal.sendInput", domain.RiskTerminal, ""), Resolve: ch2,
	})))
	if mm2.pending != nil {
		t.Fatal("a remembered tool still raised an approval sheet")
	}
	if v, ok := approvalDecision(ch2); !ok || !v {
		t.Fatal("a remembered tool was not auto-approved")
	}
}

func TestApproval_GitEntersTypedConfirm(t *testing.T) {
	// The only RiskGit tools (git.snapshotRevert / git.snapshotDelete) discard or delete
	// uncommitted work — they must require a TYPED confirmation, never single-key approve,
	// and never be remembered. (Regression guard: an earlier `strings.Contains(tool,"push")`
	// gate never matched these and let them through with a single keypress.)
	m, ch := approvalPending(t, confirmReq("git.snapshotDelete", domain.RiskGit, ""))
	if m.pending == nil || !m.pending.requireType {
		t.Fatal("an irreversible git action did not enter typed-confirmation mode")
	}
	// 'a' is now just a typed character (builds the phrase), not approve-and-remember.
	m2 := asModel(t, mustModel(m.onApprovalKey(runeKey('a'))))
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("'a' approved an irreversible git action in typed-confirm mode")
	}
	if len(m2.approvedTools) != 0 {
		t.Fatal("a git tool was added to the session allow-list (must never be remembered)")
	}
}

func TestApproval_TypedConfirmForSystem(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("daintree.call", domain.RiskSystem, ""))
	if m.pending == nil || !m.pending.requireType {
		t.Fatal("a system-risk action did not enter typed-confirmation mode")
	}
	// A bare 'y' is just a typed character now, NOT an approval.
	m = asModel(t, mustModel(m.onApprovalKey(runeKey('y'))))
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("'y' approved in typed-confirmation mode")
	}
	// Type the phrase, then Enter approves.
	m.pending.confirmInput = ""
	for _, r := range confirmPhrase {
		m = asModel(t, mustModel(m.onApprovalKey(runeKey(r))))
	}
	asModel(t, mustModel(m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEnter})))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("typed phrase + Enter did not approve (ok=%v v=%v)", ok, v)
	}
}

func TestApproval_TypedConfirmWrongPhraseBells(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("daintree.call", domain.RiskSystem, ""))
	m.pending.confirmInput = "nope"
	next, cmd := m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := asModel(t, next)
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("a wrong phrase approved on Enter")
	}
	if cmd == nil {
		t.Fatal("a wrong-phrase Enter should ring the bell")
	}
	if mm.pending == nil {
		t.Fatal("a wrong-phrase Enter dismissed the sheet")
	}
}

func TestApproval_TypedConfirmEscDeclines(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("daintree.call", domain.RiskSystem, ""))
	asModel(t, mustModel(m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEsc})))
	if v, ok := approvalDecision(ch); !ok || v {
		t.Fatalf("Esc should decline in typed-confirmation mode (ok=%v v=%v)", ok, v)
	}
}
