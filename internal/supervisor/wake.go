package supervisor

import (
	"context"
	"fmt"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// agentIsActionableWake mirrors the host/cockpit wake filter: only terminal-
// watcher events with a real terminal and async-tool completions wake the
// model autonomously.
func agentIsActionableWake(e domain.QueueEvent) bool { return agent.IsActionableWake(e) }

// unattendedWakeNote rides every daemon wake prompt so the model knows nobody
// is present: no questions, no confirmation prompts — act within grants,
// queue everything else. Prompt-level (not a wire flag) so no backend contract
// change is needed for the model to see it.
const unattendedWakeNote = "\n\n[unattended] The assistant cockpit is CLOSED — this turn runs inside the " +
	"background supervisor and the user will read the outcome later. Do read-only integration " +
	"(summaries, workflow/ledger updates, queue.resolve for items you fully handled). Mutating " +
	"tools require a pre-existing automation grant; without one they are blocked and land in the " +
	"inbox as a pending approval — do NOT retry them, state the blocker in your reply instead. " +
	"Never call user.askMultipleChoice here; publish a concise attention item if a decision is needed."

// reactWake runs the autonomous wake loop — the daemon-side port of the
// embedded host's reactor: single-flight, drains pendingWake, one retry on a
// failed turn, chains while more events arrive. Wake turns are full-capability
// but dispatch as ActorWake (grants gate mutations); an attach handover cancels
// the span context, which unwinds the in-flight Send — the interrupted burst is
// REQUEUED (without consuming the retry budget) and the next supervision span
// delivers it, because the queue already marked those events notified and
// nothing else ever would.
//
// Entry is gated under r.mu: app present, not closing, ctx live, single-flight,
// events pending. The wakeWG.Add happens under the SAME mu hold that checks
// r.closing, and the teardown latches closing under mu before Waiting — that
// mutual exclusion is what makes the Add/Wait pair race-free.
func (r *Runtime) reactWake(ctx context.Context) {
	r.mu.Lock()
	a := r.app
	if a == nil || r.closing || ctx.Err() != nil || r.wakeBusy || len(r.pendingWake) == 0 {
		r.mu.Unlock()
		return
	}
	events := r.pendingWake
	r.pendingWake = nil
	r.wakeBusy = true
	already := make(map[string]struct{}, len(r.summarized))
	for id := range r.summarized {
		already[id] = struct{}{}
	}
	r.wakeWG.Add(1)
	r.mu.Unlock()

	func() {
		defer r.wakeWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				r.setError(fmt.Sprintf("wake panicked: %v", rec))
			}
		}()
		prompt := agent.BuildWakePrompt(events, already) + unattendedWakeNote
		reply, err := a.Session.Send(ctx, prompt, agent.SendOptions{IsWake: true})
		if err != nil {
			// Send only errors on the single-flight guard; treat as a failed wake.
			reply = "Model error: " + err.Error()
		}
		r.mu.Lock()
		switch {
		case ctx.Err() != nil:
			// Interrupted by a handover/shutdown mid-turn: requeue the whole burst
			// for the NEXT span without touching the retry budget — interruption is
			// not failure.
			r.pendingWake = append(append([]domain.QueueEvent{}, events...), r.pendingWake...)
			r.mu.Unlock()
			return
		case agent.IsWakeFailureReply(reply):
			if !r.wakeRetried {
				r.wakeRetried = true
				// Requeue for ONE retry, ahead of anything that arrived meanwhile.
				r.pendingWake = append(append([]domain.QueueEvent{}, events...), r.pendingWake...)
			}
			r.mu.Unlock()
			r.setError("wake turn failed: " + firstLine(reply))
			return
		}
		r.wakeRetried = false
		r.wakeTurns++
		r.lastWakeAt = domain.NowMS()
		r.lastError = ""
		// Watcher events only (mirrors host/cockpit): record the terminals we got
		// a full summary for, so a later burst renders them as one-line acks.
		for _, e := range events {
			if e.Source == domain.SourceTerminalWatcher && e.Target != nil && e.Target.TerminalID != "" {
				r.summarized[e.Target.TerminalID] = struct{}{}
			}
		}
		r.mu.Unlock()
		// Durable "while you were away" record for the next attach.
		a.RecordDetachedWake(reply)
		r.logf("daemon: wake turn completed (" + firstLine(reply) + ")")
	}()

	r.mu.Lock()
	r.wakeBusy = false
	more := len(r.pendingWake) > 0 && ctx.Err() == nil && !r.closing
	r.mu.Unlock()
	if more {
		go r.reactWake(ctx)
	}
}

// firstLine truncates a reply for one-line logging.
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' || i >= 160 {
			return s[:i]
		}
	}
	return s
}
