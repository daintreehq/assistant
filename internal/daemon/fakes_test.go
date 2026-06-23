package daemon

import (
	"context"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// --- in-memory fakes ---------------------------------------------------------

type fakeStore struct {
	mu            sync.Mutex
	timers        []domain.TimerRecord
	watchers      []domain.WatcherRecord
	timerPatches  map[string]map[string]any
	watchPatches  map[string]map[string]any
	workflowPatch map[string]map[string]any
	revoked       map[string]int
	dueTimerErr   error
	dueWatcherErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		timerPatches:  map[string]map[string]any{},
		watchPatches:  map[string]map[string]any{},
		workflowPatch: map[string]map[string]any{},
		revoked:       map[string]int{},
	}
}

func (f *fakeStore) DueTimers(now int64) ([]domain.TimerRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.TimerRecord
	for _, t := range f.timers {
		if t.Status == "scheduled" && t.FireAt <= now {
			out = append(out, t)
		}
	}
	return out, f.dueTimerErr
}

func (f *fakeStore) UpdateTimer(id string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timerPatches[id] = patch
	for i := range f.timers {
		if f.timers[i].ID == id {
			if v, ok := patch["status"].(string); ok {
				f.timers[i].Status = v
			}
			if v, ok := patch["fireAt"].(int64); ok {
				f.timers[i].FireAt = v
			}
			if v, ok := patch["runCount"].(int); ok {
				f.timers[i].RunCount = v
			}
		}
	}
	return nil
}

func (f *fakeStore) ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.timers {
		if f.timers[i].ID != id {
			continue
		}
		// The guard: only claim a row STILL scheduled with the fireAt the scheduler read.
		if f.timers[i].Status != "scheduled" || f.timers[i].FireAt != expectFireAt {
			return false, nil
		}
		f.timerPatches[id] = patch
		if v, ok := patch["status"].(string); ok {
			f.timers[i].Status = v
		}
		if v, ok := patch["fireAt"].(int64); ok {
			f.timers[i].FireAt = v
		}
		if v, ok := patch["runCount"].(int); ok {
			f.timers[i].RunCount = v
		}
		return true, nil
	}
	return false, nil
}

func (f *fakeStore) DueWatchers(now int64) ([]domain.WatcherRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.WatcherRecord
	for _, w := range f.watchers {
		if w.Status == "active" && w.NextCheckAt <= now {
			out = append(out, w)
		}
	}
	return out, f.dueWatcherErr
}

func (f *fakeStore) UpdateWatcher(id string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchPatches[id] = patch
	for i := range f.watchers {
		if f.watchers[i].ID == id {
			if v, ok := patch["status"].(string); ok {
				f.watchers[i].Status = v
			}
		}
	}
	return nil
}

func (f *fakeStore) ClaimDueWatcher(id string, patch map[string]any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.watchers {
		if f.watchers[i].ID != id {
			continue
		}
		if f.watchers[i].Status != "active" {
			return false, nil // cancelled/terminal under us — don't resurrect
		}
		f.watchPatches[id] = patch
		if v, ok := patch["status"].(string); ok {
			f.watchers[i].Status = v
		}
		return true, nil
	}
	// Not tracked in the fake (most check tests pass the rec directly): treat as claimable so
	// the finalize is still exercised. The status-guard semantics are covered by storage tests.
	f.watchPatches[id] = patch
	return true, nil
}

func (f *fakeStore) RevokeGrantsByActor(actorID string, now int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[actorID]++
	return 1, nil
}

func (f *fakeStore) UpdateWorkflowRun(id string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workflowPatch[id] = patch
	return nil
}

type fakeQueue struct {
	mu          sync.Mutex
	published   []domain.QueuePublishArgs
	digest      []domain.QueueEvent
	markedIDs   [][]string
	publishErr  error
	failDeliver bool
}

func newFakeQueue() *fakeQueue { return &fakeQueue{} }

func (q *fakeQueue) Publish(args domain.QueuePublishArgs) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.published = append(q.published, args)
	return q.publishErr
}

func (q *fakeQueue) Digest(opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// Return only un-marked events (NotifiedIsNull semantics handled by markNotified
	// removing them from the digest slice).
	return append([]domain.QueueEvent(nil), q.digest...), nil
}

func (q *fakeQueue) MarkNotified(ids []string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.markedIDs = append(q.markedIDs, ids)
	// Drop marked events from the digest so a re-poll won't re-deliver them.
	marked := map[string]bool{}
	for _, id := range ids {
		marked[id] = true
	}
	var keep []domain.QueueEvent
	for _, e := range q.digest {
		if !marked[e.ID] {
			keep = append(keep, e)
		}
	}
	q.digest = keep
	return nil
}

type fakeMCP struct {
	connected bool
	results   map[string]MCPResult
	errs      map[string]error
	calls     []string
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{connected: true, results: map[string]MCPResult{}, errs: map[string]error{}}
}

func (m *fakeMCP) CallRead(ctx context.Context, name string, args map[string]any) (MCPResult, error) {
	m.calls = append(m.calls, name)
	if err, ok := m.errs[name]; ok {
		return MCPResult{}, err
	}
	if r, ok := m.results[name]; ok {
		return r, nil
	}
	return MCPResult{IsError: true}, nil
}

func (m *fakeMCP) Connected() bool { return m.connected }

type fakeModel struct {
	verdict domain.WatcherVerdict
	judge   func(q string) domain.ModelJudgeAnswer
}

func (m *fakeModel) Classify(ctx context.Context, in ClassifyInput) (domain.WatcherVerdict, error) {
	return m.verdict, nil
}

func (m *fakeModel) Judge(ctx context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error) {
	if m.judge != nil {
		return m.judge(in.Question), nil
	}
	return domain.ModelJudgeAnswer{}, nil
}

type fakeRegistry struct {
	result       domain.ToolResult
	err          error
	calls        int
	lastActorID  string
	lastToolName string
}

func (r *fakeRegistry) Dispatch(ctx context.Context, actor domain.ToolActor, actorID, name, argsJson string) (domain.ToolResult, error) {
	r.calls++
	r.lastActorID = actorID
	r.lastToolName = name
	return r.result, r.err
}

func ctxFor(store Store, queue Queue, mcp MCP, model WatcherModel) *CheckContext {
	return &CheckContext{Ctx: context.Background(), Store: store, Queue: queue, MCP: mcp, Model: model, ProjectPath: "/proj"}
}

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }
func ptrBool(v bool) *bool    { return &v }
func ptrStr(v string) *string { return &v }
