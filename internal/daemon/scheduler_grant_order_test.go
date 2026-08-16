package daemon

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// grantOrderRegistry snapshots the firing timer's revoke count at the instant
// call_safe_tool dispatches, which is the only way to observe the ordering bug:
// both the revoke and the dispatch happen, and the fire looks fine afterwards.
type grantOrderRegistry struct {
	store             *fakeStore
	dispatched        bool
	revokedAtDispatch int
}

func (r *grantOrderRegistry) Dispatch(_ context.Context, _ domain.ToolActor, actorID, _, _ string) (domain.ToolResult, error) {
	r.dispatched = true
	r.store.mu.Lock()
	r.revokedAtDispatch = r.store.revoked[actorID]
	r.store.mu.Unlock()
	return domain.Ok("ran", nil), nil
}

// TestScheduler_TerminalGrantOutlivesItsOwnDispatch pins the ordering that makes a
// one-shot grant-backed timer possible at all. A one-shot timer is TERMINAL on its
// very first fire (rescheduleePatch: repeatDone = !repeats), so the terminal-claim
// grant revoke and the call_safe_tool dispatch happen in the same fire. Revoking
// first retired the grant before the call that needed it — ConsumeGrant only matches
// revokedAt IS NULL — so the dispatch came back CONFIRMATION_REQUIRED and the target
// tool never ran (against the real registry the user got a blocked inbox item offering
// to authorize an already-fired timer). The grant must be live when the payload runs.
func TestScheduler_TerminalGrantOutlivesItsOwnDispatch(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_oneshot", Title: "spawn at fire time", FireAt: 0, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{}}}`,
	}}
	reg := &grantOrderRegistry{store: store}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: newFakeQueue(), Registry: reg})

	s.Tick(context.Background(), 10)

	if !reg.dispatched {
		t.Fatal("a due call_safe_tool timer must dispatch its payload")
	}
	if reg.revokedAtDispatch != 0 {
		t.Errorf("the timer's grants were already revoked when its own payload dispatched "+
			"(revoke count %d) — a one-shot grant-backed timer can never spend its grant",
			reg.revokedAtDispatch)
	}
	// The revoke is deferred, not dropped: a terminal timer must not leave live
	// authority behind for an id that will never fire again. Exactly once — a
	// deferred revoke that also ran inline would still satisfy "at least one".
	if got := store.revoked["tmr_oneshot"]; got != 1 {
		t.Errorf("a TERMINAL fire must revoke the timer's grants exactly once after the payload runs, got %d", got)
	}
}

// TestScheduler_FinalRepeatGrantOutlivesItsDispatch covers the OTHER terminal shape: a
// bounded repeat reaching maxRuns is terminal on its last fire, so it hits the same
// revoke-then-dispatch ordering the one-shot case does. The pre-existing maxRuns
// coverage uses an enqueue payload, which never needs a grant and so never noticed.
func TestScheduler_FinalRepeatGrantOutlivesItsDispatch(t *testing.T) {
	every := int64(60_000)
	maxRuns := 2
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_lastrun", Title: "final run", FireAt: 0, Status: "scheduled",
		RepeatEveryMs: &every, MaxRuns: &maxRuns, RunCount: 1,
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{}}}`,
	}}
	reg := &grantOrderRegistry{store: store}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: newFakeQueue(), Registry: reg})

	s.Tick(context.Background(), 10)

	if !reg.dispatched {
		t.Fatal("the final run of a bounded repeat must still dispatch its payload")
	}
	if reg.revokedAtDispatch != 0 {
		t.Errorf("the final repeat's grants were revoked before its own payload dispatched (revoke count %d)",
			reg.revokedAtDispatch)
	}
	if got := store.revoked["tmr_lastrun"]; got != 1 {
		t.Errorf("the final run must revoke the timer's grants exactly once, got %d", got)
	}
}

// TestScheduler_PanickingPayloadStillRevokes pins the promise the defer comment makes:
// revocation happens while unwinding too. A handler that panics must not leave a
// terminal timer's authority live.
func TestScheduler_PanickingPayloadStillRevokes(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_panic", Title: "boom", FireAt: 0, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{}}}`,
	}}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: newFakeQueue(), Registry: panickingRegistry{}})

	s.Tick(context.Background(), 10)

	if got := store.revoked["tmr_panic"]; got != 1 {
		t.Errorf("a panicking payload must still revoke the terminal timer's grants exactly once, got %d", got)
	}
}

// panickingRegistry blows up inside Dispatch, exercising the scheduler's recover path.
type panickingRegistry struct{}

func (panickingRegistry) Dispatch(_ context.Context, _ domain.ToolActor, _, _, _ string) (domain.ToolResult, error) {
	panic("payload exploded")
}

// TestScheduler_RepeatingFireKeepsItsGrants is the control: a non-terminal repeat
// has no revoke at all, so the deferred revoke must not start firing on every tick.
func TestScheduler_RepeatingFireKeepsItsGrants(t *testing.T) {
	every := int64(60_000)
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_repeat", Title: "recurring", FireAt: 0, Status: "scheduled",
		RepeatEveryMs: &every,
		PayloadType:   "call_safe_tool",
		PayloadJson:   `{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{}}}`,
	}}
	reg := &grantOrderRegistry{store: store}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: newFakeQueue(), Registry: reg})

	s.Tick(context.Background(), 10)

	if !reg.dispatched {
		t.Fatal("a due repeating call_safe_tool timer must dispatch its payload")
	}
	if store.revoked["tmr_repeat"] != 0 {
		t.Errorf("a repeat that is still scheduled must keep its grants for the next fire, got %d revokes",
			store.revoked["tmr_repeat"])
	}
}
