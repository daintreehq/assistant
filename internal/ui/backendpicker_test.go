package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// The live endpoint is named in the option TEXT, not merely highlighted. The highlight
// moves the instant someone presses ↓, and "which am I on" has to survive that.
func TestBackendPickerOptionsMarkTheLiveEndpoint(t *testing.T) {
	opts, targets, selected := backendPickerOptions(backend.LocalBaseURL, false)

	if len(opts) != len(targets) {
		t.Fatalf("options and targets disagree: %d vs %d", len(opts), len(targets))
	}
	if targets[selected] != backend.LocalBaseURL {
		t.Errorf("the live endpoint should start highlighted, got %q", targets[selected])
	}
	if !strings.Contains(opts[selected].Text, "(current)") {
		t.Errorf("the live row must say so in its text, got %q", opts[selected].Text)
	}
	for i, o := range opts {
		if i != selected && strings.Contains(o.Text, "(current)") {
			t.Errorf("only one row may be marked current, row %d also is: %q", i, o.Text)
		}
		// Every row needs its answer key. The sheet renders Label verbatim and the key
		// handler derives the answer letters from the option COUNT, so an option built
		// without one shows a bare "." where its key belongs, advertises a truncated
		// "A-" range in the hint, and cannot be answered by letter at all. Caught in a
		// PTY run, not by any assertion that existed at the time.
		if want := string(rune('A' + i)); o.Label != want {
			t.Errorf("row %d label = %q, want %q", i, o.Label, want)
		}
	}
}

// "Forget" appears only when there is something to forget, so it never reads as a no-op.
func TestBackendPickerOffersForgetOnlyWhenSomethingIsStored(t *testing.T) {
	plain, _, _ := backendPickerOptions(backend.DefaultBaseURL, false)
	for _, o := range plain {
		if strings.Contains(o.Text, "forget") {
			t.Fatalf("nothing is stored; the forget row must be absent, got %q", o.Text)
		}
	}

	stored, targets, _ := backendPickerOptions(backend.DefaultBaseURL, true)
	if len(stored) != len(plain)+1 {
		t.Fatalf("want one extra row when a choice is stored, got %d vs %d", len(stored), len(plain))
	}
	if targets[len(targets)-1] != "default" {
		t.Errorf("the forget row must resolve to the reset target, got %q", targets[len(targets)-1])
	}
}

// localPickerFixture builds the sheet the way openBackendPicker does, without needing an
// App (the UI test harness has none).
func localPickerFixture(t *testing.T, chosen *int) *pendingQuestion {
	t.Helper()
	opts, _, selected := backendPickerOptions(backend.DefaultBaseURL, true)
	return &pendingQuestion{
		req:      tools.AskChoiceRequest{Question: "Backend endpoint", Options: opts, Default: selected},
		selected: selected,
		shownAt:  domain.NowMS(),
		local: func(i int) (string, string) {
			*chosen = i
			return "Backend", "switched"
		},
	}
}

// It renders as a picker, not as Daintree interrupting — and Esc is described as what it
// does here, which is close the sheet, not cancel a turn.
func TestLocalQuestionSheetRendersAsAPicker(t *testing.T) {
	got := -1
	out := renderQuestion(darkTheme(), localPickerFixture(t, &got), 100)

	if !strings.Contains(out, "Backend endpoint") {
		t.Errorf("the sheet should carry the picker's own title, got:\n%s", out)
	}
	if strings.Contains(out, "needs a decision") {
		t.Errorf("a sheet the user opened must not claim Daintree interrupted them:\n%s", out)
	}
	if !strings.Contains(out, "dismiss") || strings.Contains(out, "Esc cancel") {
		t.Errorf("Esc closes this sheet; it must not read as cancelling work, got:\n%s", out)
	}
}

// ↑/↓ move the highlight. Without this it is the printed list again, with extra steps.
func TestLocalQuestionSheetMovesWithArrows(t *testing.T) {
	got := -1
	m := testModel(100)
	m.pendingQuestion = localPickerFixture(t, &got)
	start := m.pendingQuestion.selected

	m2, _ := m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if now := m2.(Model).pendingQuestion.selected; now == start {
		t.Fatalf("↓ did not move the highlight (still %d)", now)
	}
	m3, _ := m2.(Model).onQuestionKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if now := m3.(Model).pendingQuestion.selected; now != start {
		t.Errorf("↑ should return to %d, got %d", start, now)
	}
}

// Esc DISMISSES a user-opened picker. Cancelling the turn instead would destroy work the
// user never staked on this decision — the reason pendingQuestion.local exists at all.
func TestLocalQuestionSheetEscapeDoesNotCancelTheTurn(t *testing.T) {
	got := -1
	m := testModel(100)
	m.pendingQuestion = localPickerFixture(t, &got)
	m.inFlight = true

	m2, _ := m.onQuestionKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	final := m2.(Model)
	if final.pendingQuestion != nil {
		t.Error("Esc must close the picker")
	}
	if !final.inFlight {
		t.Error("Esc on a user-opened picker must not cancel the running turn")
	}
	if got != -1 {
		t.Errorf("dismissing must not choose anything, but option %d ran", got)
	}
}

// THE reason render priority must mirror input priority, and the most dangerous bug in
// this feature.
//
// onKey routes to the approval sheet FIRST. If View rendered the question sheet on top
// of a live approval, the user would see a picker and be typing into an invisible
// approval: `A` means "pick option A" on the sheet they can see and "approve, and stop
// asking" on the one they cannot. The old sign-in sheet carried this guard; the question
// sheet argued it away on the grounds that only one TOOL can block at a time, which
// stopped being true when /backend began opening this sheet from an idle cockpit — an
// attention event can start an autonomous wake underneath it that then asks to approve
// a mutating tool.
func TestApprovalRendersOverAPickerBecauseItOwnsTheKeys(t *testing.T) {
	got := -1
	m := testModel(100)
	m.pendingQuestion = localPickerFixture(t, &got)
	m.pending = &pendingConfirm{
		req:     tools.ConfirmRequest{ToolName: "terminal.run", Summary: "rm -rf something"},
		shownAt: domain.NowMS(),
	}

	out := m.View().Content
	if strings.Contains(out, "Backend endpoint") {
		t.Error("the picker must NOT render over a live approval — its keys go to the approval")
	}
	if !strings.Contains(out, "terminal.run") {
		t.Errorf("the approval owns the keys, so it must be what is on screen:\n%s", out)
	}

	// The picker is not destroyed, only yielded: it comes back once the approval is gone.
	m.pending = nil
	if back := m.View().Content; !strings.Contains(back, "Backend endpoint") {
		t.Errorf("the picker should reappear after the approval is answered:\n%s", back)
	}
}
