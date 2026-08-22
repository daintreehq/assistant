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

// handleCommand dispatches one validated command. It must NOT
// block the command loop on a running Send — prompt/wake run on worker goroutines
// while interrupt/approval:decide keep being serviced.
func (h *Host) handleCommand(cmd HostCommand) {
	if !h.ready || h.bridge == nil || h.app == nil {
		h.report("not-ready", "Host is still starting.")
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
		h.handleSlashCommand(cmd.CommandLine)
	case CmdQuestionAnswer:
		// Same shape as approval:decide — unblocks a parked dispatch, bridge-guarded.
		h.bridge.ResolveQuestion(cmd.QuestionID, cmd.Index)
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
func (h *Host) handlePrompt(text string) {
	// Mint the aborter + claim busy under one lock so a worker's finally can't race
	// the busy check. interrupt cancels turnCancel. The generation counter is the
	// identity guard: a stale finally must not null a newer turn's cancel (Go funcs
	// aren't comparable, so we tag each turn).
	ctx, cancel := context.WithCancel(h.runCtx)
	h.turnMu.Lock()
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
				h.handlePrompt(stranded)
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
		h.handlePrompt(stranded)
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
	if h.busy || h.closing || !h.ready || h.bridge == nil || h.app == nil {
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

	h.bridge.StartExchange()

	func() {
		defer func() {
			if r := recover(); r != nil {
				h.report("wake-failed", fmt.Sprintf("wake panicked: %v", r))
			}
		}()
		prompt := agent.BuildWakePrompt(events, already)
		// IsWake: autonomous watcher-wake turn (not user-typed) → the footer anchors on the
		// active workflow objective instead of echoing the verbose wake blob.
		reply, err := h.session.Send(ctx, prompt, agent.SendOptions{IsWake: true})
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
			if !h.wakeRetried {
				h.wakeRetried = true
				// Requeue for ONE retry (unshift — preserve order ahead of new events).
				h.pendingWake = append(append([]domain.QueueEvent{}, events...), h.pendingWake...)
			}
			h.turnMu.Unlock()
			return
		}
		h.turnMu.Lock()
		h.wakeRetried = false
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
		h.turnMu.Unlock()
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
		h.handlePrompt(stranded)
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
		// Reject every outstanding approval so a parked dispatch unblocks (declined).
		if h.bridge != nil {
			h.bridge.SettlePendingApprovals(DecisionRejected)
		}
		// host:shutdown BEFORE app.Shutdown() — reason reaches Daintree regardless.
		// Written SYNCHRONOUSLY (bypassing the queue) so the final frame can't be
		// lost in the writer-goroutine close race, then Close() stops the writer +
		// unblocks the reader. (Events a dying turn posts after Close are dropped —
		// host:shutdown stays the last frame on the wire.)
		ev := EvShutdown{Reason: reason}
		if resumeSessionID != "" {
			ev.ResumeSessionID = resumeSessionID
		}
		h.tr.sendSync(h.sessionID, ev)
		h.tr.Close()

		// Bounded join: wait for the cancelled workers to actually unwind before
		// app.Shutdown closes the store/MCP under them. Bounded so a Send that
		// ignores cancellation cannot wedge shutdown (we then proceed and accept
		// the abandonment — the process is exiting anyway).
		h.joinTurns(h.turnJoinTimeout)

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

		if h.app != nil {
			func() {
				defer func() { _ = recover() }()
				_ = h.app.Shutdown(context.Background())
			}()
		}
		// Flush stdout, then exit (deferred so the final write lands first).
		h.exit(0)
	})
}

// joinTurns waits (bounded) for every live prompt/wake worker to unwind. The
// WaitGroup is raced-free against new Adds because teardown latched `closing`
// under turnMu before calling this (workers Add under the same mutex).
func (h *Host) joinTurns(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		h.turnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		h.tr.diag("host: teardown timed out waiting for an in-flight turn to unwind; proceeding")
	}
}

// onPanic is the boot-phase / steady-state fatal funnel (TS errorGuard). Before
// host:ready a panic is "bootstrap-error" → exit 1; after ready it is "uncaught"
// → teardown error. Only reports when sessionId is set.
func (h *Host) onPanic(r any) {
	msg := fmt.Sprintf("%v\n%s", r, debug.Stack())
	if h.guardActive || !h.ready {
		h.report("bootstrap-error", msg)
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

// handleSlashCommand runs a slash line and posts its output.
//
// Not a turn: no turn:start/turn:end is emitted and the model is never consulted, so a
// command costs nothing and cannot be answered with prose about itself. A `/quit` is
// honoured by winding the session down exactly as a shutdown command would.
func (h *Host) handleSlashCommand(line string) {
	out := h.app.RunCommand(h.runCtx, line)
	h.post(EvCommandResult{
		Command: line,
		Text:    out.Text,
		Quit:    out.Quit,
		Unknown: out.Unknown,
	})
	// A command may have reconnected (or lost) the control plane — /reconnect exists
	// precisely to change this — so re-report rather than leaving a stale status.
	h.postMcpStatus()
	if out.Quit {
		h.cancelTurn()
		h.teardown(ShutdownExit, "")
	}
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
