package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// Covers PR-watcher edges not in prwatcher_test.go: baseline-on-first-
// observation, forge.getPR args (cwd/projectPath fallback + bounded timeout),
// closed→stop, activity@info (no double-publish), silent-unchanged, payload shapes,
// the disconnected/throws/isError/unrecognizable transient guards, and the two-step
// baseline→merge integration.

// prMCP returns a configurable forge.getPR result and records the args.
type prMCP struct {
	connected bool
	result    MCPResult
	err       error
	calls     []map[string]any
}

func (m *prMCP) Connected() bool                               { return m.connected }
func (m *prMCP) SupportsSubscribe() bool                       { return false }
func (m *prMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (m *prMCP) Unsubscribe(_ context.Context, _ string) error { return nil }
func (m *prMCP) CallRead(_ context.Context, name string, args map[string]any) (MCPResult, error) {
	if name != "forge.getPR" {
		return MCPResult{IsError: true}, nil
	}
	m.calls = append(m.calls, args)
	return m.result, m.err
}

func prCtx(m MCP) (*CheckContext, *fakeStore, *fakeQueue) {
	store := newFakeStore()
	queue := newFakeQueue()
	return &CheckContext{Ctx: context.Background(), Store: store, Queue: queue, MCP: m, ProjectPath: "/default/project"}, store, queue
}

func sc(m map[string]any) MCPResult { return MCPResult{StructuredContent: m} }

func TestPrWatcher_BaselineOnFirstObservationNoPublish(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open", "draft": false, "updated_at": "2026-01-01T00:00:00Z"})}
	ctx, store, queue := prCtx(mcp)
	rec := prWatcher("wch_b", PrWatcherOptions{PrNumber: 5}) // no lastState
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctx, rec)
	if res.Published || res.Status != "active" {
		t.Fatalf("an open PR on first observation must only baseline, got %+v", res)
	}
	if len(queue.published) != 0 {
		t.Error("baseline must not publish")
	}
	// The new baseline is persisted.
	var opts PrWatcherOptions
	_ = json.Unmarshal([]byte(store.watchPatches["wch_b"]["optionsJson"].(string)), &opts)
	if opts.LastState != "open" || opts.LastIsDraft == nil || *opts.LastIsDraft || opts.LastUpdatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("baseline state not persisted, got %+v", opts)
	}
}

func TestPrWatcher_ForgeArgsCwdAndFallback(t *testing.T) {
	t.Run("explicit cwd", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open"})}
		ctx, store, _ := prCtx(mcp)
		rec := prWatcher("wch_c", PrWatcherOptions{PrNumber: 77, Cwd: "/my/repo", LastState: "open"})
		store.watchers = []domain.WatcherRecord{rec}
		RunPrWatcherCheck(ctx, rec)
		if len(mcp.calls) != 1 {
			t.Fatalf("expected one forge.getPR call, got %d", len(mcp.calls))
		}
		if mcp.calls[0]["cwd"] != "/my/repo" || mcp.calls[0]["prNumber"] != 77 {
			t.Errorf("forge.getPR args wrong, got %+v", mcp.calls[0])
		}
	})
	t.Run("falls back to projectPath", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open"})}
		ctx, store, _ := prCtx(mcp)
		rec := prWatcher("wch_d", PrWatcherOptions{PrNumber: 5, LastState: "open"})
		store.watchers = []domain.WatcherRecord{rec}
		RunPrWatcherCheck(ctx, rec)
		if mcp.calls[0]["cwd"] != "/default/project" {
			t.Errorf("cwd should fall back to projectPath, got %v", mcp.calls[0]["cwd"])
		}
	})
}

func TestPrWatcher_ClosedStopsAndPublishes(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "closed"})}
	ctx, store, queue := prCtx(mcp)
	rec := prWatcher("wch_cl", PrWatcherOptions{PrNumber: 3, LastState: "open"})
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctx, rec)
	if res.Transition != PrTransitionStateChange || res.Status != "condition_met" {
		t.Fatalf("closed → state_change/condition_met, got %+v", res)
	}
	if !anyTitleContains(queue, "closed") {
		t.Error("the published event title should mention closed")
	}
}

func TestPrWatcher_MergedRevokesGrants(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open", "merged": true})}
	ctx, store, queue := prCtx(mcp)
	rec := prWatcher("wch_mg", PrWatcherOptions{PrNumber: 9, LastState: "open"})
	store.watchers = []domain.WatcherRecord{rec}

	RunPrWatcherCheck(ctx, rec)
	if store.revoked["wch_mg"] != 1 {
		t.Error("a stopped PR watcher must revoke its scoped grants")
	}
	if !anyTitleContains(queue, "merged") {
		t.Error("the published event should mention merged")
	}
}

func TestPrWatcher_ActivityAtInfoNoDoublePublish(t *testing.T) {
	t.Run("updatedAt advances → activity@info", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open", "updated_at": "2026-02-02T00:00:00Z"})}
		ctx, store, queue := prCtx(mcp)
		rec := prWatcher("wch_a", PrWatcherOptions{PrNumber: 1, LastState: "open", LastUpdatedAt: "2026-01-01T00:00:00Z"})
		store.watchers = []domain.WatcherRecord{rec}
		res := RunPrWatcherCheck(ctx, rec)
		if res.Transition != PrTransitionActivity {
			t.Fatalf("advanced updatedAt → activity, got %s", res.Transition)
		}
		if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityInfo {
			t.Errorf("activity must publish a single info event, got %+v", queue.published)
		}
	})
	t.Run("state change suppresses the activity ping", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "merged", "updated_at": "2026-02-02T00:00:00Z"})}
		ctx, store, queue := prCtx(mcp)
		rec := prWatcher("wch_a2", PrWatcherOptions{PrNumber: 1, LastState: "open", LastUpdatedAt: "2026-01-01T00:00:00Z"})
		store.watchers = []domain.WatcherRecord{rec}
		res := RunPrWatcherCheck(ctx, rec)
		if res.Transition != PrTransitionStateChange {
			t.Fatalf("a merge should win over activity, got %s", res.Transition)
		}
		if len(queue.published) != 1 {
			t.Errorf("exactly one event (the merge), not merge + activity, got %d", len(queue.published))
		}
	})
}

func TestPrWatcher_SilentWhenUnchanged(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open", "draft": false, "updated_at": "2026-01-01T00:00:00Z"})}
	ctx, store, queue := prCtx(mcp)
	last := false
	rec := prWatcher("wch_s", PrWatcherOptions{PrNumber: 1, LastState: "open", LastIsDraft: &last, LastUpdatedAt: "2026-01-01T00:00:00Z"})
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctx, rec)
	if res.Published || res.Status != "active" {
		t.Fatalf("nothing changed → silent + active, got %+v", res)
	}
	if len(queue.published) != 0 {
		t.Error("unchanged PR must publish nothing")
	}
}

func TestPrWatcher_GitLabDraftReady(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "opened", "work_in_progress": false})}
	ctx, store, _ := prCtx(mcp)
	last := true
	rec := prWatcher("wch_gl", PrWatcherOptions{PrNumber: 6, LastState: "open", LastIsDraft: &last})
	store.watchers = []domain.WatcherRecord{rec}
	res := RunPrWatcherCheck(ctx, rec)
	if res.Transition != PrTransitionDraftReady {
		t.Fatalf("GitLab opened+!wip from a draft → draft_ready, got %s", res.Transition)
	}
}

func TestPrWatcher_PayloadShapes(t *testing.T) {
	t.Run("JSON text body", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: MCPResult{Text: `{"state":"merged"}`}}
		ctx, store, _ := prCtx(mcp)
		rec := prWatcher("wch_tb", PrWatcherOptions{PrNumber: 2, LastState: "open"})
		store.watchers = []domain.WatcherRecord{rec}
		res := RunPrWatcherCheck(ctx, rec)
		if res.Transition != PrTransitionStateChange || res.State != "merged" {
			t.Fatalf("text-body PR should parse merged, got %+v", res)
		}
	})
	t.Run("nested pr wrapper", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"pr": map[string]any{"state": "closed"}})}
		ctx, store, _ := prCtx(mcp)
		rec := prWatcher("wch_w", PrWatcherOptions{PrNumber: 4, LastState: "open"})
		store.watchers = []domain.WatcherRecord{rec}
		res := RunPrWatcherCheck(ctx, rec)
		if res.State != "closed" {
			t.Fatalf("nested pr wrapper should parse closed, got %+v", res)
		}
	})
	t.Run("envelope state ignored, nested PR read", func(t *testing.T) {
		mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "ok", "pr": map[string]any{"state": "merged"}})}
		ctx, store, queue := prCtx(mcp)
		rec := prWatcher("wch_e", PrWatcherOptions{PrNumber: 12, LastState: "open"})
		store.watchers = []domain.WatcherRecord{rec}
		res := RunPrWatcherCheck(ctx, rec)
		if res.State != "merged" {
			t.Fatalf("envelope state must not short-circuit the nested merge, got %+v", res)
		}
		if len(queue.published) != 1 {
			t.Error("the merge should publish once")
		}
	})
}

func TestPrWatcher_TransientGuards(t *testing.T) {
	mkRec := func() domain.WatcherRecord {
		return prWatcher("wch_t", PrWatcherOptions{PrNumber: 1, LastState: "open"})
	}
	cases := []struct {
		name string
		mcp  *prMCP
	}{
		{"disconnected", &prMCP{connected: false}},
		{"throws", &prMCP{connected: true, err: context.DeadlineExceeded}},
		{"isError", &prMCP{connected: true, result: MCPResult{IsError: true, StructuredContent: map[string]any{"state": "merged"}}}},
		{"unrecognizable", &prMCP{connected: true, result: sc(map[string]any{"totallyUnrelated": true})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, queue := prCtx(tc.mcp)
			rec := mkRec()
			store.watchers = []domain.WatcherRecord{rec}
			res := RunPrWatcherCheck(ctx, rec)
			if res.Published || res.Status != "active" {
				t.Fatalf("%s must reschedule without publishing, got %+v", tc.name, res)
			}
			if len(queue.published) != 0 {
				t.Errorf("%s must publish nothing", tc.name)
			}
		})
	}
}

func TestPrWatcher_TimeoutPublishesInfo(t *testing.T) {
	mcp := &prMCP{connected: false}
	ctx, store, queue := prCtx(mcp)
	rec := prWatcher("wch_to", PrWatcherOptions{PrNumber: 1, LastState: "open"})
	rec.StopAfterMs = ptrInt64(1)
	rec.CreatedAt = domain.NowMS() - 60_000
	store.watchers = []domain.WatcherRecord{rec}

	res := RunPrWatcherCheck(ctx, rec)
	if res.Status != "timeout" {
		t.Fatalf("a watcher past stopAfterMs must time out, got %+v", res)
	}
	if len(queue.published) != 1 || !containsStr(queue.published[0].Title, "watch ended") {
		t.Errorf("timeout should publish a single 'watch ended' event, got %+v", queue.published)
	}
}

func TestPrWatcher_TwoStepBaselineThenMerge(t *testing.T) {
	mcp := &prMCP{connected: true, result: sc(map[string]any{"state": "open"})}
	ctx, store, queue := prCtx(mcp)
	rec := prWatcher("wch_2", PrWatcherOptions{PrNumber: 21})
	store.watchers = []domain.WatcherRecord{rec}

	// First poll: open → baseline only.
	first := RunPrWatcherCheck(ctx, rec)
	if first.Published {
		t.Fatal("first poll should only baseline")
	}
	// Re-load the persisted baseline into the record, then poll as merged.
	var opts PrWatcherOptions
	_ = json.Unmarshal([]byte(store.watchPatches["wch_2"]["optionsJson"].(string)), &opts)
	rec.OptionsJson = ptrStr(mustJSON(opts))
	mcp.result = sc(map[string]any{"state": "open", "merged": true})

	second := RunPrWatcherCheck(ctx, rec)
	if second.Transition != PrTransitionStateChange {
		t.Fatalf("second poll should fire on the merge, got %s", second.Transition)
	}
	if len(queue.published) != 1 {
		t.Errorf("exactly one event across the two steps, got %d", len(queue.published))
	}
}

func anyTitleContains(q *fakeQueue, sub string) bool {
	for _, p := range q.published {
		if containsStr(p.Title, sub) || containsStr(p.Summary, sub) {
			return true
		}
	}
	return false
}
