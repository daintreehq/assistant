package host

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// defaultTurnJoinTimeout bounds teardown's wait for cancelled prompt/wake turns to
// unwind before app.Shutdown closes the resources they use. Generous enough for a
// cooperative Send to observe its cancelled context; short enough that a wedged
// turn can't hang shutdown.
const defaultTurnJoinTimeout = 5 * time.Second

// defaultAppShutdownTimeout bounds teardown's WAIT for App.Shutdown, which today
// takes no context of its own (internal/app.App.Shutdown() is a plain
// error-returning call, and cancelling this wait cannot interrupt whatever it is
// doing partway through — closing that gap end to end would mean threading a
// context through every subsystem's own Close/Drain, which is real future work,
// not this). What this bound DOES guarantee: teardown itself never hangs forever
// on a stuck subsystem. If App.Shutdown does not return in time, teardown logs it,
// reports a non-zero exit, and proceeds — the same backstop the transport's write
// path relies on for an uninterruptible Write(): process exit is what actually
// releases the flock and any other held OS resource, not a clean goroutine join.
// Tests shrink it (Host.appShutdownTimeout).
const defaultAppShutdownTimeout = 10 * time.Second

// handleCommand dispatches one validated command. It must NOT
// block the command loop on a running Send — prompt/wake run on worker goroutines
// while interrupt/approval:decide keep being serviced.
func (h *Host) handleCommand(cmd HostCommand) {
	if !h.ready || h.bridge == nil || h.app == nil {
		h.report("not-ready", "Host is still starting.")
		return
	}

	// A command that owns the session refuses prompts and other commands for as long as
	// it holds it — see Host.cmdExclusive. Deliberately narrow: question answers,
	// approvals, interrupt and shutdown all still land, because the thing holding the
	// session is a sheet that only an answer (or a Stop) can settle.
	//
	// Prompts are let back in slightly EARLIER than commands (cmdPromptsReleased), so
	// the two are asked separately.
	promptsBlocked, commandsBlocked := h.exclusiveGates()
	if (cmd.Type == CmdPrompt && promptsBlocked) || (cmd.Type == CmdCommand && commandsBlocked) {
		h.report("command-busy",
			"A command is waiting on your answer. Answer it, or interrupt it, before sending anything else.")
		return
	}

	switch cmd.Type {
	case CmdPrompt:
		h.handlePrompt(cmd.Text)
	case CmdApprovalDecide:
		// Resolving an approval unblocks a parked dispatch goroutine. Off-loop-safe:
		// the bridge guards its own state, so call directly (no blocking).
		h.bridge.ResolveApproval(cmd.ApprovalID, ConfirmationDecision(cmd.Decision))
	case CmdCommand:
		// A command the App reports as slow goes to a worker; everything else runs
		// inline, as it always has. See handleSlashCommandAsync.
		if runner, ok := h.app.(CommandProgressRunner); ok && runner.IsSlowCommand(cmd.CommandLine) {
			h.handleSlashCommandAsync(cmd.CommandLine)
		} else {
			h.handleSlashCommand(cmd.CommandLine)
		}
	case CmdOperations:
		h.postOperations()
	case CmdTimers:
		h.postTimers()
	case CmdTimerCancel:
		h.cancelTimer(cmd.TimerID)
	case CmdInterjectRetract:
		h.retractInjection()
	case CmdQuestionAnswer:
		// Same shape as an approval decision, and unblocks a dispatch the same way. A
		// negative index is the host saying the user dismissed the sheet, which cancels
		// the tool call rather than answering it.
		h.bridge.ResolveQuestion(cmd.QuestionID, cmd.ChoiceIndex)
	case CmdInterrupt:
		h.handleInterrupt()
	case CmdHibernate:
		// Cancel any in-flight turn + reject pending approvals BEFORE teardown — a
		// command-driven hibernate must unpark a blocked dispatch exactly like the
		// parent-exit path (teardown re-does both, idempotently, and additionally
		// cancels + joins a live WAKE turn). The resume handle IS the sessionId
		// (conversation persists keyed by it).
		h.cancelTurn()
		h.teardown(ShutdownHibernate, h.sessionID)
	case CmdShutdown:
		// Same: cancel the active turn before teardown so a running Send unwinds.
		h.cancelTurn()
		h.teardown(ShutdownExit, "")
	}
}

// handlePrompt runs a command-driven turn. A prompt sent while a turn is already
// running is FOLDED into the running turn (InjectPrompt) — the model picks it up at its
// next tool-iteration boundary ("between tasks"), matching the host composer — rather
// than rejected. The send runs on a worker goroutine so the command loop keeps servicing
// interrupt/decide.
func (h *Host) handlePrompt(text string) { h.admitPrompt(text, false) }

// admitPrompt is handlePrompt with the caller saying whether the prompt was RECLAIMED —
// already accepted by the engine and already removed from the injection queue.
//
// The distinction only matters when a command owns the session. An inbound prompt is
// refused (the words are still in the composer); a reclaimed one is HELD, because
// refusing it throws away a message nobody was told about.
//
// Checked HERE rather than before the call, under the same lock hold that claims busy.
// A separate guard was a TOCTOU: it read the flag, released the lock and then called in,
// and a `/backend` claiming exclusivity in that gap meant the reclaimed prompt was
// admitted anyway and lost to the reservation it had just been protected from.
func (h *Host) admitPrompt(text string, reclaimed bool) {
	// Mint the aborter + claim busy under one lock so a worker's finally can't race
	// the busy check. interrupt cancels turnCancel. The generation counter is the
	// identity guard: a stale finally must not null a newer turn's cancel (Go funcs
	// aren't comparable, so we tag each turn).
	ctx, cancel := context.WithCancel(h.runCtx)
	h.turnMu.Lock()
	if h.cmdExclusive && !h.cmdPromptsReleased {
		if reclaimed {
			// Appended under the SAME hold that observed the flag. Unlocking first and
			// re-locking to append is the same TOCTOU one layer down: the command's
			// unwind can clear exclusivity and drain an empty queue in that gap, and the
			// append then lands behind a drain that will never run again — the prompt
			// stranded for the life of the session.
			h.deferredPrompts = append(h.deferredPrompts, text)
			h.turnMu.Unlock()
			cancel()
			return
		}
		h.turnMu.Unlock()
		cancel()
		h.report("command-busy",
			"A command is waiting on your answer. Answer it, or interrupt it, before sending anything else.")
		return
	}
	if h.closing {
		// Teardown already latched: no new turn may start (the turnWG join below
		// would otherwise race a fresh Add).
		h.turnMu.Unlock()
		cancel()
		h.report("not-ready", "Host is shutting down.")
		return
	}
	if h.busy {
		h.turnMu.Unlock()
		cancel()
		// Fold it into the in-flight turn instead of rejecting. Daintree's parent already
		// holds the text it sent, so there's no echo — just a status so it knows the prompt
		// joined the running turn rather than starting a new one.
		h.session.InjectPrompt(text)
		// RACE CLOSE: the running turn may have passed its FINAL injection-fold
		// check and completed before the injection above landed — the prompt would
		// then sit buffered forever while we report it folded. Re-check under the
		// lock: if the turn is gone, reclaim whatever is still buffered and
		// dispatch it as a fresh turn. (The other interleaving — the injection
		// landing BEFORE the turn's finally releases busy — is covered by
		// finishPromptTurn's own reclaim, which runs while busy is still held.)
		h.turnMu.Lock()
		stillBusy := h.busy
		h.turnMu.Unlock()
		if !stillBusy {
			if stranded := h.reclaimStrandedInjections(); stranded != "" {
				h.dispatchReclaimedPrompt(stranded)
				return
			}
		}
		h.report("prompt-folded",
			"A turn is already running; this message was folded into it and will be picked up between tasks.")
		return
	}
	h.busy = true
	h.turnGen++
	gen := h.turnGen
	h.turnCancel = cancel
	// Registered under the SAME lock hold that checked `closing`, so teardown's
	// bounded join always covers this worker.
	h.turnWG.Add(1)
	h.turnMu.Unlock()

	h.bridge.StartExchange()

	go func() {
		defer h.turnWG.Done()
		defer func() {
			// A panic in a turn worker is fatal-for-turn, not fatal-for-host.
			if r := recover(); r != nil {
				h.report("turn-failed", fmt.Sprintf("turn panicked: %v\n%s", r, debug.Stack()))
			}
			h.finishPromptTurn(gen, ctx)
		}()
		if _, err := h.session.Send(ctx, text, agent.SendOptions{}); err != nil {
			h.report("turn-failed", fmt.Sprintf("send failed: %v", err))
		}
		// The one-time session-ended-watchers note is owned by the Session now (it surfaces
		// once in the uncached footer on the first turn), so the host no longer consumes it.
	}()
}

// finishPromptTurn is the prompt finally. Identity guard: only clear turnCancel
// when this turn's generation is still the active one (a later prompt bumped the
// generation, so a stale finally leaves the newer turn's cancel intact). Then
// settle any dangling assistant turn, reclaim injections the turn never
// consumed, clear busy, and drain deferred wakes.
func (h *Host) finishPromptTurn(gen uint64, ctx context.Context) {
	// Cost BEFORE the turn settles, so it still carries the turn id it belongs to.
	h.postCost()
	h.bridge.SettleTurn(OutcomeAnswered)

	cancelled := ctx.Err() != nil
	if cancelled {
		// An aborted turn drops its unconsumed injections (the host's Ctrl+C
		// discard): a message folded into work the user abandoned must not
		// resurrect as a fresh turn. handleInterrupt discards too — this covers
		// an injection that slipped in while the cancelled turn was unwinding.
		h.session.DiscardPendingInjections()
	}

	h.turnMu.Lock()
	if h.turnGen == gen {
		h.turnCancel = nil
	}
	// Reclaim injections the finished turn never folded in, BEFORE releasing
	// busy: while busy is held no new turn can start, so anything buffered here
	// is provably stranded (Send has already returned — its final fold check is
	// behind us). An injection landing after this reclaim is caught by
	// handlePrompt's own post-inject busy re-check instead. Together the two
	// checks close the "prompt reported folded but never consumed" race.
	//
	// Cancelled turns reclaim too: the Ctrl+C discard above already dropped
	// everything folded into the abandoned work, so an injection still buffered
	// HERE provably arrived after that discard — a new prompt typed while the
	// turn unwound. It must become a fresh turn, not vanish with the old one.
	stranded := ""
	if !h.closing {
		stranded = h.reclaimStrandedInjections()
	}
	h.busy = false
	more := len(h.pendingWake) > 0
	h.turnMu.Unlock()
	if stranded != "" {
		// Dispatch the stranded prompt as a fresh command turn. Deferred wakes
		// stay queued — that turn's own finally drains them.
		h.dispatchReclaimedPrompt(stranded)
		return
	}
	if more {
		go h.reactWake()
	}
}

// reclaimStrandedInjections drains every buffered-but-unfolded injection from
// the session (retraction is LIFO; arrival order is restored) and returns them
// joined as one prompt text — "" when none. Used by the strand-race closes in
// handlePrompt/finishPromptTurn/reactWake.
func (h *Host) reclaimStrandedInjections() string {
	var texts []string
	for {
		text, ok := h.session.RetractPendingInjection()
		if !ok {
			break
		}
		texts = append(texts, text)
	}
	if len(texts) == 0 {
		return ""
	}
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
	return strings.Join(texts, "\n\n")
}

// handleInterrupt is the three coordinated actions. Order
// matters: abort the turn signal, reject pending approvals (an awaited confirm
// can't be unparked by the signal alone), then the display-side bridge interrupt.
//
// DELIBERATE: interrupt aborts only COMMAND turns (turnCancel), never a wake turn.
// A wake has already claimed its attention burst (the queue marked those events
// notified), so a user abort would strand them undelivered; prompts sent during a
// wake fold into it via InjectPrompt instead. Shutdown/hibernate/parent-exit DO
// cancel wakes — see teardown/cancelWake.
func (h *Host) handleInterrupt() {
	// A slow command first, and unconditionally. A slow command is the work a user can
	// be left staring at with no other way out — a browser tab that is theirs to
	// abandon, a picker they have decided not to answer — and Stop is where they will
	// reach for that. Cancelled alongside a turn rather than instead of one: the two are
	// independent, so a command running beside a turn must not survive a Stop aimed at
	// either.
	h.cancelCommand()

	h.turnMu.Lock()
	cancel := h.turnCancel
	h.turnMu.Unlock()
	if cancel != nil {
		// Drop buffered-but-unfolded injections BEFORE cancelling: the turn they
		// were folded into is being abandoned, and finishPromptTurn must not
		// resurrect them as a fresh turn (mirrors the host's Ctrl+C discard).
		// Guarded on a live COMMAND turn: during a wake (turnCancel nil) folded
		// prompts stay — the wake keeps running and will consume them.
		h.session.DiscardPendingInjections()
		cancel()
	}
	h.bridge.SettlePendingApprovals(DecisionRejected)
	h.bridge.SettlePendingQuestions()
	if cancel == nil {
		h.turnMu.Lock()
		waking := h.wakeCancel != nil
		h.turnMu.Unlock()
		if waking {
			// A WAKE turn, which this interrupt deliberately does not abort (see
			// above). Closing its visible turn as cancelled would report work as
			// stopped while it carries on invisibly, so say what happened instead.
			//
			// Checked against wakeCancel rather than inferred from a nil turnCancel: a
			// nil one only means no COMMAND turn is running, which is also true when a
			// Stop lands just after an ordinary turn finished — and announcing
			// background work there would invent a wake that does not exist.
			h.bridge.Info("That was background work the assistant started on its own. " +
				"Stopping does not abort it — it will finish on its own.")
		}
		return
	}
	h.bridge.Interrupt()
}

// cancelTurn aborts the in-flight COMMAND turn's context (if any). Safe to call
// with no active turn.
func (h *Host) cancelTurn() {
	h.turnMu.Lock()
	cancel := h.turnCancel
	h.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelWake aborts the in-flight WAKE turn's context (if any). Called only on
// the shutdown paths (teardown) — user interrupt deliberately leaves wakes alone
// (see handleInterrupt). Safe to call with no active wake.
func (h *Host) cancelWake() {
	h.turnMu.Lock()
	cancel := h.wakeCancel
	h.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelCommand aborts an in-flight SLOW command (see handleSlashCommandAsync).
//
// The loopback listener a `/login` is parked on selects on its context, so cancelling
// here closes the socket and unwinds the wait immediately. Without it, shutdown reaches
// its bounded join with that wait still running, times out, and tears the App down
// underneath a command still holding it.
func (h *Host) cancelCommand() {
	h.turnMu.Lock()
	cancel := h.cmdCancel
	h.turnMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// reactWake runs an autonomous wake turn — full capability, like a user turn, so the
// reactor can act (relay between agents, resolve inbox items), not just report. Gated
// by busy/ready/closing; drains pendingWake; chains itself if more remain.
//
// Cancellation semantics: a wake registers its own child cancel context
// (wakeCancel), which the SHUTDOWN paths (teardown — shutdown/hibernate/parent-exit)
// cancel and then join before app.Shutdown, so resources are never closed under a
// live Send. User interrupt deliberately does NOT abort a wake (the burst is
// already claimed — see handleInterrupt); prompts arriving mid-wake fold in via
// InjectPrompt.
func (h *Host) reactWake() {
	h.turnMu.Lock()
	// cmdExclusive gates this as firmly as busy does, and the reason is a data-loss bug
	// rather than a preference. A session-owning command holds the endpoint reservation
	// — `/backend` with no argument does, while its picker is open — so a wake admitted
	// here would reach Session.Send and be refused. The burst has ALREADY been marked
	// notified by the scheduler and has already been drained from pendingWake by the
	// time that happens, and the retry path requeues only once per event (wakeRetries): a second
	// refusal drops the events for good, in this process and across a restart.
	//
	// Deferring instead costs nothing. The events stay queued, and the command's own
	// unwind re-drives this the moment it finishes.
	//
	// EXCLUSIVE, not merely slow. `/login` is slow and holds nothing — prompts are
	// admitted throughout it — so deferring autonomous work for its five minutes would
	// be a policy this codebase states nowhere and contradicts everywhere else.
	if h.busy || h.cmdExclusive || h.closing || !h.ready || h.bridge == nil || h.app == nil {
		h.turnMu.Unlock()
		return
	}
	events := h.pendingWake
	h.pendingWake = nil
	if len(events) == 0 {
		h.turnMu.Unlock()
		return
	}
	h.busy = true
	ctx, cancel := context.WithCancel(h.runCtx)
	h.wakeCancel = cancel
	// Add under the SAME lock hold that checked `closing` (the supervisor wakeWG
	// pattern): teardown latches closing before Waiting, so this registration can
	// never race the join.
	h.turnWG.Add(1)
	// Snapshot the cross-burst summarized set for the prompt builder.
	already := make(map[string]struct{}, len(h.summarizedTerminals))
	for id := range h.summarizedTerminals {
		already[id] = struct{}{}
	}
	h.turnMu.Unlock()

	// Deferred FIRST so it runs LAST: the whole unwind (settle, busy clear) is
	// covered by teardown's join.
	defer h.turnWG.Done()

	h.bridge.StartWakeExchange()

	func() {
		defer func() {
			if r := recover(); r != nil {
				h.report("wake-failed", fmt.Sprintf("wake panicked: %v", r))
			}
		}()
		// Judged HERE, not when the burst was queued: a message can sit behind another
		// turn for as long as that turn takes, and one that went stale meanwhile must
		// not spend a turn being mistaken for observed activity.
		if events = agent.DropStaleTimerMessages(events); len(events) == 0 {
			return
		}
		// A scheduled message runs alone; anything else in this burst goes back on the
		// queue rather than being folded into someone else's errand.
		// Deferred back onto the IN-MEMORY queue, not re-armed durably.
		//
		// The scheduler calls its attention callback and only THEN marks the burst
		// notified (Scheduler.notify), so a ClearNotified from in here races that mark
		// and usually loses: the events come back notified and are never delivered
		// again. Re-queuing sidesteps the race entirely and is what both reactors
		// already do for a retry — reactWake chains while anything remains, so the
		// deferred events get their own turn immediately after this one.
		var deferred []domain.QueueEvent
		events, deferred = agent.SplitWakeBatch(events)
		if len(deferred) > 0 {
			h.turnMu.Lock()
			h.pendingWake = append(append([]domain.QueueEvent{}, deferred...), h.pendingWake...)
			h.turnMu.Unlock()
		}
		prompt := agent.BuildWakePrompt(events, already)
		// IsWake: autonomous watcher-wake turn (not user-typed) → the footer anchors on the
		// active workflow objective instead of echoing the verbose wake blob.
		reply, err := h.session.Send(ctx, prompt, agent.SendOptions{
			IsWake:           true,
			FromTimerMessage: agent.BurstHasTimerMessage(events),
		})
		if ctx.Err() != nil && (err != nil || reply == domain.CancelledReply) {
			// Shutdown/hibernate cancelled the wake mid-turn. Detect it via ctx —
			// NOT via err: the real Session reports cooperative cancellation as
			// (CancelledReply, nil), never an error, so an err-keyed check would
			// silently drop the burst. Requeue it (without consuming the retry
			// budget) and stay quiet — interruption is not failure. Wakes are only
			// ever cancelled by teardown, whose post-join sweep DURABLY re-arms
			// everything left in pendingWake (the queue already stamped these
			// events notified, and this in-memory requeue dies with the process) so
			// the next run re-delivers them. This requeue happens under turnMu
			// BEFORE the worker's turnWG.Done, which is what guarantees teardown's
			// join-then-sweep observes it.
			h.turnMu.Lock()
			sweepDone := h.wakeSweepDone
			if !sweepDone {
				h.pendingWake = append(append([]domain.QueueEvent{}, events...), h.pendingWake...)
			}
			h.turnMu.Unlock()
			if sweepDone {
				// This wake outlived the bounded join: the durable sweep already
				// ran, so an in-memory requeue would die with the process. Re-arm
				// directly instead — best-effort, the store may itself be
				// mid-shutdown by now.
				h.rearmWakeEvents(events)
			}
			return
		}
		if err != nil {
			h.report("wake-failed", fmt.Sprintf("wake send failed: %v", err))
			h.turnMu.Lock()
			// PER EVENT. A shared flag meant one event's failure could spend the
			// retry belonging to an unrelated one — and since a message now takes its
			// turn alone, that unrelated one is routinely the user's own instruction.
			if h.wakeRetries == nil {
				h.wakeRetries = agent.RetryLedger{}
			}
			// Requeue ONLY what still has a retry, so an exhausted event cannot ride
			// back in on a neighbour's budget.
			if retry := h.wakeRetries.TakeRetry(events); len(retry) > 0 {
				// Unshift — preserve order ahead of new events.
				h.pendingWake = append(retry, h.pendingWake...)
			}
			h.turnMu.Unlock()
			return
		}
		h.turnMu.Lock()
		if h.wakeRetries != nil {
			h.wakeRetries.Done(events)
		}
		// Only record terminals as summarized on a REAL reply — Send returns a
		// sentinel string on model failure (never throws), so a transient outage
		// must not permanently downgrade later events to one-line acks. WATCHER
		// events only (mirrors the host): an async-tool completion carries a
		// terminalId too, but must not poison the "got a full watcher summary" set
		// — a later real watcher event for that terminal still earns the summary.
		if !agent.IsWakeFailureReply(reply) {
			for _, e := range events {
				if e.Source == domain.SourceTerminalWatcher && e.Target != nil && e.Target.TerminalID != "" {
					h.summarizedTerminals[e.Target.TerminalID] = struct{}{}
				}
			}
		}
		// Collected under the lock, RESOLVED outside it. Closing an errand is a SQLite
		// write, and a busy database would otherwise hold turnMu for the whole busy
		// timeout — blocking the finalisation of a turn that has already succeeded.
		var toResolve []string
		if !agent.IsWakeFailureReply(reply) {
			// Only on a REAL reply: a model failure means the instruction was not
			// carried out, and resolving it then would bury work nobody did.
			toResolve = agent.TimerMessageEventIDs(events)
		}
		h.turnMu.Unlock()
		// The errand is done, so close it. Left open it keeps the supervisor from ever
		// reaching idle exit, and a repeating message grows one permanent inbox row per
		// firing.
		if len(toResolve) > 0 && h.app != nil {
			if failed := h.app.ResolveAttention(toResolve); len(failed) > 0 {
				// NOT debug-only: debug logging is off in normal use, and a failed
				// resolve leaves the errand open AND notified, which nothing retries.
				h.report("attention-resolve-failed", fmt.Sprintf(
					"carried out %d scheduled message(s) but could not close %d inbox item(s); they will look unhandled",
					len(toResolve), len(failed)))
			}
		}
	}()

	// Wake turns spend money too — often the utility calls a user has no other way to
	// see — so report before settling, exactly as the prompt path does. Without this
	// the figure went stale until the next interactive turn happened to finish.
	h.postCost()
	h.bridge.SettleTurn(OutcomeAnswered)

	h.turnMu.Lock()
	h.wakeCancel = nil
	// Same strand close as finishPromptTurn: a prompt folded into this wake near
	// its end may never have been consumed — reclaim it (before releasing busy)
	// and dispatch it as a fresh command turn. A cancelled wake skips this: the
	// shutdown paths latch closing first, so nothing new may start.
	stranded := ""
	if ctx.Err() == nil && !h.closing {
		stranded = h.reclaimStrandedInjections()
	}
	h.busy = false
	more := len(h.pendingWake) > 0 && !h.closing
	h.turnMu.Unlock()
	cancel() // release the child context's resources
	if stranded != "" {
		// Deferred wakes stay queued — the dispatched turn's finally drains them.
		h.dispatchReclaimedPrompt(stranded)
		return
	}
	if more {
		go h.reactWake()
	}
}

// rearmWakeEvents durably re-arms an undelivered wake burst's queue events so
// the NEXT process run re-delivers them (see App.RearmAttention). Best-effort: a
// failure is logged to stderr. nil-app / empty-burst safe.
func (h *Host) rearmWakeEvents(events []domain.QueueEvent) {
	if h.app == nil {
		return
	}
	ids := make([]string, 0, len(events))
	for _, e := range events {
		if e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := h.app.RearmAttention(ids); err != nil {
		h.tr.diag(fmt.Sprintf("host: failed to re-arm %d undelivered wake event(s) for the next run: %v", len(ids), err))
	}
}

// teardown emits host:shutdown FIRST (so Daintree sees the reason even if the App
// shutdown hangs), drains pending approvals, cancels + joins in-flight turns
// (bounded), shuts the App, flushes, and exits. Idempotent via teardownOnce.
func (h *Host) teardown(reason HostShutdownReason, resumeSessionID string) {
	h.teardownOnce.Do(func() {
		// Latch closing FIRST, under turnMu (the supervisor wakeWG pattern): after
		// this no prompt/wake worker can register on turnWG, so the bounded join
		// below can never race a fresh Add.
		h.turnMu.Lock()
		h.closing = true
		h.turnMu.Unlock()
		// Abort the in-flight command turn AND any live wake turn so their Sends
		// unwind before app.Shutdown closes the resources they use. (Command paths
		// already call cancelTurn before teardown; both are idempotent.)
		h.cancelTurn()
		h.cancelWake()
		// And an in-flight slow command. It is the one worker that can be parked on a
		// human rather than a model — a sign-in waits five minutes for a browser, a
		// picker waits as long as someone leaves its sheet open — so leaving it out
		// meant every shutdown during one burned the whole join timeout and then closed
		// the App under it.
		h.cancelCommand()
		// Reject every outstanding approval so a parked dispatch unblocks (declined).
		if h.bridge != nil {
			h.bridge.SettlePendingApprovals(DecisionRejected)
			h.bridge.SettlePendingQuestions()
		}
		// host:shutdown BEFORE app.Shutdown() — reason reaches Daintree regardless.
		// sendSync routes it through the SAME queue as every other frame, marked
		// terminal: the writer goroutine writes it in FIFO order after anything
		// already queued and then stops for good, so nothing can ever follow it on
		// the wire. Close() (after sendSync returns) then stops the reader. (Events a
		// dying turn posts after this point are refused at enqueue time — see
		// stampAndEnqueue's sealed check.)
		ev := EvShutdown{Reason: reason}
		if resumeSessionID != "" {
			ev.ResumeSessionID = resumeSessionID
		}
		h.tr.sendSync(h.sessionID, ev)
		// sendSync's own priority-frame delivery can itself fail (queue never had
		// room, or the writer never confirmed within its budget) — sendFailed is set
		// synchronously by fail()/failSend() either way, so it can be read right
		// here rather than racing the async onSendFail hook. A host:shutdown that
		// never actually reached the parent must not be reported as a clean exit.
		transportFailedToDeliver := h.tr.sendFailed.Load()
		h.tr.Close()

		// Bounded join: wait for the cancelled workers to actually unwind before
		// app.Shutdown closes the store/MCP under them. Bounded so a Send that
		// ignores cancellation cannot wedge shutdown (we then proceed and accept
		// the abandonment — the process is exiting anyway).
		turnJoinTimedOut := h.joinTurns(h.turnJoinTimeout)

		// DURABLE wake re-arm: any burst still in pendingWake dies with this
		// process — a queued burst that never started, or one a cancelled wake
		// worker just requeued (its requeue lands under turnMu before its
		// turnWG.Done, so the join above ordered it before this sweep). The
		// scheduler stamped those queue events notified when it handed them over,
		// so without nulling notifiedAt here the next run's notify pass would
		// never re-digest them and the wake would be silently lost forever.
		// Runs BEFORE app.Shutdown (the store must still be open); best-effort.
		h.turnMu.Lock()
		leftover := h.pendingWake
		h.pendingWake = nil
		// A wake that outlives the bounded join re-arms itself directly once this
		// flag is set (see reactWake's cancellation path).
		h.wakeSweepDone = true
		h.turnMu.Unlock()
		h.rearmWakeEvents(leftover)

		// Bounded WAIT (see appShutdownTimeout): App.Shutdown itself has no context
		// of its own to cancel, so a stuck subsystem still runs to completion in the
		// background, but teardown does not wait for it past the bound — the
		// process exit below is what actually releases the flock either way.
		appShutdownTimedOut := false
		appShutdownFailed := false
		if h.app != nil {
			done := make(chan struct{})
			// shutdownErr/shutdownPanicked are written by this goroutine and read
			// ONLY inside the <-done case below — never on the timeout branch, where
			// the goroutine may still be running and writing them. Go's channel-close
			// happens-before guarantee is what makes that read race-free; reading them
			// unconditionally after the select (even one only reached via a boolean
			// short-circuit) would not be.
			var shutdownErr error
			var shutdownPanicked bool
			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						shutdownPanicked = true
					}
				}()
				shutdownErr = h.app.Shutdown(context.Background())
			}()
			select {
			case <-done:
				appShutdownFailed = shutdownErr != nil || shutdownPanicked
			case <-time.After(h.appShutdownTimeout):
				h.tr.diag("host: app shutdown did not complete within " + h.appShutdownTimeout.String() +
					"; exiting anyway so the process (and its flock) releases")
				appShutdownTimedOut = true
			}
		}

		// Exit code is HONEST: a fatal reason, a shutdown frame that never reached
		// the parent, a subsystem that never confirmed it closed cleanly (timed out,
		// errored, or panicked doing so), or a turn that never unwound within its
		// own bound are each a reason a supervisor or a person reading the exit
		// status should be told something did not go cleanly — not folded into the
		// same 0 a normal exit reports. Flush stdout, then exit (deferred so the
		// final write lands first).
		code := 0
		if reason == ShutdownError || transportFailedToDeliver || appShutdownTimedOut || appShutdownFailed || turnJoinTimedOut {
			code = 1
		}
		h.exit(code)
	})
}

// joinTurns waits (bounded) for every live prompt/wake worker to unwind, and
// reports whether the bound was hit rather than every worker actually settling.
// The WaitGroup is raced-free against new Adds because teardown latched `closing`
// under turnMu before calling this (workers Add under the same mutex).
func (h *Host) joinTurns(timeout time.Duration) (timedOut bool) {
	done := make(chan struct{})
	go func() {
		h.turnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(timeout):
		h.tr.diag("host: teardown timed out waiting for an in-flight turn to unwind; proceeding")
		return true
	}
}

// onPanic is the boot-phase / steady-state fatal funnel (TS errorGuard). Before
// host:ready a panic is "bootstrap-error" → exit 1; after ready it is "uncaught"
// → teardown error. Only reports when sessionId is set.
func (h *Host) onPanic(r any) {
	msg := fmt.Sprintf("%v\n%s", r, debug.Stack())
	if h.guardActive || !h.ready {
		// reportSync, not report: this branch calls exit(1) immediately afterward,
		// with no teardown/sendSync in between to guarantee ordering the way the
		// steady-state path's shutdown-first sequencing does. report() only
		// ENQUEUES the frame — flushExit's Sync() flushes the OS-level file, not the
		// writer goroutine's queue, so the queued error could easily never reach the
		// parent before the process exits. reportSync waits (bounded) for actual
		// delivery, exactly like every other fatal pre-app path.
		h.reportSync("bootstrap-error", msg)
		h.exit(1)
		return
	}
	h.report("uncaught", msg)
	h.teardown(ShutdownError, "")
}

// flushExit flushes stdout then exits (Go analog of setImmediate(() =>
// process.exit())). os.Stdout is unbuffered, so a Sync suffices to push any OS
// buffer before the process dies.
func flushExit(code int) {
	_ = os.Stdout.Sync()
	os.Exit(code)
}

// dispatchReclaimedPrompt re-dispatches a prompt the engine ACCEPTED and then could not
// consume — one buffered as an injection into a turn that ended before folding it in.
//
// It is not `handlePrompt`, because the words are already the user's and already taken.
// The inbound gate in handleCommand REFUSES a prompt while a session-owning command
// holds the session, which is right for something typed a moment ago and still in the
// composer; refusing one of these would throw away a message the engine has already
// removed from the injection queue and told nobody about. So it is HELD instead, and the
// command's own unwind dispatches it.
//
// The window is real and narrow, and it is NOT "while busy is still set" — a bare
// `/backend` is refused outright then (handleSlashCommandAsync gates an exclusive
// command on h.busy). It is the sliver AFTER the finishing turn clears busy and BEFORE
// it has re-dispatched what it reclaimed: the loop is free, an exclusive command can be
// admitted, and the reclaimed prompt then arrives into a session somebody else owns.
func (h *Host) dispatchReclaimedPrompt(text string) { h.admitPrompt(text, true) }

// drainDeferredPrompts dispatches everything dispatchReclaimedPrompt held back. Called
// from the unwind of the command that was holding the session; nothing else will, since
// these prompts belong to no turn whose finally would come along for them.
//
// A prompt held when TEARDOWN runs is lost: handlePrompt refuses once `closing` is
// latched, and there is nowhere durable to put a prompt (the wake queue has
// RearmAttention; prose has no equivalent). That is the same fate a stranded injection
// already meets at shutdown — reclaimStrandedInjections is skipped while closing — so it
// is a known limit of the injection strand rather than something this lane introduced.
func (h *Host) drainDeferredPrompts() {
	h.turnMu.Lock()
	pending := h.deferredPrompts
	h.deferredPrompts = nil
	h.turnMu.Unlock()
	for _, text := range pending {
		// Back through the GUARD, not straight to a turn.
		//
		// No other EXCLUSIVE command can interpose from this caller: cmdBusy is still set
		// until after the worker's unwind, so nothing can take the session while this
		// loop runs. (Prompts and ordinary commands still can — they are simply not what
		// would hurt.) The guard is here for the OTHER strand-recovery callers, which
		// re-dispatch a reclaimed prompt from a finishing turn or wake with no such
		// protection, and because a re-check that is correct from every caller is worth
		// more than one that is correct only from the caller it was written for.
		// admitPrompt re-checks under the lock that claims busy, so a prompt lands or is
		// held again, never in between.
		h.dispatchReclaimedPrompt(text)
	}
}

// exclusiveHeld reports whether a command currently owns the session. Takes turnMu
// itself so callers on the loop read it the same way every other flag there is read.
func (h *Host) exclusiveHeld() bool {
	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	return h.cmdExclusive
}

// exclusiveGates reports whether prompts and commands are currently refused. They are
// released at different moments — see Host.cmdPromptsReleased.
func (h *Host) exclusiveGates() (promptsBlocked, commandsBlocked bool) {
	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	return h.cmdExclusive && !h.cmdPromptsReleased, h.cmdExclusive
}

// handleSlashCommand runs a slash line and posts its output.
//
// Not a turn: no turn:start/turn:end is emitted and the model is never consulted, so a
// command costs nothing and cannot be answered with prose about itself. A `/quit` is
// honoured by winding the session down exactly as a shutdown command would.
func (h *Host) handleSlashCommand(line string) {
	out := h.runAndPostCommand(line)
	if out.Quit {
		h.cancelTurn()
		h.teardown(ShutdownExit, "")
	}
}

// runAndPostCommand runs a slash line, posts its result, and re-reports MCP status.
//
// Split from handleSlashCommand so it is callable from a worker: teardown joins the
// worker group, so a goroutine inside that group must never be the one to call it.
func (h *Host) runAndPostCommand(line string) CommandOutcome {
	return h.runAndPostCommandCtx(h.runCtx, line)
}

// runAndPostCommandCtx is runAndPostCommand against a caller-supplied context, so a slow
// command can be cancelled without cancelling the whole run.
func (h *Host) runAndPostCommandCtx(ctx context.Context, line string) CommandOutcome {
	return h.runAndPostCommandWith(ctx, line, nil)
}

// runAndPostCommandWith runs a slash line, runs `beforePost` if given, and only then
// posts the result.
//
// The seam exists because the RESULT is what a host uses to decide the command is over.
// Daintree keeps its composer disabled from the moment a local question is answered
// until that frame arrives; posting it while this command still owned the session meant
// the composer came back live a beat early, and a prompt submitted in that beat was
// refused by the gate — accepted by the transport, cleared from the draft, and never
// seen by the model. The gate has to be down before the result says so.
func (h *Host) runAndPostCommandWith(
	ctx context.Context, line string, beforePost func(),
) CommandOutcome {
	var out CommandOutcome
	if runner, ok := h.app.(CommandProgressRunner); ok {
		// Progress arrives as ordinary info notices — the same channel a degraded MCP or
		// a repeating tool failure uses. A command that reports nothing posts nothing.
		out = runner.RunCommandWithProgress(ctx, line, func(stage string) {
			h.post(EvNotice{Level: "info", Message: stage})
		})
	} else {
		out = h.app.RunCommand(ctx, line)
	}
	if beforePost != nil {
		beforePost()
	}
	h.post(EvCommandResult{
		Command:             line,
		Text:                out.Text,
		Quit:                out.Quit,
		Unknown:             out.Unknown,
		ConversationCleared: out.ConversationCleared,
	})
	// A command may have reconnected (or lost) the control plane — /reconnect exists
	// precisely to change this — so re-report rather than leaving a stale status.
	h.postMcpStatus()
	return out
}

// releaseExclusive drops the session-ownership gate and hands back anything held behind
// it. Idempotent: the worker's own unwind calls it again.
func (h *Host) releaseExclusive() {
	h.turnMu.Lock()
	// PROMPTS only. Commands stay blocked until this command's own result has been
	// posted and its unwind clears cmdExclusive — see Host.cmdPromptsReleased.
	first := h.cmdExclusive && !h.cmdPromptsReleased
	h.cmdPromptsReleased = true
	h.turnMu.Unlock()
	if first {
		h.drainDeferredPrompts()
	}
}

// handleSlashCommandAsync runs a SLOW slash line on a worker goroutine.
//
// The command loop is single-threaded, and everything it does not service while blocked
// is a thing the user cannot do: interrupt a turn, answer an approval, hibernate, quit.
// That is a fair trade for a command that reads a table and returns. It is not a fair
// trade for `/login`, which opens a browser and then waits up to five minutes for a
// loopback callback — for those five minutes the panel would accept no input, show no
// progress, and be indistinguishable from a hung engine. Nor for a command that ASKS,
// where the answer arrives as a command on the very loop it would be blocking: inline,
// that is not a freeze, it is a deadlock.
//
// Registered in the same worker group as a turn, under the same `closing` check, so
// teardown's bounded join covers an abandoned command rather than racing it.
func (h *Host) handleSlashCommandAsync(line string) {
	runner, isRunner := h.app.(CommandProgressRunner)
	exclusive := isRunner && runner.IsExclusiveCommand(line)

	// Its own context, not h.runCtx: teardown and interrupt both need a handle on this
	// specific wait, and h.runCtx is only cancelled once the process is already going.
	ctx, cancel := context.WithCancel(h.runCtx)

	h.turnMu.Lock()
	if h.closing {
		h.turnMu.Unlock()
		cancel()
		h.report("not-ready", "Host is shutting down.")
		return
	}
	// A turn already ADMITTED owns the session even though Session.inFlight is not
	// claimed until its worker reaches Send. Without this, a bare `/backend` arriving in
	// that gap takes the reservation and the accepted prompt's Send is then refused —
	// the user's message swallowed by a command they typed afterwards.
	if h.busy && exclusive {
		h.turnMu.Unlock()
		cancel()
		h.report("command-busy",
			"A turn is running. Wait for it to finish before switching what the session is bound to.")
		return
	}
	if h.cmdBusy {
		h.turnMu.Unlock()
		cancel()
		// REJECTED, not queued. These commands change what the session is bound to —
		// the account, the endpoint — and running a second while the first is still
		// deciding means the two settle in whichever order they happen to finish rather
		// than the order they were typed: a /logout sent after a /login can land first
		// and leave the session signed in. Saying so is also the honest answer: the
		// first one is waiting on the user, and they are the only one who can finish it.
		h.report("command-busy",
			"Another command is still waiting on you. Finish or interrupt it before starting another.")
		return
	}
	h.cmdBusy = true
	// Set HERE, on the loop, before the goroutine below is scheduled. Setting it inside
	// the worker would leave a window in which the command has been dispatched and the
	// gate is not yet up, and a prompt arriving in that window is admitted, starts a
	// turn, and is then refused by a reservation it never saw coming.
	if exclusive {
		h.cmdExclusive = true
		h.cmdPromptsReleased = false
	}
	h.cmdCancel = cancel
	h.turnWG.Add(1)
	h.turnMu.Unlock()

	go func() {
		defer h.turnWG.Done()
		defer func() {
			// Idempotent with the release before the result was posted: that one is the
			// one that matters for ordering, this one covers the paths that never reach
			// it (a panic, a command that returned before the seam).
			h.releaseExclusive()
			h.turnMu.Lock()
			h.cmdBusy = false
			h.cmdExclusive = false
			h.cmdPromptsReleased = false
			h.cmdCancel = nil
			// A wake that arrived while this command held the session was DEFERRED
			// rather than attempted (see reactWake), so it is still queued and nothing
			// else will come along to drive it. This is that something.
			more := len(h.pendingWake) > 0 && !h.closing && !h.busy
			h.turnMu.Unlock()
			cancel()
			if more {
				go h.reactWake()
			}
			// A panic in a command worker is fatal-for-command, not fatal-for-host —
			// the same rule a turn worker follows.
			if r := recover(); r != nil {
				h.report("command-failed", fmt.Sprintf("command panicked: %v\n%s", r, debug.Stack()))
				// And it STILL reports a result. Every dispatched command reports
				// exactly once — the panic path is the one that used to report none,
				// and a host that holds UI open until a command's result (Daintree
				// disables its composer while a picker's command finishes applying an
				// answer) would then hold it open for the rest of the session, waiting
				// on a frame that can no longer arrive.
				h.post(EvCommandResult{
					Command: line,
					Text:    "That command failed and could not finish.",
				})
			}
		}()
		// The gate comes down BEFORE the result is published, because the result is what
		// tells the host the command is over. See runAndPostCommandWith.
		out := h.runAndPostCommandWith(ctx, line, h.releaseExclusive)
		if out.Quit {
			// Unreachable by construction: no command is both Slow and quitting, and
			// tearing down from inside the worker group would deadlock on its own join.
			// Reported rather than silently dropped, because the day that stops being
			// true the symptom would otherwise be a /quit that did nothing.
			h.report("command-failed",
				"a slow command asked to quit; that is not supported from a command worker")
		}
	}()
}

// postCost emits the session's cumulative spend. Best-effort: a missing ledger reports
// nothing rather than failing a turn over a display figure.
func (h *Host) postCost() {
	if h.app == nil || h.bridge == nil {
		return
	}
	total, complete := h.app.CostSnapshot()
	h.bridge.PostCost(total, complete)
}

// postMcpStatus reports the control plane's reachability. Best-effort.
func (h *Host) postMcpStatus() {
	if h.app == nil || h.bridge == nil {
		return
	}
	connected, toolCount, errMsg := h.app.McpStatus()
	h.bridge.post(EvMcpStatus{Connected: connected, ToolCount: toolCount, Error: errMsg})
}

// postOperations answers an operations request with the current deck.
func (h *Host) postOperations() {
	if h.app == nil || h.bridge == nil {
		return
	}
	h.bridge.post(EvOperations{Snapshot: h.app.Operations(h.runCtx)})
}

// postTimers answers a timers request with the scheduled-timer list.
func (h *Host) postTimers() {
	if h.app == nil || h.bridge == nil {
		return
	}
	rows, ok := h.app.Timers(h.runCtx)
	h.bridge.post(EvTimers{
		Timers:     rows,
		Outcomes:   h.app.TimerOutcomes(h.runCtx),
		TakenAt:    domain.NowMS(),
		ReadFailed: !ok,
	})
}

// cancelTimer retires one timer for the user and answers with the outcome.
//
// It runs on the command loop rather than a worker. The operation is two local
// SQLite writes with no network and no model call, so it settles in well under
// the time an inline command is allowed to hold the loop — and putting it on a
// worker would buy a race between a cancel and the snapshot that follows it for
// no latency anyone can perceive.
//
// It answers even when nothing could be done. An unknown id, a storage fault and
// a successful retire all produce exactly one timer:cancelled, because the host
// UI has a row in a pending state and every path has to be able to settle it.
func (h *Host) cancelTimer(timerID string) {
	if h.app == nil || h.bridge == nil {
		return
	}
	h.bridge.post(EvTimerCancelled{Outcome: h.app.CancelTimer(h.runCtx, timerID)})
}

// retractInjection takes back the most recently buffered follow-up (LIFO) and hands the
// text to the host, which puts it back in the composer for editing.
//
// This is Escape on an empty composer in the cockpit. It is only ever a window: the
// message is buffered rather than sent, and the running turn folds the buffer in at its
// next tool-iteration boundary. Once that has happened the model has seen it and there is
// nothing to reclaim — which is what `retracted: false` says, so the host can leave the
// draft alone instead of blanking it over a retract that did not happen.
func (h *Host) retractInjection() {
	text, ok := h.session.RetractPendingInjection()
	h.bridge.PostInterjectRetracted(ok, text)
}
