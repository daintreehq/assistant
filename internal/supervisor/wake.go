package supervisor

import (
	"context"
	"fmt"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/domain"
)

// agentIsActionableWake mirrors the host/attached session wake filter: only terminal-
// watcher events with a real terminal and async-tool completions wake the
// model autonomously.
func agentIsActionableWake(e domain.QueueEvent) bool { return agent.IsActionableWake(e) }

// unattendedWakeNote rides every daemon wake prompt so the model knows nobody
// is present: no questions, no confirmation prompts — act within grants,
// queue everything else. Prompt-level (not a wire flag) so no backend contract
// change is needed for the model to see it.
const unattendedWakeNote = "\n\n[unattended] The assistant attached session is CLOSED — this turn runs inside the " +
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
	// Off the lock, before anything else: re-read the account posture if the shared
	// marker moved. This is the only place it can do I/O safely.
	r.refreshAuthPosture(ctx)

	r.mu.Lock()
	a := r.app
	if a == nil || r.closing || ctx.Err() != nil || r.wakeBusy || len(r.pendingWake) == 0 {
		r.mu.Unlock()
		return
	}
	// THE stop-paid-work gate.
	//
	// This is the whole reason the auth revision exists. A wake turn is a PAID backend
	// request made with nobody watching, hours after the visible UI closed. When someone
	// logs out in a terminal, this process is still holding an access token that stays
	// cryptographically valid until its expiry — so without a check here it would keep
	// spending on an account the user believes they have signed out of, for up to an
	// hour, and would then discover the fact by 401-looping.
	//
	// It is checked HERE rather than in the backend client because the correct response
	// differs: a client can only fail the request, whereas the daemon can decline to
	// start one at all and put the events back for whoever attaches next. Nothing is
	// lost — pendingWake is preserved, so an operator signing back in finds the work
	// still queued.
	if !r.authorizedToSpendLocked() {
		announce := r.authJustBlocked
		r.authJustBlocked = false
		r.mu.Unlock()
		if announce {
			// Logged off the lock. r.logf can block on a file write, and the runtime
			// mutex serialises the control socket.
			r.logf("daemon: signed out — unattended work is paused until you sign in again")
		}
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
		// Watcher events only (mirrors host/attached session): record the terminals we got
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

// refreshAuthPosture re-reads the account state when the shared marker has moved.
//
// It performs I/O — a marker read and, when that moved, a credential-store lookup — so it
// MUST NOT be called with r.mu held. Doing so was a real bug: the credential lookup can
// reach the OS keychain and, on a machine with no recorded login, discovery could reach
// the network. Holding the runtime mutex across either one blocks the daemon's whole
// control socket, its status replies and its handover behind a call that may take
// seconds or prompt.
func (r *Runtime) refreshAuthPosture(ctx context.Context) {
	r.mu.Lock()
	mgr := r.auth
	r.mu.Unlock()
	if mgr == nil {
		return
	}
	// The marker moves on login, logout and revocation. Unchanged means the cached state
	// is still current, which is the overwhelmingly common case and costs one stat.
	marker := mgr.Revision().Current()
	if marker == mgr.Revision().Observed() {
		return
	}
	// Hydrate FIRST, adopt the marker only if it reached an authoritative answer.
	//
	// Adopting first was a real bug. A logout could be marked observed while the
	// credential store was locked and the hydrate therefore inconclusive; the next check
	// would see "unchanged", read the stale authorized state, and start spending. Worse,
	// the marker was consumed permanently, so the logout would never be retried even
	// after the store recovered. An unresolved posture leaves the marker unobserved so
	// the next tick tries again.
	if mgr.Hydrate(ctx) {
		mgr.Revision().MarkObserved(marker)
	}
}

// authorizedToSpendLocked reports whether this daemon may make a paid backend request.
//
// PURE: it reads cached state and does no I/O whatever, which is what makes it safe to
// call with r.mu held. refreshAuthPosture does the reading, off the lock, first.
//
// It is conservative in one direction only. An install with NO account at all is
// permitted, because the backend's open door still serves anonymous requests and
// refusing here would disable unattended supervision for every existing user. What it
// refuses is a session that DID exist and has since ended — the case where continuing
// means spending someone's money after they told us to stop.
func (r *Runtime) authorizedToSpendLocked() bool {
	if r.auth == nil {
		return true // no account layer configured: the open door applies
	}
	switch r.auth.State() {
	case auth.StateRevoked, auth.StateSignedOut:
		// Reported once per transition. A daemon writing this on every 3s tick would
		// bury the rest of the log.
		if !r.authBlocked {
			r.authBlocked = true
			// The field is set DIRECTLY, not through setError: setError takes r.mu, and
			// this function runs with it already held. The log line goes out after the
			// caller releases, via the returned flag — see reactWake.
			r.lastError = "signed out — unattended work is paused until you sign in again"
			r.authJustBlocked = true
		}
		return false
	}
	r.authBlocked = false
	return true
}

// enforceAuthPosture is the periodic check that bounds how long a signed-out daemon can
// keep spending, and that resumes work after a sign-in.
//
// The gate in reactWake runs ONCE before a turn, but a single turn issues many backend
// rounds and can run for minutes — so a logout part-way through would otherwise permit
// paid rounds for the rest of it. This runs on the monitor tick and cancels the span on a
// blocked transition; the existing cancellation path requeues the burst, so nothing is
// lost. An already-dispatched HTTP request cannot be retracted, but every subsequent
// round can be, and that is the difference between seconds of overspend and minutes.
//
// It also handles the other direction, which the one-shot gate cannot: after a sign-in,
// work already sitting in pendingWake would wait for some unrelated event to arrive. Here
// it is picked up immediately.
func (r *Runtime) enforceAuthPosture(ctx context.Context) {
	r.refreshAuthPosture(ctx)

	r.mu.Lock()
	if r.auth == nil {
		r.mu.Unlock()
		return
	}
	blocked := !r.authorizedToSpendLocked()
	announce := r.authJustBlocked
	r.authJustBlocked = false
	busy := r.wakeBusy
	pending := len(r.pendingWake) > 0
	r.mu.Unlock()

	if announce {
		r.logf("daemon: signed out — unattended work is paused until you sign in again")
	}
	switch {
	case blocked && busy:
		// A turn is running on a session that is no longer entitled to spend.
		r.interruptSupervision()
	case !blocked && pending && !busy:
		// Signed back in with work already queued.
		go r.reactWake(ctx)
	}
}
