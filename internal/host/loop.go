package host

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

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
	case CmdInterrupt:
		h.handleInterrupt()
	case CmdHibernate:
		// Cancel any in-flight turn + reject pending approvals BEFORE teardown — a
		// command-driven hibernate must unpark a blocked dispatch exactly like the
		// parent-exit path (teardown also drains approvals, but the turn's context is
		// only cancelled here). The resume handle IS the sessionId (conversation
		// persists keyed by it).
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
// next tool-iteration boundary ("between tasks"), matching the cockpit composer — rather
// than rejected. The send runs on a worker goroutine so the command loop keeps servicing
// interrupt/decide.
func (h *Host) handlePrompt(text string) {
	// Mint the aborter + claim busy under one lock so a worker's finally can't race
	// the busy check. interrupt cancels turnCancel. The generation counter is the
	// identity guard: a stale finally must not null a newer turn's cancel (Go funcs
	// aren't comparable, so we tag each turn).
	ctx, cancel := context.WithCancel(h.runCtx)
	h.turnMu.Lock()
	if h.busy {
		h.turnMu.Unlock()
		cancel()
		// Fold it into the in-flight turn instead of rejecting. Daintree's parent already
		// holds the text it sent, so there's no echo — just a status so it knows the prompt
		// joined the running turn rather than starting a new one.
		h.app.Session().InjectPrompt(text)
		h.report("prompt-folded",
			"A turn is already running; this message was folded into it and will be picked up between tasks.")
		return
	}
	h.busy = true
	h.turnGen++
	gen := h.turnGen
	h.turnCancel = cancel
	h.turnMu.Unlock()

	h.bridge.StartExchange()

	go func() {
		defer func() {
			// A panic in a turn worker is fatal-for-turn, not fatal-for-host.
			if r := recover(); r != nil {
				h.report("turn-failed", fmt.Sprintf("turn panicked: %v\n%s", r, debug.Stack()))
			}
			h.finishPromptTurn(gen)
		}()
		if _, err := h.app.Session().Send(ctx, text, agent.SendOptions{}); err != nil {
			h.report("turn-failed", fmt.Sprintf("send failed: %v", err))
		}
		// The one-time session-ended-watchers note is owned by the Session now (it surfaces
		// once in the uncached footer on the first turn), so the host no longer consumes it.
	}()
}

// finishPromptTurn is the prompt finally. Identity guard: only clear turnCancel
// when this turn's generation is still the active one (a later prompt bumped the
// generation, so a stale finally leaves the newer turn's cancel intact). Then
// settle any dangling assistant turn, clear busy, and drain deferred wakes.
func (h *Host) finishPromptTurn(gen uint64) {
	h.bridge.SettleTurn(OutcomeAnswered)

	h.turnMu.Lock()
	if h.turnGen == gen {
		h.turnCancel = nil
	}
	h.busy = false
	more := len(h.pendingWake) > 0
	h.turnMu.Unlock()
	if more {
		go h.reactWake()
	}
}

// handleInterrupt is the three coordinated actions. Order
// matters: abort the turn signal, reject pending approvals (an awaited confirm
// can't be unparked by the signal alone), then the display-side bridge interrupt.
func (h *Host) handleInterrupt() {
	h.cancelTurn()
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

// reactWake runs an autonomous wake turn — full capability, like a user turn, so the
// reactor can act (relay between agents, resolve inbox items), not just report. Gated
// by busy/ready; drains pendingWake; chains itself if more remain. Wake turns do NOT
// register turnCancel (unabortable by design).
func (h *Host) reactWake() {
	h.turnMu.Lock()
	if h.busy || !h.ready || h.bridge == nil || h.app == nil {
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
	// Snapshot the cross-burst summarized set for the prompt builder.
	already := make(map[string]struct{}, len(h.summarizedTerminals))
	for id := range h.summarizedTerminals {
		already[id] = struct{}{}
	}
	h.turnMu.Unlock()

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
		reply, err := h.app.Session().Send(h.runCtx, prompt, agent.SendOptions{IsWake: true})
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
		// events only (mirrors the cockpit): an async-tool completion carries a
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

	h.bridge.SettleTurn(OutcomeAnswered)

	h.turnMu.Lock()
	h.busy = false
	more := len(h.pendingWake) > 0
	h.turnMu.Unlock()
	if more {
		go h.reactWake()
	}
}

// teardown emits host:shutdown FIRST (so Daintree sees the reason even if the App
// shutdown hangs), drains pending approvals, shuts the App, flushes, and exits.
// Idempotent via teardownOnce.
func (h *Host) teardown(reason HostShutdownReason, resumeSessionID string) {
	h.teardownOnce.Do(func() {
		// Reject every outstanding approval so a parked dispatch unblocks (declined).
		if h.bridge != nil {
			h.bridge.SettlePendingApprovals(DecisionRejected)
		}
		// host:shutdown BEFORE app.Shutdown() — reason reaches Daintree regardless.
		// Written SYNCHRONOUSLY (bypassing the queue) so the final frame can't be
		// lost in the writer-goroutine close race, then Close() stops the writer +
		// unblocks the reader.
		ev := EvShutdown{Reason: reason}
		if resumeSessionID != "" {
			ev.ResumeSessionID = resumeSessionID
		}
		h.tr.sendSync(h.sessionID, ev)
		h.tr.Close()

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
