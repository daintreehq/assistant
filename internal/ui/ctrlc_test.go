package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
)

// ctrlc_test.go locks the staged-Ctrl+C contract: a single press is
// never a hard kill — it cancels an in-flight turn (or, when idle, just arms) and only
// a second press within the window quits. Ctrl+D at an empty composer is EOF → quit.

func ctrlCKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl} }
func ctrlDKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl} }

// asModel re-asserts a tea.Model back to the concrete ui.Model for field inspection.
func asModel(t *testing.T, next tea.Model) Model {
	t.Helper()
	m, ok := next.(Model)
	if !ok {
		t.Fatalf("returned %T, want ui.Model", next)
	}
	return m
}

func TestCtrlC_IdleFirstPressArmsNotQuit(t *testing.T) {
	m := harnessModel()
	next, cmd := m.onKey(ctrlCKey())
	mm := asModel(t, next)
	if mm.quitting {
		t.Fatal("first idle Ctrl+C quit the cockpit; want arm-only (interrupt-then-quit)")
	}
	if !mm.quitArmed {
		t.Fatal("first idle Ctrl+C did not arm the quit window")
	}
	if cmd == nil {
		t.Fatal("first Ctrl+C should schedule a quit-arm expiry tick")
	}
	if got := ansi.Strip(mm.View().Content); !strings.Contains(got, "Press Ctrl+C again to exit") {
		t.Fatalf("footer is missing the staged-quit cue:\n%s", got)
	}
}

func TestCtrlC_SecondPressQuits(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onKey(ctrlCKey())))
	if !mm.quitArmed {
		t.Fatal("precondition: first press must arm")
	}
	mm2 := asModel(t, mustModel(mm.onKey(ctrlCKey())))
	if !mm2.quitting {
		t.Fatal("a second Ctrl+C while armed did not quit")
	}
}

func TestCtrlC_InFlightFirstPressCancels(t *testing.T) {
	m := harnessModel()
	cell := &TurnCell{ID: "turn_x", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true

	mm := asModel(t, mustModel(m.onKey(ctrlCKey())))
	if mm.quitting {
		t.Fatal("first Ctrl+C during a live turn quit; want cancel-first")
	}
	if !mm.quitArmed {
		t.Fatal("first Ctrl+C during a turn did not arm the quit window")
	}
	at := mm.activeTurnCell()
	if at == nil || at.Phase != domain.PhaseCancelling {
		t.Fatalf("turn was not set to Cancelling on first Ctrl+C; got %+v", at)
	}
}

func TestCtrlC_OtherKeyDisarms(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onKey(ctrlCKey())))
	if !mm.quitArmed {
		t.Fatal("precondition: armed")
	}
	mm2 := asModel(t, mustModel(mm.onKey(tea.KeyPressMsg{Code: 'x', Text: "x"})))
	if mm2.quitArmed {
		t.Fatal("a normal keypress did not disarm the staged-quit window")
	}
}

func TestCtrlC_ExpiryDisarmsOnlyMatchingGen(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onKey(ctrlCKey())))
	gen := mm.quitArmGen

	// A stale expiry (older generation) must NOT disarm a freshly armed prompt.
	stale, _ := mm.Update(QuitArmExpireMsg{Gen: gen - 1})
	ms := asModel(t, stale)
	if !ms.quitArmed {
		t.Fatal("a stale expiry tick disarmed the quit window")
	}
	// The matching expiry disarms.
	fresh, _ := ms.Update(QuitArmExpireMsg{Gen: gen})
	mf := asModel(t, fresh)
	if mf.quitArmed {
		t.Fatal("the matching expiry tick did not disarm the quit window")
	}
}

func TestCtrlD_EmptyComposerQuits(t *testing.T) {
	m := harnessModel()
	mm := asModel(t, mustModel(m.onKey(ctrlDKey())))
	if !mm.quitting {
		t.Fatal("Ctrl+D at an empty composer did not quit (EOF)")
	}
}

func TestCtrlD_WithTextDoesNotQuit(t *testing.T) {
	m := harnessModel()
	m.composer.Restore("hello")
	mm := asModel(t, mustModel(m.onKey(ctrlDKey())))
	if mm.quitting {
		t.Fatal("Ctrl+D with buffered text quit; it should fall through to forward-delete")
	}
}

// mustModel adapts the (tea.Model, tea.Cmd) pair of onKey/Update down to the model for
// chaining; the cmd is dropped where the test only inspects state.
func mustModel(next tea.Model, _ tea.Cmd) tea.Model { return next }
