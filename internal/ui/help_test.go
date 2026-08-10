package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/commands"
)

// help_test.go covers T10: SwitchPanel wiring (the help/ops views are now reachable) and
// the ?-at-empty-prompt help trigger, plus the unified key cheat-sheet.

func TestCommandComplete_SwitchToHelpView(t *testing.T) {
	m := harnessModel()
	before := len(m.transcript)
	next, _ := m.onCommandComplete(CommandCompleteMsg{Title: "Help", Text: "...", SwitchPanel: PanelHelp})
	mm := asModel(t, next)
	if mm.view != viewHelp {
		t.Fatalf("/help did not switch to the help view; view=%v", mm.view)
	}
	if len(mm.transcript) != before {
		t.Fatal("a panel-switch command should not also print a transcript card")
	}
}

func TestCommandComplete_SwitchToOpsPanel(t *testing.T) {
	m := harnessModel()
	next, _ := m.onCommandComplete(CommandCompleteMsg{Title: "Inbox", Text: "...", SwitchPanel: PanelInbox})
	mm := asModel(t, next)
	if mm.view != viewOperations {
		t.Fatalf("/inbox did not switch to the ops deck; view=%v", mm.view)
	}
	if mm.activePanel != PanelInbox {
		t.Fatalf("/inbox did not focus the inbox panel; activePanel=%v", mm.activePanel)
	}
}

func TestCommandComplete_NonSwitchingPrintsCard(t *testing.T) {
	m := harnessModel()
	before := len(m.transcript)
	next, _ := m.onCommandComplete(CommandCompleteMsg{Title: "Status", Text: "all good"})
	mm := asModel(t, next)
	if mm.view != viewHome {
		t.Fatalf("a non-switching command changed the view to %v", mm.view)
	}
	if len(mm.transcript) != before+1 {
		t.Fatal("a non-switching command should print a transcript card")
	}
}

func TestQuestionMark_OnEmptyComposerOpensHelp(t *testing.T) {
	m := harnessModel()
	next, _ := m.onKey(tea.KeyPressMsg{Code: '?', Text: "?"})
	mm := asModel(t, next)
	if mm.view != viewHelp {
		t.Fatalf("? on an empty composer did not open the help view; view=%v", mm.view)
	}
}

func TestQuestionMark_WithTextTypesLiterally(t *testing.T) {
	m := harnessModel()
	m.composer.Restore("what is")
	next, _ := m.onKey(tea.KeyPressMsg{Code: '?', Text: "?"})
	mm := asModel(t, next)
	if mm.view != viewHome {
		t.Fatalf("? with buffered text wrongly opened help; view=%v", mm.view)
	}
	if !strings.HasSuffix(mm.composer.Value(), "?") {
		t.Fatalf("? with text should type literally; buffer=%q", mm.composer.Value())
	}
}

func TestHelpText_IncludesKeyCheatSheet(t *testing.T) {
	help := commands.HelpTextUI()
	for _, want := range []string{"Keys", "Ctrl+C", "Ctrl+R", "command palette", "Ctrl+A/E", "/q, /exit"} {
		if !strings.Contains(help, want) {
			t.Errorf("help text is missing the key cheat-sheet entry %q", want)
		}
	}
}
