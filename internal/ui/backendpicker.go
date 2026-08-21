package ui

import (
	"fmt"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"

	tea "charm.land/bubbletea/v2"
)

// backendpicker.go opens `/backend` as a SELECTION SHEET rather than a printed list.
//
// The first version printed the endpoints and told the user to run the command again
// with an argument. That is a menu that cannot be used as one: reading a list and then
// retyping a choice is two steps where the cockpit already has a one-step affordance for
// exactly this, in the question sheet the model uses for user.askMultipleChoice.
//
// It reuses that sheet instead of growing a second one. Same rendering, same ↑/↓ and
// letter keys, same debounce against typed-ahead keystrokes — the only difference is
// pendingQuestion.local, which marks the sheet as user-opened so Esc dismisses it rather
// than cancelling a turn nobody staked on it.
//
// A CUSTOM url stays on the argument form (`/backend https://…`). Offering it here would
// mean a text-entry stage inside the sheet, and the whole point of the sheet is the
// zero-typing path. The hint below says so, in the one place someone looking for it is
// already looking.

// openBackendPicker builds the sheet from the live endpoint list. It is a no-op (with a
// bell) when another sheet already owns the keys, so a picker can never appear on top of
// an approval and steal keystrokes meant for it.
func (m Model) openBackendPicker() (tea.Model, tea.Cmd) {
	if m.app == nil {
		return m, bellCmd()
	}
	if m.pending != nil || m.pendingQuestion != nil {
		return m, bellCmd()
	}

	opts, targets, selected := backendPickerOptions(
		m.app.SnapshotConfig().BackendURL, m.app.HasStoredBackendURL())

	m.pendingQuestion = &pendingQuestion{
		req: tools.AskChoiceRequest{
			Question: "Backend endpoint",
			Options:  opts,
			Default:  selected,
		},
		selected: selected,
		shownAt:  domain.NowMS(),
		local: func(idx int) (string, string) {
			if idx < 0 || idx >= len(targets) {
				return "Backend", "No change."
			}
			return "Backend", m.controller.switchBackend(targets[idx])
		},
	}
	return m.afterStateChange(nil)
}

// backendPickerOptions builds the menu: one row per known endpoint, plus a "forget"
// row when something is actually remembered. It returns the option texts, the target
// each row resolves to, and the index to highlight first.
//
// Split out from openBackendPicker so it can be tested without an App — and because the
// two things it has to get right are worth pinning: the CURRENT endpoint is named in the
// option text (not merely highlighted), and "forget" is offered only when there is
// something to forget.
func backendPickerOptions(current string, hasStored bool) ([]tools.ChoiceOption, []string, int) {
	opts := make([]tools.ChoiceOption, 0, len(app.BackendChoices)+1)
	targets := make([]string, 0, len(app.BackendChoices)+1)
	selected := 0
	for _, c := range app.BackendChoices {
		text := fmt.Sprintf("%-9s %s", c.Alias, c.URL)
		if c.URL == current {
			// Marked in the option TEXT, not only by the highlight: the highlight tracks
			// where the cursor is, which moves the instant someone presses ↓, and the
			// answer to "which am I on" has to survive that.
			text += "   (current)"
			selected = len(opts)
		}
		opts = append(opts, tools.ChoiceOption{Label: choiceLabel(len(opts)), Text: text})
		targets = append(targets, c.URL)
	}
	// Offered only when there is something to forget, so the common case stays a
	// two-row menu and the option never reads as a no-op.
	if hasStored {
		opts = append(opts, tools.ChoiceOption{Label: choiceLabel(len(opts)), Text: "forget the remembered choice (use the default)"})
		targets = append(targets, app.BackendResetAlias)
	}
	return opts, targets, selected
}

// choiceLabel is the per-row answer key (A, B, C…), assigned here for the same reason
// questionx assigns it for the model: the sheet RENDERS the label and the key handler
// derives the answer letter from the option count, so an option built without one shows
// a bare "." where its key should be and cannot be answered by letter at all.
func choiceLabel(i int) string { return string(rune('A' + i)) }
