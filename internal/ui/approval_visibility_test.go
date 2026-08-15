package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
)

// approval_visibility_test.go pins the fail-closed rule for an approval the user cannot
// see. On a pane too short or too narrow for the fixed band, footer() shows a one-line
// notice instead of the sheet — but keys still route to the approval, so an affirmative
// would approve a mutating call with no consequence, tool name or args on screen.
// Declining must keep working; approving must not.

// illegibleApproval parks a pending approval on a pane too small to render its sheet.
func illegibleApproval(t *testing.T, rows, cols int) (Model, chan bool) {
	t.Helper()
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	m.rows, m.columns = rows, cols
	// Past the debounce window, so only the visibility gate can be what stops an approval.
	m.pending.shownAt = domain.NowMS() - approveDebounceMs - 50
	if m.approvalLegible() {
		t.Fatalf("fixture precondition: %dx%d was expected to be too small for the sheet", cols, rows)
	}
	return m, ch
}

func TestApproval_TooSmallRefusesAffirmativesButStillDeclines(t *testing.T) {
	// Two ways to be illegible: not enough rows for the band, and not enough columns for
	// the core approve/decline pair.
	panes := []struct {
		name       string
		rows, cols int
	}{
		{name: "too short", rows: 5, cols: 100},
		{name: "too narrow", rows: 40, cols: 18},
	}
	affirmatives := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "Y", key: runeKey('y')},
		{name: "A", key: runeKey('a')},
		{name: "F", key: runeKey('f')},
	}
	for _, pane := range panes {
		for _, aff := range affirmatives {
			t.Run(pane.name+"/"+aff.name, func(t *testing.T) {
				m, ch := illegibleApproval(t, pane.rows, pane.cols)
				next, cmd := m.onApprovalKey(aff.key)
				mm := asModel(t, next)
				if v, ok := approvalDecision(ch); ok {
					t.Fatalf("%s approved (v=%v) an approval the user cannot see", aff.name, v)
				}
				if mm.pending == nil {
					t.Fatal("the sheet was dismissed by a refused affirmative")
				}
				if cmd == nil {
					t.Errorf("a refused affirmative should ring the bell")
				}
			})
		}
		t.Run(pane.name+"/decline", func(t *testing.T) {
			m, ch := illegibleApproval(t, pane.rows, pane.cols)
			asModel(t, mustModel(m.onApprovalKey(runeKey('n'))))
			if v, ok := approvalDecision(ch); !ok || v {
				t.Fatalf("declining must always work (ok=%v v=%v)", ok, v)
			}
		})
		t.Run(pane.name+"/escape", func(t *testing.T) {
			m, ch := illegibleApproval(t, pane.rows, pane.cols)
			asModel(t, mustModel(m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEsc})))
			if v, ok := approvalDecision(ch); !ok || v {
				t.Fatalf("Esc must always decline (ok=%v v=%v)", ok, v)
			}
		})
	}
}

// The typed phrase is the highest-friction approval there is; it must not complete against
// a sheet the user cannot read either.
func TestApproval_TooSmallRefusesTheTypedPhrase(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("git.push", domain.RiskGit, ""))
	m.rows = 5
	m.pending.requireType = true
	m.pending.confirmInput = confirmPhrase
	if m.approvalLegible() {
		t.Fatal("fixture precondition: 5 rows was expected to be too small")
	}
	next, cmd := m.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if v, ok := approvalDecision(ch); ok {
		t.Fatalf("a matched phrase approved (v=%v) against an invisible sheet", v)
	}
	if asModel(t, next).pending == nil {
		t.Fatal("the typed sheet was dismissed")
	}
	if cmd == nil {
		t.Error("the refusal should ring the bell")
	}
	// Escape still declines from the typed sheet.
	m2, ch2 := approvalPending(t, confirmReq("git.push", domain.RiskGit, ""))
	m2.rows = 5
	m2.pending.requireType = true
	asModel(t, mustModel(m2.onApprovalKey(tea.KeyPressMsg{Code: tea.KeyEsc})))
	if v, ok := approvalDecision(ch2); !ok || v {
		t.Fatalf("Esc must decline the typed sheet regardless of size (ok=%v v=%v)", ok, v)
	}
}

// The collapsed footer must say a decision is waiting and which key is safe — otherwise
// the user sees a generic "resize" notice and never learns the turn is blocked on them.
func TestApproval_TooSmallNoticeNamesThePendingDecision(t *testing.T) {
	m, _ := illegibleApproval(t, 5, 100)
	out := ansi.Strip(m.View().Content)
	if !strings.Contains(out, "pending approval") {
		t.Errorf("the collapsed footer must say an approval is waiting:\n%s", out)
	}
	if !strings.Contains(out, "Esc declines") {
		t.Errorf("the collapsed footer must name the key that still works:\n%s", out)
	}
	assertNoHeightOverflow(t, "approval-too-small", m.View().Content, m.rows)

	// With no approval pending the notice stays generic.
	plain := testModel(100)
	plain.rows = 5
	plain.transcript = append(plain.transcript, TranscriptCell{Turn: &TurnCell{
		ID: "t", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS(),
	}})
	plain.activeTurn = "t"
	if got := ansi.Strip(plain.footer()); strings.Contains(got, "pending approval") {
		t.Errorf("no approval pending, yet the notice claims one:\n%s", got)
	}
}

// A normally-sized cockpit is unaffected: the gate must not add friction to the ordinary
// path, or every approval becomes harder for no reason.
func TestApproval_NormalPaneStillApproves(t *testing.T) {
	m, ch := approvalPending(t, confirmReq("terminal.sendInput", domain.RiskTerminal, ""))
	if !m.approvalLegible() {
		t.Fatal("the default harness pane must render the sheet legibly")
	}
	m.pending.shownAt = domain.NowMS() - approveDebounceMs - 50
	asModel(t, mustModel(m.onApprovalKey(runeKey('y'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("a visible sheet must still approve on Y (ok=%v v=%v)", ok, v)
	}
}

// Resizing back up restores the sheet AND the ability to approve — the gate is about the
// current frame, never a latched state.
func TestApproval_ResizeRestoresApproval(t *testing.T) {
	m, ch := illegibleApproval(t, 5, 100)
	m.rows = 40
	if !m.approvalLegible() {
		t.Fatal("growing the pane back must make the sheet legible again")
	}
	asModel(t, mustModel(m.onApprovalKey(runeKey('y'))))
	if v, ok := approvalDecision(ch); !ok || !v {
		t.Fatalf("after a resize the sheet must approve normally (ok=%v v=%v)", ok, v)
	}
}
