package daemon

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// Ports the scheduler edges not yet covered: a throwing timer is isolated so later
// timers still fire AND notify() still delivers; a due pr_state watcher routes to
// forge.getPR (not the terminal engine); a legacy run_check fires WITHOUT any model
// call; and the timer id is threaded as the scoped-grant actorId.

// panicTimerStore wraps fakeStore so UpdateTimer panics for one id, simulating the
// TS reschedule() throw that escapes fireTimer's inner try/catch.
type panicTimerStore struct {
	*fakeStore
	panicID string
}

func (s *panicTimerStore) UpdateTimer(id string, patch map[string]any) error {
	if id == s.panicID {
		panic("simulated sqlite failure")
	}
	return s.fakeStore.UpdateTimer(id, patch)
}

func TestScheduler_ThrowingTimerIsolatedLaterFiresAndNotifies(t *testing.T) {
	inner := newFakeStore()
	// `bad` is due earlier (smaller fireAt) so it fires first; `good` must still run.
	inner.timers = []domain.TimerRecord{
		{ID: "bad", Title: "boom", FireAt: 100, Status: "scheduled",
			PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"boom"}`},
		{ID: "good", Title: "survivor", FireAt: 200, Status: "scheduled",
			PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"survived"}`},
	}
	// Seed the digest so notify() has something to deliver after the tick publishes.
	store := &panicTimerStore{fakeStore: inner, panicID: "bad"}
	queue := newFakeQueue()
	// The "survived" event is published during the tick; mirror it into the digest
	// so notify() delivers it (the fake queue's Publish and Digest are separate).
	queue.digest = []domain.QueueEvent{{ID: "evt_good", Severity: domain.SeverityAttention, Summary: "survived"}}

	var delivered int32
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}})
	s.SetOnAttention(func(events []domain.QueueEvent) {
		for _, e := range events {
			if e.Summary == "survived" {
				atomic.AddInt32(&delivered, 1)
			}
		}
	})

	// The whole tick must complete without panicking out.
	s.Tick(context.Background(), 1000)

	// The survivor still published its enqueue event despite the earlier throw.
	survived := false
	for _, p := range queue.published {
		if p.Summary == "survived" {
			survived = true
		}
	}
	if !survived {
		t.Error("the later timer must still fire after an earlier one throws")
	}
	if atomic.LoadInt32(&delivered) != 1 {
		t.Error("notify() must still deliver the surviving timer's attention event")
	}
}

func TestScheduler_RunCheckConsultsNoModel(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_rc", Title: "legacy check", FireAt: 0, Status: "scheduled",
		PayloadType: "run_check", PayloadJson: `{"type":"run_check","checkPrompt":"is the build done?"}`,
	}}
	queue := newFakeQueue()
	// A model whose Classify/Judge panic if reached — proves the grounded-reminder
	// fallback consults NO model path.
	model := &progModel{classErr: errModelMustNotRun, judgeFn: func(string, string) domain.ModelJudgeAnswer {
		panic("run_check must not call the model")
	}}
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, newProgMCP(map[string]termCfg{}), model)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), 10)

	if len(queue.published) != 1 {
		t.Fatalf("run_check should publish exactly one reminder, got %d", len(queue.published))
	}
	p := queue.published[0]
	if p.Severity != domain.SeverityAttention {
		t.Errorf("run_check reminder must publish at attention, got %s", p.Severity)
	}
	if !containsStr(p.Summary, "is the build done?") || !containsStr(p.Summary, "deprecated") {
		t.Errorf("run_check summary must carry the prompt + deprecation note, got %q", p.Summary)
	}
}

func TestScheduler_RunCheckTypelessDispatchesOnColumn(t *testing.T) {
	// payloadType column says run_check but the JSON blob omits `type` — dispatch on
	// the column, not payload.type, so it fires rather than silently no-op.
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_tl", Title: "typeless", FireAt: 0, Status: "scheduled",
		PayloadType: "run_check", PayloadJson: `{"checkPrompt":"did it deploy?"}`,
	}}
	queue := newFakeQueue()
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}})
	s.Tick(context.Background(), 10)

	if len(queue.published) != 1 || !containsStr(queue.published[0].Summary, "did it deploy?") {
		t.Fatalf("typeless run_check must dispatch on the DB column, got %+v", queue.published)
	}
	if store.timerPatches["tmr_tl"]["status"] != "fired" {
		t.Error("typeless run_check should advance to fired")
	}
}

func TestScheduler_PrStateWatcherRoutesToForge(t *testing.T) {
	store := newFakeStore()
	mcp := newProgMCP(map[string]termCfg{})
	mcp.pulse = nil
	// forge.getPR reports the PR merged.
	mcp.list = nil
	forge := &forgeRoutingMCP{state: "merged"}
	store.watchers = []domain.WatcherRecord{prWatcher("wch_pr", PrWatcherOptions{PrNumber: 5, LastState: "open"})}
	queue := newFakeQueue()
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, forge, nil)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), 10)

	if !forge.calledForge {
		t.Error("a pr_state watcher must route to forge.getPR, not the terminal engine")
	}
	if forge.calledTerminal {
		t.Error("a pr_state watcher must NEVER touch terminal.getStatus")
	}
	if store.watchPatches["wch_pr"]["status"] != "condition_met" {
		t.Errorf("the merged PR should reach condition_met, got %v", store.watchPatches["wch_pr"]["status"])
	}
}

func TestScheduler_TimerIdThreadedAsGrantActor(t *testing.T) {
	// The scheduler must thread the timer id as the scoped-grant actorId so a
	// per-timer grant can authorize an otherwise-denied call_safe_tool.
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_grant", Title: "granted", FireAt: 0, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"test.project","args":{}}}`,
	}}
	queue := newFakeQueue()
	reg := &actorRecordingRegistry{result: domain.Ok("ran", nil)}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg})
	s.Tick(context.Background(), 10)

	if reg.lastActor != domain.ActorTimer {
		t.Errorf("call_safe_tool must dispatch as the timer actor, got %s", reg.lastActor)
	}
	// Finding 2: the firing timer's id must be threaded as the actorId so the
	// downstream grant lookup (keyed on the actor id) can match a scoped grant.
	if reg.lastActorID != "tmr_grant" {
		t.Errorf("timer id must be threaded as the grant actorId, got %q", reg.lastActorID)
	}
}

// --- supporting fakes --------------------------------------------------------

// forgeRoutingMCP records whether the PR engine routed to forge.getPR (and never to
// the terminal engine), returning a merged PR.
type forgeRoutingMCP struct {
	state          string
	calledForge    bool
	calledTerminal bool
}

func (m *forgeRoutingMCP) Connected() bool                               { return true }
func (m *forgeRoutingMCP) SupportsSubscribe() bool                       { return false }
func (m *forgeRoutingMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (m *forgeRoutingMCP) Unsubscribe(_ context.Context, _ string) error { return nil }
func (m *forgeRoutingMCP) CallRead(_ context.Context, name string, _ map[string]any) (MCPResult, error) {
	switch name {
	case "forge.getPR":
		m.calledForge = true
		return MCPResult{StructuredContent: map[string]any{"state": m.state}}, nil
	case "terminal.getStatus":
		m.calledTerminal = true
	}
	return MCPResult{IsError: true}, nil
}

// actorRecordingRegistry records the actor passed to Dispatch.
type actorRecordingRegistry struct {
	result      domain.ToolResult
	lastActor   domain.ToolActor
	lastActorID string
}

func (r *actorRecordingRegistry) Dispatch(_ context.Context, actor domain.ToolActor, actorID, _, _ string) (domain.ToolResult, error) {
	r.lastActor = actor
	r.lastActorID = actorID
	return r.result, nil
}
