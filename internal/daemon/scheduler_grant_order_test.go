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
// revokedAt IS NULL — so the dispatch came back CONFIRMATION_REQUIRED and the timer
// silently did nothing. The grant must still be live when the payload dispatches.
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
	// authority behind for an id that will never fire again.
	if store.revoked["tmr_oneshot"] == 0 {
		t.Error("a TERMINAL fire must still revoke the timer's grants once the payload has run")
	}
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
