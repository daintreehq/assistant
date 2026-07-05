package ui

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/terminal"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// cmds.go holds the out-of-band tea.Cmds (host wipe, BEL) and the App-hook helpers.
// These bypass Bubble Tea's managed render to touch the host terminal directly —
// reserved for /clear, resize redraw, and the attention ding (TTY-gated, never throw).

// hostClearCmd wipes the host screen+scrollback via the TTY-gated escape. It runs
// as a tea.Cmd (returns nil msg) so it is ordered AFTER any preceding commit in a
// Sequence/Batch. The wipe is the ONLY path that clears host scrollback.
func hostClearCmd() tea.Cmd {
	return func() tea.Msg {
		terminal.ClearHost(os.Stdout)
		return nil
	}
}

// bellCmd rings the terminal BEL once (fresh-attention ding). TTY-gated, no-op off
// a terminal, never throws.
func bellCmd() tea.Cmd {
	return func() tea.Msg {
		terminal.Bell(os.Stdout)
		return nil
	}
}

// appAutoDecline rewires the App's confirm + question hooks to auto-reject every future
// request (shutdown: a dispatch must never block on a modal with no UI subscriber). A
// question can't be declined, so it returns context.Canceled — the tool maps that to a
// QUESTION_CANCELLED failure.
func appAutoDecline() app.AppHooks {
	return app.AppHooks{
		Confirm: func(_ context.Context, _ tools.ConfirmRequest) (bool, error) { return false, nil },
		AskChoice: func(_ context.Context, _ tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			return tools.AskChoiceAnswer{}, context.Canceled
		},
	}
}

// sendAttention forwards a scheduler attention burst through the pump as a
// dedicated message. The model maps it to an AttentionBatchMsg in Update. We route
// it through the same channel the runtime events use so ordering stays FIFO and we
// never call Program.Send in a hot path.
func (p *eventPump) sendAttention(events []domain.QueueEvent) {
	// emit (via send) drains buffered tokens first, so the burst stays ordered.
	p.send(pumpEvent{kind: pumpAttention, attention: events})
}
