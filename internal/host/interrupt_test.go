package host

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Host interrupt contract (issue #141): the host `interrupt` command must actually
// abort the in-flight turn (cancel its context, reject pending approvals, then
// display-interrupt), not merely suppress display output.
//
// The full App can't be driven directly, so we mirror the exact per-turn wiring
// from loop.go (handlePrompt / handleInterrupt / finishPromptTurn — the busy flag,
// the generation identity guard, the turnCancel context) and drive it with a
// cooperative fake session that genuinely watches ctx.Done() and models a tool
// parked awaiting approval. We use the REAL Bridge for the approval bookkeeping so
// the deadlock path (settlePendingApprovals unblocking a parked confirm) is the
// production code. Keep in sync with internal/host/loop.go.

const cancelledReply = "Turn cancelled"

// cooperativeSession mirrors AgentSession's cooperative-cancel contract: it stays
// pending until finished OR ctx aborts (returning the sentinel — never an error).
// When needsApproval is set it first blocks on bridge.Confirm; a settled/declined
// approval falls through to the post-dispatch ctx check, exactly like dispatch→loop.
type cooperativeSession struct {
	bridge *Bridge

	mu     sync.Mutex
	finish chan struct{}
	calls  []sendCall
}

type sendCall struct {
	text          string
	ctx           context.Context
	needsApproval bool
}

func (s *cooperativeSession) send(ctx context.Context, text string, needsApproval bool) string {
	s.mu.Lock()
	s.calls = append(s.calls, sendCall{text: text, ctx: ctx, needsApproval: needsApproval})
	fin := make(chan struct{})
	s.finish = fin
	s.mu.Unlock()

	if ctx.Err() != nil {
		return cancelledReply
	}
	if needsApproval {
		// Park awaiting approval, like a mutating tool in dispatch.
		approved := s.bridge.Confirm(ctx, ConfirmRequest{ToolName: "delete.files", Summary: "delete files"})
		_ = approved
		// Approval settled (declined on interrupt) — the loop re-checks the signal.
		if ctx.Err() != nil {
			return cancelledReply
		}
	}
	select {
	case <-ctx.Done():
		return cancelledReply
	case <-fin:
		return "done"
	}
}

func (s *cooperativeSession) lastCall() sendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

func (s *cooperativeSession) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *cooperativeSession) finishCurrent() {
	s.mu.Lock()
	fin := s.finish
	s.finish = nil
	s.mu.Unlock()
	if fin != nil {
		close(fin)
	}
}

// harness mirrors the prompt/interrupt wiring in loop.go. busy + turnCancel + the
// generation counter live here exactly as on Host.
type harness struct {
	parent  context.Context
	bridge  *Bridge
	session *cooperativeSession

	mu         sync.Mutex
	busy       bool
	turnCancel context.CancelFunc
	turnGen    uint64
}

type promptHandle struct {
	rejected bool
	done     chan struct{}
}

func newHarness() *harness {
	b := NewBridge(BridgeOptions{SessionID: "s", Post: func(HostEvent) {}, ApprovalTimeoutMs: 0})
	h := &harness{parent: context.Background(), bridge: b}
	h.session = &cooperativeSession{bridge: b}
	return h
}

func (h *harness) prompt(text string, needsApproval bool) promptHandle {
	ctx, cancel := context.WithCancel(h.parent)
	h.mu.Lock()
	if h.busy {
		h.mu.Unlock()
		cancel()
		return promptHandle{rejected: true, done: closedChan()}
	}
	h.busy = true
	h.turnGen++
	gen := h.turnGen
	h.turnCancel = cancel
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer h.finishPromptTurn(gen)
		_ = h.session.send(ctx, text, needsApproval)
	}()
	return promptHandle{rejected: false, done: done}
}

// finishPromptTurn mirrors loop.go: identity guard clears turnCancel only when this
// turn's generation is still active, then clears busy.
func (h *harness) finishPromptTurn(gen uint64) {
	h.bridge.SettleTurn(OutcomeAnswered)
	h.mu.Lock()
	if h.turnGen == gen {
		h.turnCancel = nil
	}
	h.busy = false
	h.mu.Unlock()
}

// interrupt mirrors handleInterrupt: cancel the turn, reject pending approvals, then
// the display-side bridge interrupt.
func (h *harness) interrupt() {
	h.mu.Lock()
	cancel := h.turnCancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.bridge.SettlePendingApprovals(DecisionRejected)
	h.bridge.Interrupt()
}

// terminate mirrors loop.go's CmdShutdown/CmdHibernate arms (Fix 7): a
// command-driven terminal command cancels the in-flight turn AND rejects pending
// approvals before teardown, exactly like parent-exit — not a bare teardown. Keep
// in sync with internal/host/loop.go handleCommand + teardown.
func (h *harness) terminate() {
	h.mu.Lock()
	cancel := h.turnCancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// teardown's drain step.
	h.bridge.SettlePendingApprovals(DecisionRejected)
}

func (h *harness) isBusy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.busy
}

func (h *harness) controllerNil() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turnCancel == nil
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not finish")
	}
}

// settle gives the worker goroutine a moment to reach its current await.
func settle() { time.Sleep(20 * time.Millisecond) }

func TestInterruptFreshPromptSignalNotAborted(t *testing.T) {
	h := newHarness()
	h.prompt("hello", false)
	settle()
	if !h.isBusy() {
		t.Fatal("prompt did not mark busy")
	}
	call := h.session.lastCall()
	if call.ctx.Err() != nil {
		t.Fatal("fresh prompt got an already-aborted context")
	}
}

func TestInterruptAbortsInFlightAndClearsBusy(t *testing.T) {
	h := newHarness()
	p := h.prompt("long task", false)
	settle()
	if !h.isBusy() {
		t.Fatal("not busy before interrupt")
	}
	h.interrupt()
	waitDone(t, p.done)
	if h.isBusy() {
		t.Fatal("busy not cleared after interrupt")
	}
	if !h.controllerNil() {
		t.Fatal("turnCancel not cleared after interrupt")
	}
}

// The #141 deadlock: an interrupt must unblock a turn parked awaiting tool approval.
func TestInterruptUnblocksApprovalDeadlock(t *testing.T) {
	h := newHarness()
	p := h.prompt("delete files", true)
	settle() // let send() reach the Confirm() await
	h.interrupt()
	waitDone(t, p.done)
	if h.isBusy() {
		t.Fatal("busy not cleared after approval-deadlock interrupt")
	}
	if !h.controllerNil() {
		t.Fatal("turnCancel not cleared")
	}
}

func TestInterruptAbortsContextBeforeReturn(t *testing.T) {
	h := newHarness()
	p := h.prompt("task", false)
	settle()
	ctx := h.session.lastCall().ctx
	h.interrupt()
	if ctx.Err() == nil {
		t.Fatal("interrupt did not cancel the turn context")
	}
	waitDone(t, p.done)
}

func TestSecondPromptAfterInterruptGetsFreshController(t *testing.T) {
	h := newHarness()
	first := h.prompt("first", false)
	settle()
	firstCtx := h.session.lastCall().ctx
	h.interrupt()
	waitDone(t, first.done)
	if firstCtx.Err() == nil {
		t.Fatal("first context should be aborted")
	}

	second := h.prompt("second", false)
	settle()
	if second.rejected {
		t.Fatal("second prompt rejected after interrupt")
	}
	secondCtx := h.session.lastCall().ctx
	if secondCtx == firstCtx || secondCtx.Err() != nil {
		t.Fatal("second prompt did not get a fresh, non-aborted context")
	}
	h.session.finishCurrent()
	waitDone(t, second.done)
	if h.isBusy() {
		t.Fatal("busy after second turn completed")
	}
}

func TestInterruptBeforePromptIsNoop(t *testing.T) {
	h := newHarness()
	if !h.controllerNil() {
		t.Fatal("controller should be nil before any prompt")
	}
	// Must not panic with no active turn.
	h.interrupt()
}

func TestNormalCompletionClearsControllerViaIdentityGuard(t *testing.T) {
	h := newHarness()
	p := h.prompt("task", false)
	settle()
	if h.controllerNil() {
		t.Fatal("controller nil during active turn")
	}
	h.session.finishCurrent()
	waitDone(t, p.done)
	if h.isBusy() || !h.controllerNil() {
		t.Fatal("normal completion did not clear busy/controller")
	}
}

// The identity guard keeps a next turn's controller alive after the prior finally.
func TestIdentityGuardKeepsNextTurnControllerAlive(t *testing.T) {
	h := newHarness()
	first := h.prompt("first", false)
	settle()
	h.interrupt()
	waitDone(t, first.done) // first turn's finally runs here
	second := h.prompt("second", false)
	settle()
	if h.controllerNil() {
		t.Fatal("next turn's controller nulled by the stale first finally")
	}
	if h.session.lastCall().ctx.Err() != nil {
		t.Fatal("second turn context aborted by stale finally")
	}
	h.session.finishCurrent()
	waitDone(t, second.done)
}

// Fix 7: a command-driven shutdown/hibernate cancels the in-flight turn (it must
// not go straight to teardown leaving the turn context live).
func TestTerminateCancelsActiveTurnContext(t *testing.T) {
	h := newHarness()
	p := h.prompt("long task", false)
	settle()
	ctx := h.session.lastCall().ctx
	if ctx.Err() != nil {
		t.Fatal("turn context aborted before terminate")
	}
	h.terminate()
	if ctx.Err() == nil {
		t.Fatal("shutdown/hibernate did not cancel the in-flight turn context")
	}
	waitDone(t, p.done)
}

// Fix 7: terminate must also unpark a turn blocked awaiting tool approval (the same
// deadlock class as interrupt) by rejecting pending approvals before teardown.
func TestTerminateUnblocksApprovalDeadlock(t *testing.T) {
	h := newHarness()
	p := h.prompt("delete files", true)
	settle() // let send() reach the Confirm() await
	h.terminate()
	waitDone(t, p.done)
	if h.isBusy() {
		t.Fatal("busy not cleared after terminate during an approval wait")
	}
}

func TestRejectsSecondPromptWhileBusy(t *testing.T) {
	h := newHarness()
	h.prompt("first", false)
	settle()
	second := h.prompt("second", false)
	if !second.rejected {
		t.Fatal("second prompt while busy not rejected")
	}
	if h.session.callCount() != 1 {
		t.Fatalf("rejected prompt still called send: %d calls", h.session.callCount())
	}
}
