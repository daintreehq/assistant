package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// scheduler_pressure_test.go pins the tick's MCP load shape: N due terminal
// watchers share ONE prefetched batched terminal.getStatus (never N separate
// reads), and job execution within a pass is capped at tickJobConcurrency so a
// pile of simultaneously-due items can't burst the server.

func TestScheduler_PrefetchSharesOneStatusReadAcrossWatchers(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// Three watchers over three distinct working terminals with inline output —
	// the deterministic still-working fast path: no getOutput fallback, no model.
	mcp := newProgMCP(map[string]termCfg{
		"term-1": {agentState: "working", recentOutput: strptr("building 1")},
		"term-2": {agentState: "working", recentOutput: strptr("building 2")},
		"term-3": {agentState: "working", recentOutput: strptr("building 3")},
	})
	store.watchers = []domain.WatcherRecord{
		watcherWith("wch_1", []string{"term-1"}),
		watcherWith("wch_2", []string{"term-2"}),
		watcherWith("wch_3", []string{"term-3"}),
	}
	model := &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, mcp, model)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), domain.NowMS())

	status := mcp.callsFor("terminal.getStatus")
	if len(status) != 1 {
		t.Fatalf("three due watchers must share ONE prefetched getStatus, got %d calls", len(status))
	}
	got := map[string]bool{}
	for _, id := range stringIDs(status[0].args["terminalIds"]) {
		got[id] = true
	}
	for _, want := range []string{"term-1", "term-2", "term-3"} {
		if !got[want] {
			t.Errorf("prefetch id set %v missing %s", got, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("prefetch must cover exactly the union of watcher targets, got %v", got)
	}
	if _, ok := status[0].args["includeOutput"]; !ok {
		t.Error("the prefetched status read must request includeOutput (the watcher read shape)")
	}
	// Every watcher still produced its check (re-armed with a fresh nextCheckAt).
	for _, id := range []string{"wch_1", "wch_2", "wch_3"} {
		if store.watchPatches[id] == nil {
			t.Errorf("watcher %s never finalized its check", id)
		}
	}
}

func TestScheduler_PrefetchSkipsCorruptTargetsWatcher(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-1": {agentState: "working", recentOutput: strptr("building")},
	})
	bad := watcherWith("wch_bad", []string{"term-x"})
	bad.TargetsJson = "not-json"
	store.watchers = []domain.WatcherRecord{
		watcherWith("wch_good", []string{"term-1"}),
		bad,
	}
	model := &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, mcp, model)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), domain.NowMS())

	status := mcp.callsFor("terminal.getStatus")
	if len(status) != 1 {
		t.Fatalf("expected one prefetched getStatus despite the corrupt watcher, got %d", len(status))
	}
	if ids := stringIDs(status[0].args["terminalIds"]); len(ids) != 1 || ids[0] != "term-1" {
		t.Errorf("prefetch must skip the corrupt watcher's targets, got %v", ids)
	}
	// The corrupt watcher is disabled by its own check; the good one finalizes.
	if store.watchPatches["wch_bad"] == nil || store.watchPatches["wch_bad"]["status"] != "error" {
		t.Errorf("corrupt watcher must be disabled, got %v", store.watchPatches["wch_bad"])
	}
	if store.watchPatches["wch_good"] == nil {
		t.Error("the healthy watcher never finalized its check")
	}
}

// failingStatusMCP fails every terminal.getStatus (tool-level error) while
// serving getOutput normally, recording all calls — the shared-read-failure
// probe: watchers must NOT fan out per-terminal deep reads when the one shared
// prefetch already failed.
type failingStatusMCP struct {
	mu    sync.Mutex
	calls []string
}

func (m *failingStatusMCP) CallRead(_ context.Context, name string, _ map[string]any) (MCPResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name)
	m.mu.Unlock()
	if name == "terminal.getStatus" {
		return MCPResult{Text: "boom", IsError: true}, nil
	}
	return MCPResult{StructuredContent: map[string]any{"content": "tail"}}, nil
}
func (m *failingStatusMCP) Connected() bool                               { return true }
func (m *failingStatusMCP) SupportsSubscribe() bool                       { return false }
func (m *failingStatusMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (m *failingStatusMCP) Unsubscribe(_ context.Context, _ string) error { return nil }

func (m *failingStatusMCP) count(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c == name {
			n++
		}
	}
	return n
}

func TestScheduler_SharedPrefetchFailureShortCircuitsFanOut(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := &failingStatusMCP{}
	store.watchers = []domain.WatcherRecord{
		watcherWith("wch_1", []string{"term-1"}),
		watcherWith("wch_2", []string{"term-2"}),
	}
	model := &progModel{classErr: errModelMustNotRun}
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, mcp, model)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), domain.NowMS())

	// Exactly the one failed prefetch — the watchers must not re-read status
	// themselves NOR fall into the per-terminal getOutput fan-out.
	if got := mcp.count("terminal.getStatus"); got != 1 {
		t.Errorf("terminal.getStatus calls = %d, want just the failed prefetch", got)
	}
	if got := mcp.count("terminal.getOutput"); got != 0 {
		t.Errorf("terminal.getOutput calls = %d, want 0 on a shared read failure", got)
	}
	// Both watchers re-armed quietly (still active, nothing published).
	for _, id := range []string{"wch_1", "wch_2"} {
		p := store.watchPatches[id]
		if p == nil || p["status"] != "active" {
			t.Errorf("watcher %s must re-arm active on a shared read failure, got %v", id, p)
		}
	}
	if len(queue.published) != 0 {
		t.Errorf("a shared read failure must not publish, got %d events", len(queue.published))
	}
}

func TestWatcher_StalePrefetchFallsBackToOwnRead(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-1": {agentState: "working", recentOutput: strptr("building")},
	})
	rec := watcherWith("wch_1", []string{"term-1"})
	model := &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}

	// Fresh snapshot: consumed in place of an own read.
	fresh := ctxFor(store, queue, mcp, model)
	fresh.PrefetchedStatuses = &StatusBatch{Ok: true, ByID: map[string]TerminalStatusEntry{
		"term-1": {TerminalID: "term-1", AgentState: "working", RecentOutput: strptr("building")},
	}}
	fresh.PrefetchedStatusesAt = domain.NowMS()
	RunTerminalWatcherCheck(fresh, rec)
	if got := len(mcp.callsFor("terminal.getStatus")); got != 0 {
		t.Errorf("a fresh prefetch must be used in place of an own read, got %d reads", got)
	}

	// Stale snapshot (older than PrefetchFreshnessMS): the check re-reads itself.
	stale := ctxFor(store, queue, mcp, model)
	stale.PrefetchedStatuses = fresh.PrefetchedStatuses
	stale.PrefetchedStatusesAt = domain.NowMS() - PrefetchFreshnessMS - 1_000
	RunTerminalWatcherCheck(stale, rec)
	if got := len(mcp.callsFor("terminal.getStatus")); got != 1 {
		t.Errorf("a stale prefetch must fall back to the watcher's own read, got %d reads", got)
	}
}

func TestScheduler_PrefetchSkippedWhenDisconnected(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-1": {agentState: "working"}})
	mcp.connected = false
	store.watchers = []domain.WatcherRecord{watcherWith("wch_1", []string{"term-1"})}
	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, mcp, &progModel{})
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})
	s.Tick(context.Background(), domain.NowMS())

	if calls := mcp.callsFor("terminal.getStatus"); len(calls) != 0 {
		t.Fatalf("disconnected MCP must see no status reads at all, got %d", len(calls))
	}
	// The watcher still ran (its own !connected branch) and re-armed.
	if store.watchPatches["wch_1"] == nil {
		t.Error("the watcher check must still run (and re-arm) while disconnected")
	}
}

func TestScheduler_TickJobsBoundedByConcurrencyCap(t *testing.T) {
	store := newFakeStore()
	for i := 0; i < 10; i++ {
		store.timers = append(store.timers, domain.TimerRecord{
			ID: "tmr_" + string(rune('a'+i)), Title: "t", FireAt: 0, Status: "scheduled",
			PayloadType: "call_safe_tool",
			PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"test.tool","args":{}}}`,
		})
	}
	queue := newFakeQueue()
	var inFlight, maxSeen int64
	release := make(chan struct{})
	reg := &blockingRegistry{enter: func() {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			m := atomic.LoadInt64(&maxSeen)
			if n <= m || atomic.CompareAndSwapInt64(&maxSeen, m, n) {
				break
			}
		}
		<-release
		atomic.AddInt64(&inFlight, -1)
	}}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: reg})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Tick(context.Background(), 10)
	}()

	// Wait for the pool to fill, then assert the cap held and release everyone.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&inFlight) < int64(tickJobConcurrency) {
		if time.Now().After(deadline) {
			t.Fatalf("pool never filled: %d in flight", atomic.LoadInt64(&inFlight))
		}
		time.Sleep(time.Millisecond)
	}
	// Give any over-cap job a beat to (incorrectly) start before we assert.
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt64(&maxSeen); got > int64(tickJobConcurrency) {
		t.Errorf("observed %d concurrent jobs; cap is %d", got, tickJobConcurrency)
	}
	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&maxSeen); got > int64(tickJobConcurrency) {
		t.Errorf("observed %d concurrent jobs after drain; cap is %d", got, tickJobConcurrency)
	}
}
