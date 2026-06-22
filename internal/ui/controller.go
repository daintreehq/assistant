package ui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// controller.go bridges the root model to agent.Session. It enforces single-flight
// Send (one outstanding turn), routes approval confirms to the sheet, and runs
// /clear. Per the concurrency rule, the controller NEVER mutates the model — it only
// launches goroutines that send tea.Msgs back (turn/wake/command completion). The
// model owns the FIFO queue + wake-priority drain in Update.

// controller wraps the App + the pump and exposes turn/command/confirm bridges as
// tea.Cmds that run a Session.Send off the loop and report completion via a msg.
type controller struct {
	app  *app.App
	pump *eventPump

	mu       sync.Mutex
	cancel   context.CancelFunc // the in-flight turn's cancel (per user turn)
	pendingC chan bool          // resolve channel for the in-flight confirm
}

// newController wires the App's hooks: the event pump as the agent-event sink, and
// a confirm hook that surfaces an ApprovalRequestedMsg and BLOCKS the runtime
// goroutine on a resolve channel until the user decides (or shutdown auto-declines).
func newController(a *app.App, pump *eventPump, send func(tea.Msg)) *controller {
	c := &controller{app: a, pump: pump}
	a.SetHooks(app.AppHooks{
		AgentEvents: pump,
		// Confirm blocks the dispatching goroutine: it pushes the request to the UI
		// (carrying a fresh resolve channel) and waits for the decision. The model
		// routes the user's Y/N to the channel via resolveConfirm.
		Confirm: func(ctx context.Context, req tools.ConfirmRequest) (bool, error) {
			resolve := make(chan bool, 1)
			send(ApprovalRequestedMsg{Request: req, Resolve: resolve})
			select {
			case ok := <-resolve:
				return ok, nil
			case <-ctx.Done():
				// Cancelled mid-prompt → decline (fail closed).
				return false, nil
			}
		},
		// Out-of-band log lines become info NoteCells.
		Log: func(msg string) { send(LogMsg{Level: NoteInfo, Text: msg}) },
	})
	return c
}

// runTurn launches a user turn (or slash-derived prompt) under a fresh cancellable
// context, single-flight. turnID is the UI cell id the completion is tagged with so
// the reducer's ordering barrier can reject a completion meant for a turn that is no
// longer active (#1). Completion rides the SAME ordered pump stream as the turn's
// events (pump.Complete), so it can never overtake queued AgentEventMsgs. The model
// must have already verified the single-flight lock is free before calling this.
func (c *controller) runTurn(parent context.Context, turnID, prompt string, readOnly bool) tea.Cmd {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return func() tea.Msg {
		reply, err := c.app.Session.Send(ctx, prompt, agent.SendOptions{ReadOnly: readOnly})
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		cancel() // release the context regardless of how Send returned
		// #8: a single-flight clash (the UI guard and the session guard disagreeing)
		// surfaces ErrTurnInProgress; flag it as a failed turn instead of masquerading
		// as a normal empty completion that silently seals/drains.
		failed := err != nil || isFailureReply(reply)
		c.pump.Complete(completionPayload{runID: turnID, reply: reply, failed: failed})
		return nil
	}
}

// runWake launches an autonomous wake reactor turn (readOnly) — the model only
// inspects & reports, never mutates. Completion rides the ordered
// pump stream tagged with the wake cell id (#1).
func (c *controller) runWake(parent context.Context, turnID, prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return func() tea.Msg {
		reply, err := c.app.Session.Send(ctx, prompt, agent.SendOptions{ReadOnly: true})
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		cancel()
		failed := err != nil || isFailureReply(reply)
		c.pump.Complete(completionPayload{wake: true, runID: turnID, reply: reply, failed: failed})
		return nil
	}
}

// cancelTurn aborts the in-flight turn's context (Esc → Cancelling…). Synchronous;
// the model has already set phase Cancelling before calling this.
func (c *controller) cancelTurn() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runCommand executes a slash command off the loop and reports CommandCompleteMsg.
// Some commands (compact, skills find) hit the model, so this must not block Update.
func (c *controller) runCommand(parent context.Context, line string) tea.Cmd {
	return func() tea.Msg {
		res := commands.HandleUICommand(parent, line, c.app)
		if !res.Handled {
			return nil
		}
		return CommandCompleteMsg{
			Title:           res.Title,
			Text:            res.Text,
			ClearTranscript: res.ClearTranscript,
			Quit:            res.Quit,
			// commands.PanelKey and ui.PanelKey share the same string values; convert across
			// the package seam so the cockpit can act on the requested view switch.
			SwitchPanel: PanelKey(res.SwitchPanel),
		}
	}
}

// isFailureReply reports whether a Session.Send reply is a failure sentinel (Send
// returns a sentinel string on model/tool failure rather than an error, so a wake
// reactor must prefix-match to know it failed). The session's sentinels are in
// domain; we treat the cancelled reply and any error-prefixed reply as failure.
func isFailureReply(reply string) bool {
	return reply == "" ||
		reply == domain.CancelledReply ||
		(len(reply) >= 2 && reply[0] == '[')
}
