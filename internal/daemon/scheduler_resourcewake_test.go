package daemon

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Covers the resource-update fast path (issue #204): a pushed agent-state
// transition brings a not-yet-due terminal watcher forward and runs its check
// immediately, instead of waiting for the next tick interval.

func TestScheduler_ResourceWakeRunsImmediateCheck(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr("")},
	})
	mcp.supportsSub = true
	// Active terminal watcher whose next poll is far in the future.
	rec := watcherWith("wch_wake", []string{"term-a"})
	rec.NextCheckAt = domain.NowMS() + 1_000_000
	store.watchers = []domain.WatcherRecord{rec}

	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, mcp, workingModel())
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})

	s.onResourceWake(context.Background())

	// The wake nudged the not-yet-due watcher and ran its check this pass.
	if len(mcp.callsFor("terminal.getStatus")) == 0 {
		t.Fatal("a resource wake must run an immediate watcher check (getStatus polled)")
	}
}

func TestScheduler_ResourceWakeIgnoresNonTerminalWatchers(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// A PR watcher (non-terminal kind) must NOT be nudged by a terminal resource wake.
	pr := prWatcher("wch_pr", PrWatcherOptions{PrNumber: 7, LastState: "open"})
	pr.NextCheckAt = domain.NowMS() + 1_000_000
	store.watchers = []domain.WatcherRecord{pr}

	ctxFn := func(ctx context.Context, _ domain.ToolActor, _ string) *CheckContext {
		return ctxFor(store, queue, newProgMCP(map[string]termCfg{}), nil)
	}
	s := NewScheduler(SchedulerDeps{Store: store, Queue: queue, Registry: &fakeRegistry{}, CtxFor: ctxFn})

	s.onResourceWake(context.Background())

	if patch, ok := store.watchPatches["wch_pr"]; ok {
		if _, nudged := patch["nextCheckAt"]; nudged {
			t.Error("a PR watcher must not be pulled forward by a terminal resource wake")
		}
	}
}

func TestDrainResourceUpdates(t *testing.T) {
	ch := make(chan string, 8)
	for i := 0; i < 5; i++ {
		ch <- "u"
	}
	drainResourceUpdates(ch)
	if len(ch) != 0 {
		t.Errorf("drain should empty the channel, %d left", len(ch))
	}
}
