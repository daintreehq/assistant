package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
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
	if mm.approvedTools["terminal.sendInput"] != approveDefaultCount {
		t.Fatalf("A did not seed the bounded session allow-list (got %d, want %d)",
			mm.approvedTools["terminal.sendInput"], approveDefaultCount)
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

// autoApprove fires one ApprovalRequestedMsg for tool/risk and returns the resulting model
// plus whether it auto-resolved true without surfacing a sheet.
func autoApprove(t *testing.T, m Model, tool string, risk domain.RiskClass) (Model, bool) {
	t.Helper()
	ch := make(chan bool, 1)
	mm := asModel(t, mustModel(m.Update(ApprovalRequestedMsg{
		Request: confirmReq(tool, risk, ""), Resolve: ch,
	})))
	v, ok := approvalDecision(ch)
	return mm, mm.pending == nil && ok && v
}

func TestApproval_BoundedCountDecrementsAndRePrompts(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000 // past debounce
	mm := asModel(t, mustModel(m.onApprovalKey(runeKey('a'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("A did not approve (ok=%v v=%v)", ok, v)
	}
	if got := mm.approvedTools["terminal.sendInput"]; got != approveDefaultCount {
		t.Fatalf("A seeded count %d, want %d", got, approveDefaultCount)
	}
	// Each of the granted calls auto-approves without a sheet and decrements the count,
	// dropping the entry once the grant is spent.
	for i := 0; i < approveDefaultCount; i++ {
		var ok bool
		mm, ok = autoApprove(t, mm, "terminal.sendInput", domain.RiskTerminal)
		if !ok {
			t.Fatalf("bounded auto-approve %d did not resolve true without a sheet", i)
		}
	}
	if _, present := mm.approvedTools["terminal.sendInput"]; present {
		t.Fatal("a spent bounded grant was not dropped from the allow-list")
	}
	// The grant is spent — the next request must surface the sheet again (not auto-resolve).
	ch3 := make(chan bool, 1)
	mm = asModel(t, mustModel(mm.Update(ApprovalRequestedMsg{
		Request: confirmReq("terminal.sendInput", domain.RiskTerminal, ""), Resolve: ch3,
	})))
	if mm.pending == nil {
		t.Fatal("a spent bounded grant did not re-surface the approval sheet")
	}
	if _, ok := approvalDecision(ch3); ok {
		t.Fatal("a spent bounded grant auto-resolved instead of re-prompting")
	}
}

func TestApproval_AllowForeverNeverDecrements(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000
	mm := asModel(t, mustModel(m.onApprovalKey(runeKey('f'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("F did not approve (ok=%v v=%v)", ok, v)
	}
	if got := mm.approvedTools["terminal.sendInput"]; got != allowForeverCount {
		t.Fatalf("F seeded count %d, want forever sentinel %d", got, allowForeverCount)
	}
	// Far more calls than the bounded default must never exhaust a forever grant.
	for i := 0; i < approveDefaultCount+3; i++ {
		var ok bool
		mm, ok = autoApprove(t, mm, "terminal.sendInput", domain.RiskTerminal)
		if !ok {
			t.Fatalf("forever grant failed to auto-approve call %d", i)
		}
	}
	if got := mm.approvedTools["terminal.sendInput"]; got != allowForeverCount {
		t.Fatalf("forever grant decayed to %d", got)
	}
}

func TestApproval_FDoesNotRememberGit(t *testing.T) {
	// Git/system actions enter typed-confirmation mode, so 'f' is just a phrase character
	// there — it must never become an approve-and-remember-forever shortcut.
	m, ch := approvalPending(t, confirmReq("git.push", domain.RiskGit, ""))
	if m.pending == nil || !m.pending.requireType {
		t.Fatal("an irreversible git action did not enter typed-confirmation mode")
	}
	m2 := asModel(t, mustModel(m.onApprovalKey(runeKey('f'))))
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("'f' approved an irreversible git action in typed-confirm mode")
	}
	if len(m2.approvedTools) != 0 {
		t.Fatal("a git tool was added to the session allow-list via F (must never be remembered)")
	}
}

// lastCommandCell returns the most recent CommandCell in the transcript (the card a slash
// command renders), failing the test if there is none.
func lastCommandCell(t *testing.T, m Model) *CommandCell {
	t.Helper()
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if m.transcript[i].Command != nil {
			return m.transcript[i].Command
		}
	}
	t.Fatal("no command cell in transcript")
	return nil
}

func TestApprovals_CommandListsAndClears(t *testing.T) {
	m := harnessModel()
	m.approvedTools = map[string]int{
		"terminal.sendInput":     3,
		"project.createWorktree": allowForeverCount,
	}
	// /approvals lists each active grant with its remaining count / forever marker.
	mm := asModel(t, mustModel(m.onSubmit("/approvals")))
	card := lastCommandCell(t, mm)
	if card.Title != "Approvals" {
		t.Fatalf("/approvals card title = %q, want Approvals", card.Title)
	}
	if !strings.Contains(card.Text, "terminal.sendInput") || !strings.Contains(card.Text, "3 more") {
		t.Fatalf("/approvals list missing the bounded grant: %q", card.Text)
	}
	if !strings.Contains(card.Text, "forever this session") {
		t.Fatalf("/approvals list missing the forever grant: %q", card.Text)
	}
	// /approvals clear empties the allow-list and reports how many it cleared.
	mm2 := asModel(t, mustModel(mm.onSubmit("/approvals clear")))
	if len(mm2.approvedTools) != 0 {
		t.Fatalf("/approvals clear left %d grants", len(mm2.approvedTools))
	}
	if c := lastCommandCell(t, mm2); !strings.Contains(c.Text, "Cleared") {
		t.Fatalf("/approvals clear card = %q, want a Cleared… note", c.Text)
	}

	// Trailing garbage is a usage error and must not clear the allow-list.
	mm2.approvedTools = map[string]int{"git.commit": 2}
	mm3 := asModel(t, mustModel(mm2.onSubmit("/approvals clear extra")))
	if len(mm3.approvedTools) != 1 {
		t.Fatalf("malformed /approvals clear mutated the allow-list: %+v", mm3.approvedTools)
	}
	if c := lastCommandCell(t, mm3); !strings.Contains(c.Text, "Usage:") {
		t.Fatalf("malformed /approvals clear card = %q, want usage", c.Text)
	}
}

func TestApprovals_CommandEmptyState(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onSubmit("/approvals")))
	card := lastCommandCell(t, mm)
	if !strings.Contains(card.Text, "No active session approvals") {
		t.Fatalf("/approvals empty-state text = %q", card.Text)
	}
}

func TestApproval_PressingAEmitsGrantNote(t *testing.T) {
	m, _ := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000
	mm := asModel(t, mustModel(m.onApprovalKey(runeKey('a'))))
	var found bool
	for _, c := range mm.transcript {
		if c.Note != nil && strings.Contains(c.Note.Text, "terminal.sendInput") {
			found = true
		}
	}
	if !found {
		t.Fatal("pressing A did not announce the grant in scrollback")
	}
}

func TestApproval_GrantIsToolScoped(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.pending.shownAt = domain.NowMS() - 1000
	mm := asModel(t, mustModel(m.onApprovalKey(runeKey('a'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("A did not approve (ok=%v v=%v)", ok, v)
	}
	// A DIFFERENT tool must still surface its own sheet — grants are keyed by exact tool
	// name, never by risk class.
	mm2, autoApproved := autoApprove(t, mm, "project.createWorktree", domain.RiskProject)
	if autoApproved || mm2.pending == nil {
		t.Fatal("a grant for one tool wrongly auto-approved a different tool")
	}
}

func TestApproval_StaleEntriesDoNotAutoApprove(t *testing.T) {
	// 0 (spent) and any negative other than the -1 sentinel must NOT grant — guards the
	// explicit "count > 0 || count == allowForeverCount" check against a bare != 0.
	for _, count := range []int{0, -2, -7} {
		m := harnessModel()
		m.approvedTools = map[string]int{"terminal.sendInput": count}
		mm, autoApproved := autoApprove(t, m, "terminal.sendInput", domain.RiskTerminal)
		if autoApproved || mm.pending == nil {
			t.Fatalf("stale entry %d auto-approved instead of surfacing the sheet", count)
		}
	}
}

func TestApproval_PreseededGitNeverAutoApproves(t *testing.T) {
	// Even a (never-set-in-practice) forever grant on a git tool must not auto-approve:
	// rememberable() gates the auto path, so it falls through to typed-confirmation.
	m := harnessModel()
	m.approvedTools = map[string]int{"git.push": allowForeverCount}
	ch := make(chan bool, 1)
	mm := asModel(t, mustModel(m.Update(ApprovalRequestedMsg{
		Request: confirmReq("git.push", domain.RiskGit, ""), Resolve: ch,
	})))
	if mm.pending == nil || !mm.pending.requireType {
		t.Fatal("a preseeded git grant bypassed typed-confirmation")
	}
	if _, ok := approvalDecision(ch); ok {
		t.Fatal("a preseeded git grant auto-resolved")
	}
}

func TestApprovals_UnknownSubcommandReportsUsage(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onSubmit("/approvals bogus")))
	if card := lastCommandCell(t, mm); !strings.Contains(card.Text, "Usage") {
		t.Fatalf("/approvals bogus = %q, want a usage hint", card.Text)
	}
}

func TestApproval_GitEntersTypedConfirm(t *testing.T) {
	// A RiskGit action (e.g. git.push) may rewrite or publish history — it must require a
	// TYPED confirmation, never single-key approve, and never be remembered. (Regression
	// guard: an earlier `strings.Contains(tool,"push")` gate was name-based; the verdict is
	// driven by the RiskGit class, so ANY git-risk tool routes to typed-confirm.)
	m, ch := approvalPending(t, confirmReq("git.push", domain.RiskGit, ""))
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

func TestApproval_RequireTypeReadsFieldNotRisk(t *testing.T) {
	// The cockpit must gate typed-confirm on the pre-stamped NeedsTypedConfirm field, NOT
	// re-derive it from the risk class. Decouple the two to prove it (regression guard
	// against restoring the deleted needsTypedConfirm(risk) helper):
	//  - a RiskTerminal request with the field SET must enter typed-confirm,
	//  - a RiskGit request with the field CLEARED must not.
	terminalTyped := tools.ConfirmRequest{ToolName: "x.terminal", Risk: domain.RiskTerminal, NeedsTypedConfirm: true}
	mm := asModel(t, mustModel(harnessModel().Update(
		ApprovalRequestedMsg{Request: terminalTyped, Resolve: make(chan bool, 1)})))
	if mm.pending == nil || !mm.pending.requireType {
		t.Fatal("cockpit ignored NeedsTypedConfirm=true on a non-git/system risk (re-deriving from risk?)")
	}

	gitUntyped := tools.ConfirmRequest{ToolName: "git.push", Risk: domain.RiskGit, NeedsTypedConfirm: false}
	mm2 := asModel(t, mustModel(harnessModel().Update(
		ApprovalRequestedMsg{Request: gitUntyped, Resolve: make(chan bool, 1)})))
	if mm2.pending == nil {
		t.Fatal("git request did not surface an approval sheet")
	}
	if mm2.pending.requireType {
		t.Fatal("cockpit re-derived typed-confirm from RiskGit instead of trusting the cleared field")
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
