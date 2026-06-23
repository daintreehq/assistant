package daemon

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func newScheduler(store Store, queue Queue, reg Registry, ctxFn func(context.Context, domain.ToolActor, string) *CheckContext) *Scheduler {
	return NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg, CtxFor: ctxFn})
}

func TestScheduler_FireEnqueueTimer_AttentionDedupeStable(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_1", Title: "Reminder", FireAt: 100, Status: "scheduled",
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"ping"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 200)

	if len(queue.published) != 1 {
		t.Fatalf("want 1 published, got %d", len(queue.published))
	}
	p := queue.published[0]
	if p.Severity != domain.SeverityAttention || p.Summary != "ping" {
		t.Errorf("enqueue should publish attention 'ping', got sev=%s summary=%q", p.Severity, p.Summary)
	}
	if p.DedupeKey != "timer:tmr_1" {
		t.Errorf("dedupeKey = %q, want timer:tmr_1", p.DedupeKey)
	}
	// One-shot timer ends as "fired".
	if store.timerPatches["tmr_1"]["status"] != "fired" {
		t.Errorf("one-shot timer should end fired, got %v", store.timerPatches["tmr_1"]["status"])
	}
	if store.revoked["tmr_1"] != 1 {
		t.Error("a finished timer must revoke its grants")
	}
}

func TestScheduler_RunCheckLegacyReminder(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_2", Title: "Old", FireAt: 0, Status: "scheduled",
		PayloadType: "run_check", PayloadJson: `{"type":"run_check","checkPrompt":"is it done?"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 10)
	if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityAttention {
		t.Fatalf("run_check should fire one attention reminder")
	}
	if got := queue.published[0].Summary; got == "is it done?" {
		t.Errorf("run_check summary must be wrapped in the deprecation reminder, got %q", got)
	}
}

func TestScheduler_RepeatCatchUpSingleFire(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_3", Title: "Repeat", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// Long sleep: now is far past several missed deadlines. DueTimers returns the
	// row once; reschedule moves fireAt from NOW → a single catch-up fire.
	now := int64(10_000_000)
	s.Tick(context.Background(), now)
	if len(queue.published) != 1 {
		t.Fatalf("a long sleep must produce exactly ONE catch-up fire, got %d", len(queue.published))
	}
	patch := store.timerPatches["tmr_3"]
	if patch["status"] != "scheduled" {
		t.Errorf("repeat should stay scheduled, got %v", patch["status"])
	}
	if patch["fireAt"].(int64) != now+every {
		t.Errorf("next fire must be relative to NOW (%d), got %v", now+every, patch["fireAt"])
	}
	// (now-FireAt)/every = (10_000_000-1000)/60_000 = 166 collapsed occurrences.
	// The single fire must surface that backlog, not swallow it silently.
	if got := queue.published[0].Summary; !strings.Contains(got, "166 occurrences were skipped") {
		t.Errorf("catch-up summary must report the 166 collapsed occurrences, got %q", got)
	}
	if got := queue.published[0].Summary; !strings.Contains(got, "fired once now") {
		t.Errorf("catch-up summary must note the single fire, got %q", got)
	}
}

// TestScheduler_RepeatOnTimeNoClause: a repeat that fires within one interval of
// its deadline skipped nothing, so the summary must NOT carry the catch-up clause.
func TestScheduler_RepeatOnTimeNoClause(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_ontime", Title: "Repeat", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"ping"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// elapsed = 60_999-1000 = 59_999 < every → missed = 0.
	s.Tick(context.Background(), 60_999)
	if len(queue.published) != 1 {
		t.Fatalf("want 1 published, got %d", len(queue.published))
	}
	if got := queue.published[0].Summary; got != "ping" {
		t.Errorf("on-time repeat must publish the plain summary with no catch-up clause, got %q", got)
	}
}

// TestScheduler_RepeatCatchUpSingular: exactly one skipped occurrence uses the
// singular phrasing ("1 occurrence", not "occurrences").
func TestScheduler_RepeatCatchUpSingular(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_one", Title: "Repeat", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"ping"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// elapsed = 61_000-1000 = 60_000 = exactly one interval → missed = 1 (boundary).
	s.Tick(context.Background(), 61_000)
	got := queue.published[0].Summary
	if !strings.Contains(got, "1 occurrence was skipped") {
		t.Errorf("a single skipped occurrence must use singular phrasing, got %q", got)
	}
	if strings.Contains(got, "occurrences") {
		t.Errorf("singular catch-up must not pluralize, got %q", got)
	}
}

// TestScheduler_RepeatCatchUp_TerminalMaxRunsNoClause: a maxRuns-capped final fire
// has no backlog (the timer was never going to run again), so even when overdue it
// must NOT claim occurrences were skipped.
func TestScheduler_RepeatCatchUp_TerminalMaxRunsNoClause(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	maxRuns := 3
	store.timers = []domain.TimerRecord{{
		ID: "tmr_term_mr", Title: "Capped", FireAt: 0, Status: "scheduled",
		RepeatEveryMs: &every, MaxRuns: &maxRuns, RunCount: 2, // this fire makes it 3 → done
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"last"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// now is 10 intervals past FireAt: a naive floor(elapsed/every) would say "10".
	s.Tick(context.Background(), 600_000)
	if store.timerPatches["tmr_term_mr"]["status"] != "done" {
		t.Fatalf("maxRuns terminal fire should mark done, got %v", store.timerPatches["tmr_term_mr"]["status"])
	}
	if got := queue.published[0].Summary; strings.Contains(got, "skipped") {
		t.Errorf("a terminal (maxRuns) fire must not report skipped occurrences, got %q", got)
	}
}

// TestScheduler_RepeatCatchUp_TerminalRepeatUntilNoClause: a repeatUntil-capped
// final fire likewise has no backlog past the deadline — no catch-up clause.
func TestScheduler_RepeatCatchUp_TerminalRepeatUntilNoClause(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	until := int64(5000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_term_until", Title: "Until", FireAt: 4500, Status: "scheduled",
		RepeatEveryMs: &every, RepeatUntil: &until, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"last"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// now far past FireAt; next fire (now+every) is well past until → terminal.
	s.Tick(context.Background(), 300_000)
	if store.timerPatches["tmr_term_until"]["status"] != "done" {
		t.Fatalf("repeatUntil terminal fire should mark done, got %v", store.timerPatches["tmr_term_until"]["status"])
	}
	if got := queue.published[0].Summary; strings.Contains(got, "skipped") {
		t.Errorf("a terminal (repeatUntil) fire must not report skipped occurrences, got %q", got)
	}
}

// TestScheduler_RepeatCatchUp_SubTickNoFalseClause: a timer whose interval is below
// the scheduler tick collapses occurrences on every normal fire, so the gap is NOT
// evidence of downtime — no catch-up clause.
func TestScheduler_RepeatCatchUp_SubTickNoFalseClause(t *testing.T) {
	store := newFakeStore()
	every := int64(1000) // < SchedulerTickMS (3000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_subtick", Title: "Fast", FireAt: 0, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"tick"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// elapsed = 3000 = 3 intervals, but that's just normal tick granularity.
	s.Tick(context.Background(), 3000)
	if got := queue.published[0].Summary; got != "tick" {
		t.Errorf("a sub-tick repeat must not report skipped occurrences, got %q", got)
	}
}

// TestScheduler_RepeatCatchUp_CallSafeToolErrorNoClause: a failed call_safe_tool
// publishes an error-severity event; the informational catch-up clause must not
// dilute it.
func TestScheduler_RepeatCatchUp_CallSafeToolErrorNoClause(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_cst_err", Title: "Tool", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.do","args":{}}}`,
	}}
	queue := newFakeQueue()
	reg := &fakeRegistry{result: domain.Fail("SOME_ERR", "tool blew up")}
	s := newScheduler(store, queue, reg, nil)
	s.Tick(context.Background(), 10_000_000)
	p := queue.published[0]
	if p.Severity != domain.SeverityError {
		t.Fatalf("a failed call_safe_tool must publish an error event, got sev=%s", p.Severity)
	}
	if strings.Contains(p.Summary, "skipped") {
		t.Errorf("an error-severity fire must not carry the catch-up clause, got %q", p.Summary)
	}
}

// TestScheduler_RepeatCatchUp_RunCheck: the catch-up clause rides along the
// deprecated run_check reminder too, appended after the deprecation prefix.
func TestScheduler_RepeatCatchUp_RunCheck(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_rc", Title: "Old", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "run_check", PayloadJson: `{"type":"run_check","checkPrompt":"is it done?"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 10_000_000)
	got := queue.published[0].Summary
	if !strings.Contains(got, "run_check is deprecated") {
		t.Errorf("run_check must keep its deprecation prefix, got %q", got)
	}
	if !strings.Contains(got, "166 occurrences were skipped") {
		t.Errorf("run_check catch-up must report collapsed occurrences, got %q", got)
	}
}

// TestScheduler_RepeatCatchUp_CallSafeTool: a successful call_safe_tool fire
// carries the clause on its result summary.
func TestScheduler_RepeatCatchUp_CallSafeTool(t *testing.T) {
	store := newFakeStore()
	every := int64(60_000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_cst", Title: "Tool", FireAt: 1000, Status: "scheduled",
		RepeatEveryMs: &every, RunCount: 0,
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.do","args":{}}}`,
	}}
	queue := newFakeQueue()
	reg := &fakeRegistry{result: domain.Ok("did the thing", nil)}
	s := newScheduler(store, queue, reg, nil)
	s.Tick(context.Background(), 10_000_000)
	got := queue.published[0].Summary
	if !strings.Contains(got, "did the thing") {
		t.Errorf("call_safe_tool must keep its result summary, got %q", got)
	}
	if !strings.Contains(got, "166 occurrences were skipped") {
		t.Errorf("call_safe_tool catch-up must report collapsed occurrences, got %q", got)
	}
}

func TestScheduler_RepeatMaxRunsDone(t *testing.T) {
	store := newFakeStore()
	every := int64(1000)
	maxRuns := 3
	store.timers = []domain.TimerRecord{{
		ID: "tmr_4", Title: "Capped", FireAt: 0, Status: "scheduled",
		RepeatEveryMs: &every, MaxRuns: &maxRuns, RunCount: 2, // this fire makes it 3 → done
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 10)
	if store.timerPatches["tmr_4"]["status"] != "done" {
		t.Errorf("reaching maxRuns should mark a repeat done, got %v", store.timerPatches["tmr_4"]["status"])
	}
}

func TestScheduler_RepeatUntilNoExtraFire(t *testing.T) {
	store := newFakeStore()
	every := int64(1000)
	until := int64(5000)
	store.timers = []domain.TimerRecord{{
		ID: "tmr_5", Title: "Until", FireAt: 4500, Status: "scheduled",
		RepeatEveryMs: &every, RepeatUntil: &until, RunCount: 0,
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	// now=4500; next would be 5500 > until 5000 → stop (no extra fire).
	s.Tick(context.Background(), 4500)
	if store.timerPatches["tmr_5"]["status"] != "done" {
		t.Errorf("repeat-until should stop when next fire lands past deadline, got %v", store.timerPatches["tmr_5"]["status"])
	}
}

func TestScheduler_CorruptTimerDisabled(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_6", Title: "Bad", FireAt: 0, Status: "scheduled",
		PayloadType: "enqueue", PayloadJson: `{not json`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 10)
	if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityError {
		t.Fatalf("corrupt timer should publish an error event")
	}
	if store.timerPatches["tmr_6"]["status"] != "fired" {
		t.Error("corrupt timer should be disabled (fired)")
	}
	if store.revoked["tmr_6"] != 1 {
		t.Error("disabled timer should revoke grants")
	}
}

func TestScheduler_CallSafeToolSkipsConfirmationRequired(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_7", Title: "Tool", FireAt: 0, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.do","args":{}}}`,
	}}
	queue := newFakeQueue()
	reg := &fakeRegistry{result: domain.Fail("CONFIRMATION_REQUIRED", "needs confirm")}
	s := newScheduler(store, queue, reg, nil)
	s.Tick(context.Background(), 10)
	if reg.calls != 1 {
		t.Error("call_safe_tool should dispatch")
	}
	if len(queue.published) != 0 {
		t.Errorf("CONFIRMATION_REQUIRED must not raise a timer error, got %d publishes", len(queue.published))
	}
}

func TestScheduler_NotifyOnceAndMarkRegardlessOfDelivery(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	queue.digest = []domain.QueueEvent{
		{ID: "evt_1", Severity: domain.SeverityAttention},
		{ID: "evt_2", Severity: domain.SeverityUrgent},
	}
	var delivered int32
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.SetOnAttention(func(events []domain.QueueEvent) {
		atomic.AddInt32(&delivered, int32(len(events)))
		panic("delivery blew up") // must NOT prevent markNotified
	})
	s.Tick(context.Background(), 10)
	if atomic.LoadInt32(&delivered) != 2 {
		t.Errorf("callback should receive 2 events, got %d", delivered)
	}
	if len(queue.markedIDs) != 1 || len(queue.markedIDs[0]) != 2 {
		t.Fatalf("both events must be marked notified despite delivery panic")
	}
	// Second tick: events were dropped from the digest → no re-fire.
	s.Tick(context.Background(), 20)
	if atomic.LoadInt32(&delivered) != 2 {
		t.Errorf("events must be delivered exactly once, total delivered=%d", delivered)
	}
}

func TestScheduler_UnknownWatcherKindFailsClosed(t *testing.T) {
	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{{
		ID: "wch_x", Kind: "bogus", Status: "active", NextCheckAt: 0,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, func(ctx context.Context, a domain.ToolActor, id string) *CheckContext {
		return ctxFor(store, queue, newFakeMCP(), &fakeModel{})
	})
	s.Tick(context.Background(), 10)
	if store.watchPatches["wch_x"]["status"] != "error" {
		t.Errorf("unknown watcher kind must fail closed to error, got %v", store.watchPatches["wch_x"]["status"])
	}
}

// TestScheduler_NoOverlap verifies a long-running tick blocks a ticker-driven one
// and that Drain waits for the in-flight tick (the drain handle is never
// overwritten by a skipped tick).
func TestScheduler_NoOverlapAndDrain(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	var concurrent int32
	var maxConcurrent int32
	release := make(chan struct{})
	var once sync.Once

	// A blocking timer payload: the registry dispatch holds the tick until released.
	store.timers = []domain.TimerRecord{{
		ID: "tmr_block", Title: "Block", FireAt: 0, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.slow","args":{}}}`,
	}}
	reg := &blockingRegistry{enter: func() {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&concurrent, -1)
	}}

	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg, TickMS: 5})
	s.Start(context.Background())
	// Let the ticker fire many times while the first tick is blocked.
	time.Sleep(60 * time.Millisecond)
	once.Do(func() { close(release) })
	s.Stop()
	s.Drain()

	if atomic.LoadInt32(&maxConcurrent) > 1 {
		t.Errorf("ticks overlapped: maxConcurrent=%d", maxConcurrent)
	}
}

type blockingRegistry struct{ enter func() }

func (b *blockingRegistry) Dispatch(ctx context.Context, actor domain.ToolActor, actorID, name, argsJson string) (domain.ToolResult, error) {
	b.enter()
	return domain.Ok("done", nil), nil
}

// TestScheduler_StopRaceNoTickAfterDrain stresses the Stop/Drain window: a ticker
// event can be selected by the loop just as Stop() fires. Drain must not return
// until the loop has quiesced, and no tick may run a pass against the canceled ctx
// AFTER Drain() returns. We assert no DueTimers call lands post-Drain.
func TestScheduler_StopRaceNoTickAfterDrain(t *testing.T) {
	for i := 0; i < 50; i++ {
		store := newCountingStore()
		queue := newFakeQueue()
		s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, TickMS: 1})
		s.Start(context.Background())
		time.Sleep(3 * time.Millisecond) // let a few ticks run
		s.Stop()
		s.Drain()
		// Latch the count the instant Drain returns; any later tick is the bug.
		after := atomic.LoadInt32(&store.dueTimerCalls)
		time.Sleep(5 * time.Millisecond)
		if got := atomic.LoadInt32(&store.dueTimerCalls); got != after {
			t.Fatalf("a tick ran after Drain returned: %d → %d (iter %d)", after, got, i)
		}
	}
}

// TestScheduler_SlowItemDoesNotStarveOthersOrNotify proves one slow watcher/timer
// runs as an independent bounded job: the other due items still complete and
// notify() still delivers, all within roughly one item's time (not the sum).
func TestScheduler_SlowItemDoesNotStarveOthersOrNotify(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	queue.digest = []domain.QueueEvent{{ID: "evt_1", Severity: domain.SeverityAttention}}

	// One slow timer (blocks ~80ms) + two fast timers. If the pass serialized, the
	// fast ones + notify would wait behind the slow one; in parallel they don't.
	store.timers = []domain.TimerRecord{
		{ID: "slow", Title: "Slow", FireAt: 0, Status: "scheduled", PayloadType: "call_safe_tool",
			PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.slow","args":{}}}`},
		{ID: "fast1", Title: "F1", FireAt: 0, Status: "scheduled", PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"a"}`},
		{ID: "fast2", Title: "F2", FireAt: 0, Status: "scheduled", PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"b"}`},
	}
	reg := &blockingRegistry{enter: func() { time.Sleep(80 * time.Millisecond) }}

	var notified int32
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg})
	s.SetOnAttention(func(events []domain.QueueEvent) { atomic.AddInt32(&notified, int32(len(events))) })

	start := time.Now()
	s.Tick(context.Background(), 10)
	elapsed := time.Since(start)

	// Both fast enqueues published (slow one publishes nothing here).
	var enqueued int
	for _, p := range queue.published {
		if p.Summary == "a" || p.Summary == "b" {
			enqueued++
		}
	}
	if enqueued != 2 {
		t.Fatalf("both fast timers must publish despite a slow sibling, got %d", enqueued)
	}
	if atomic.LoadInt32(&notified) != 1 {
		t.Fatalf("notify must deliver at end of tick, got %d", notified)
	}
	// Parallel: total time ≈ the single slow item (~80ms), not 3×. Generous bound.
	if elapsed > 400*time.Millisecond {
		t.Fatalf("pass took %v — items did not run in parallel", elapsed)
	}
}

// TestScheduler_ConcurrentTickNoDoubleRun calls the public Tick concurrently and
// asserts the no-overlap guard (now in Tick, not just Start) lets exactly ONE pass
// run at a time.
func TestScheduler_ConcurrentTickNoDoubleRun(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	var concurrent, maxConcurrent int32
	release := make(chan struct{})

	store.timers = []domain.TimerRecord{{
		ID: "tmr_block", Title: "Block", FireAt: 0, Status: "scheduled", PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x.slow","args":{}}}`,
	}}
	reg := &blockingRegistry{enter: func() {
		n := atomic.AddInt32(&concurrent, 1)
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&concurrent, -1)
	}}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Tick(context.Background(), 10) }()
	}
	time.Sleep(40 * time.Millisecond) // let them all reach the guard
	close(release)
	wg.Wait()

	if atomic.LoadInt32(&maxConcurrent) > 1 {
		t.Fatalf("concurrent Tick calls double-ran: maxConcurrent=%d", maxConcurrent)
	}
}

// countingStore counts DueTimers calls (to detect a tick running after Drain).
type countingStore struct {
	*fakeStore
	dueTimerCalls int32
}

func newCountingStore() *countingStore { return &countingStore{fakeStore: newFakeStore()} }

func (c *countingStore) DueTimers(now int64) ([]domain.TimerRecord, error) {
	atomic.AddInt32(&c.dueTimerCalls, 1)
	return c.fakeStore.DueTimers(now)
}

// panicDueStore makes DueTimers panic — simulating a panic in the pass BODY (outside the
// per-item recovers), to prove the top-level tick recover keeps the daemon goroutine alive.
type panicDueStore struct{ *fakeStore }

func (panicDueStore) DueTimers(now int64) ([]domain.TimerRecord, error) {
	panic("boom in DueTimers")
}

// TestScheduler_TickPanicDoesNotKillDaemon: a panic in the pass body must be recovered so
// the daemon survives and later ticks still run — otherwise one bad tick silently kills ALL
// supervision for the session. A recovered panic also surfaces a supervision-error event.
func TestScheduler_TickPanicDoesNotKillDaemon(t *testing.T) {
	store := panicDueStore{newFakeStore()}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)

	// Must NOT propagate the panic out of Tick.
	s.Tick(context.Background(), 200)
	if len(queue.published) == 0 {
		t.Error("a recovered tick panic should surface a supervision-error event")
	}
	// A subsequent tick still runs (the goroutine wasn't killed) — also must not panic.
	s.Tick(context.Background(), 300)
}

// claimFailsStore simulates the main turn cancelling/editing a timer in the window between
// DueTimers (returns it as due) and the fire (the claim no longer matches → false).
type claimFailsStore struct{ *fakeStore }

func (claimFailsStore) ClaimDueTimer(string, int64, map[string]any) (bool, error) {
	return false, nil
}

// TestScheduler_ClaimRace_CancelledTimerDoesNotFire: a timer cancelled between the due read
// and the fire must NOT fire its payload (and is never written back / resurrected) — the
// claim guard catches it.
func TestScheduler_ClaimRace_CancelledTimerDoesNotFire(t *testing.T) {
	store := claimFailsStore{newFakeStore()}
	store.timers = []domain.TimerRecord{{
		ID: "tmr_1", Title: "Reminder", FireAt: 100, Status: "scheduled",
		PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"ping"}`,
	}}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{}, nil)
	s.Tick(context.Background(), 200)
	if len(queue.published) != 0 {
		t.Errorf("a timer that lost the claim (cancelled under us) must NOT fire; got %d published", len(queue.published))
	}
}
