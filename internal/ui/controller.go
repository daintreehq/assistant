package ui

import (
	"context"
	"errors"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/credentials"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// controller.go bridges the root model to agent.Session. It enforces single-flight
// Send (one outstanding turn), routes approval confirms to the sheet, and runs
// /clear. Per the concurrency rule, the controller NEVER mutates the model — it only
// launches goroutines that send tea.Msgs back (turn/wake/command completion). The
// model owns the FIFO queue + wake-priority drain in Update.

// sessionInjector is the narrow seam the composer uses to fold a typed-while-busy
// message into the running turn (and retract/discard it). *agent.Session satisfies it
// in production; UI tests substitute a fake so the harness needs no real App.
type sessionInjector interface {
	InjectPrompt(text string)
	RetractPendingInjection() (string, bool)
	DiscardPendingInjections()
}

// controller wraps the App + the pump and exposes turn/command/confirm bridges as
// tea.Cmds that run a Session.Send off the loop and report completion via a msg.
type controller struct {
	app    *app.App
	pump   *eventPump
	inject sessionInjector // mid-turn injection seam (c.app.Session in production)

	mu       sync.Mutex
	cancel   context.CancelFunc // the in-flight turn's cancel (per user turn)
	pendingC chan bool          // resolve channel for the in-flight confirm
}

// newController wires the App's hooks: the event pump as the agent-event sink, and
// a confirm hook that surfaces an ApprovalRequestedMsg and BLOCKS the runtime
// goroutine on a resolve channel until the user decides (or shutdown auto-declines).
func newController(a *app.App, pump *eventPump, send func(tea.Msg)) *controller {
	c := &controller{app: a, pump: pump, inject: a.Session}
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
		// AskChoice blocks the dispatching goroutine the same way Confirm does: it pushes
		// the request (with a fresh reply channel) to the UI and waits for the user's pick.
		// The model routes the selection — or a cancel — back through the channel. A
		// cancelled turn (ctx) unblocks it too, reported as context.Canceled so the tool
		// returns QUESTION_CANCELLED rather than a spurious answer.
		AskChoice: func(ctx context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			reply := make(chan questionReply, 1)
			send(QuestionRequestedMsg{Request: req, Reply: reply})
			select {
			case r := <-reply:
				if r.cancelled || r.index < 0 || r.index >= len(req.Options) {
					return tools.AskChoiceAnswer{}, context.Canceled
				}
				opt := req.Options[r.index]
				return tools.AskChoiceAnswer{Label: opt.Label, Index: r.index, Text: opt.Text}, nil
			case <-ctx.Done():
				return tools.AskChoiceAnswer{}, ctx.Err()
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
func (c *controller) runTurn(parent context.Context, turnID, prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return func() tea.Msg {
		// App.Send (not Session.Send) so the first user turn consumes the one-time
		// session-ended-watchers NOTE from message[1].
		reply, err := c.app.Send(ctx, prompt, agent.SendOptions{})
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

// runWake launches an autonomous wake reactor turn — a full-capability turn (same as
// a user turn) so the reactor can both report AND act (relay between agents, resolve
// the inbox item). The per-call confirmation/tier gate governs any mutation.
// Completion rides the ordered pump stream tagged with the wake cell id (#1).
func (c *controller) runWake(parent context.Context, turnID, prompt string) tea.Cmd {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return func() tea.Msg {
		// IsWake: this is an autonomous watcher-wake turn, not user-typed — the footer's
		// goal anchor substitutes the active workflow objective for the wake blob.
		reply, err := c.app.Session.Send(ctx, prompt, agent.SendOptions{IsWake: true})
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

// injectPrompt buffers a message typed while a turn is in flight; the Session folds it
// into the running turn at its next tool-iteration boundary ("between tasks"). The
// short lock inside Session.InjectPrompt makes this safe to call from Update.
func (c *controller) injectPrompt(text string) {
	if c.inject != nil {
		c.inject.InjectPrompt(text)
	}
}

// retractPendingInjection pops the newest buffered-but-not-folded injection (LIFO) so
// the user can edit or drop it; ok is false when nothing is still retractable.
func (c *controller) retractPendingInjection() (string, bool) {
	if c.inject == nil {
		return "", false
	}
	return c.inject.RetractPendingInjection()
}

// discardPendingInjections drops every buffered injection (cancel / clear).
func (c *controller) discardPendingInjections() {
	if c.inject != nil {
		c.inject.DiscardPendingInjections()
	}
}

// runCommand executes a slash command off the loop and reports CommandCompleteMsg.
// Some commands (compact, skills find) hit the model, so this must not block Update —
// their stage labels stream back through the pump (CommandProgress) so the composer's
// busy cue narrates the silent stretches instead of looking idle for the whole run.
func (c *controller) runCommand(parent context.Context, line string) tea.Cmd {
	return func() tea.Msg {
		res := commands.HandleUICommandWithProgress(parent, line, c.app, c.pump.CommandProgress)
		if !res.Handled {
			// EVERY submission that reached here incremented commandsRunning, so even a
			// rejected line (a bare "/") must complete — silently — or the counter leaks
			// and the liveness spinner ticks forever.
			return CommandCompleteMsg{Tracked: true, Unhandled: true}
		}
		return CommandCompleteMsg{
			Tracked:         true,
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

// signIn runs a `/login` attempt off the loop: verify, persist, swap the live backend
// client. It deliberately does NOT go through runCommand's counter — a sheet is already
// showing its own "Checking …" state, and a second busy cue in the footer would double
// count. The parent context is the program's, so a cockpit shutdown aborts the probe.
func (c *controller) signIn(parent context.Context, creds credentials.Credentials) tea.Cmd {
	return func() tea.Msg {
		err := c.app.SignIn(parent, creds)
		return SignInResultMsg{Endpoint: creds.BaseURL, Err: err}
	}
}

// lastSignInWarning forwards the App's caveat from the last successful sign-in ("" when
// fully verified). Nil-App safe for the headless UI harness.
func (c *controller) lastSignInWarning() string {
	if c.app == nil {
		return ""
	}
	return c.app.LastSignInWarning()
}

// currentAPIKey returns the key currently in force, for the sheet's "empty keeps the
// existing key" path. It reads the App's resolved config rather than the credentials
// file, so an env-supplied key works the same way as a stored one.
func (c *controller) currentAPIKey() (string, error) {
	// The headless UI harness builds a controller with no App; treat that as "nothing
	// to keep" rather than panicking deep inside a key handler.
	if c.app != nil {
		// Through the accessor, not the field: SignIn mutates it under the App's config
		// lock and this runs on the cockpit's event loop.
		if k := strings.TrimSpace(c.app.APIKey()); k != "" {
			return k, nil
		}
	}
	return "", errors.New("no current key to keep — enter one")
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
