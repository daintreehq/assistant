package daemon

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

func prWatcher(id string, opts PrWatcherOptions) domain.WatcherRecord {
	oj := mustJSON(opts)
	return domain.WatcherRecord{
		ID: id, Kind: "pr_state", Title: "PR watch", Goal: "watch the PR",
		TargetsJson: `["PR #7"]`, CadenceMs: int(PRWatcherCadenceMS), ModelTier: domain.ModelSmall,
		Status: "active", NextCheckAt: 0, CreatedAt: 0, OptionsJson: ptrStr(oj),
	}
}

func TestPrWatcher_MergedTerminalOnFirstObservation(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["forge.getPR"] = MCPResult{StructuredContent: map[string]any{"state": "merged", "title": "Add X"}}
	rec := prWatcher("wch_pr", PrWatcherOptions{PrNumber: 7}) // no lastState → firstObservation
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctxFor(store, queue, mcp, nil), rec)
	if res.Status != "condition_met" || res.Transition != PrTransitionStateChange {
		t.Fatalf("merged on first observation must be a terminal state_change, got %+v", res)
	}
	if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityAttention {
		t.Fatal("a merge should publish attention")
	}
	if store.revoked["wch_pr"] != 1 {
		t.Error("a terminal PR must revoke grants")
	}
}

func TestPrWatcher_DraftReady(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["forge.getPR"] = MCPResult{StructuredContent: map[string]any{"state": "open", "draft": false}}
	rec := prWatcher("wch_dr", PrWatcherOptions{PrNumber: 7, LastState: "open", LastIsDraft: ptrBool(true)})
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctxFor(store, queue, mcp, nil), rec)
	if res.Transition != PrTransitionDraftReady || res.Status != "active" {
		t.Fatalf("draft→ready should be a non-terminal draft_ready, got %+v", res)
	}
}

func TestPrWatcher_TransientFailureReschedulesNoPublish(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["forge.getPR"] = MCPResult{IsError: true} // forge error
	rec := prWatcher("wch_tf", PrWatcherOptions{PrNumber: 7, LastState: "open"})
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctxFor(store, queue, mcp, nil), rec)
	if res.Status != "active" || res.Published {
		t.Fatalf("a forge error must reschedule without publishing, got %+v", res)
	}
	if len(queue.published) != 0 {
		t.Error("transient failure must not publish")
	}
}

func TestPrWatcher_TimeoutWinsBeforeRead(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP() // would error, but timeout must win first
	rec := prWatcher("wch_to", PrWatcherOptions{PrNumber: 7, LastState: "open"})
	rec.StopAfterMs = ptrInt64(1) // created at 0; now>>1 → timed out
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctxFor(store, queue, mcp, nil), rec)
	if res.Status != "timeout" {
		t.Fatalf("timeout must win before any read, got %+v", res)
	}
	// forge.getPR must NOT have been called.
	for _, c := range mcp.calls {
		if c == "forge.getPR" {
			t.Error("timeout should retire before calling forge.getPR")
		}
	}
}

func TestPrWatcher_CorruptOptionsDisabled(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	rec := domain.WatcherRecord{
		ID: "wch_bad", Kind: "pr_state", Title: "Bad", Goal: "g",
		TargetsJson: `["PR #1"]`, CadenceMs: int(PRWatcherCadenceMS), ModelTier: domain.ModelSmall,
		Status: "active", OptionsJson: ptrStr(`{"noPrNumber":true}`),
	}
	store.watchers = []domain.WatcherRecord{rec}
	res := RunPrWatcherCheck(ctxFor(store, queue, newFakeMCP(), nil), rec)
	if res.Status != "error" {
		t.Fatalf("missing prNumber must disable the watcher, got %+v", res)
	}
	if store.watchPatches["wch_bad"]["status"] != "error" {
		t.Error("corrupt PR options should set status error")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
