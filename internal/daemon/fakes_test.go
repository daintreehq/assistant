package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/daintreehq/assistant/internal/domain"
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
	resolvedKeys  map[string]int
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

func (f *fakeStore) ListWatchers(status string) ([]domain.WatcherRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.WatcherRecord
	for _, w := range f.watchers {
		if status == "" || w.Status == status {
			out = append(out, w)
		}
	}
	return out, nil
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
			if v, ok := patch["nextCheckAt"].(int64); ok {
				f.watchers[i].NextCheckAt = v
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

func (f *fakeStore) GetWatcher(id string) (*domain.WatcherRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.watchers {
		if f.watchers[i].ID == id {
			w := f.watchers[i]
			return &w, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ResolveOpenEventsByDedupeKey(key string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolvedKeys == nil {
		f.resolvedKeys = map[string]int{}
	}
	f.resolvedKeys[key]++
	return 1, nil
}

func (f *fakeStore) UpdateWorkflowRun(id string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workflowPatch[id] = patch
	return nil
}

type fakeQueue struct {
	mu         sync.Mutex
	published  []domain.QueuePublishArgs
	digest     []domain.QueueEvent
	markedIDs  [][]string
	publishErr error
	// autoDigest makes Publish land the event in the digest slice too (like the
	// real queue), so notify() sees what a mid-tick job just published.
	autoDigest bool
}

func newFakeQueue() *fakeQueue { return &fakeQueue{} }

func (q *fakeQueue) Publish(args domain.QueuePublishArgs) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.published = append(q.published, args)
	if q.autoDigest {
		q.digest = append(q.digest, domain.QueueEvent{
			ID:       fmt.Sprintf("evt_%d", len(q.published)),
			Source:   args.Source,
			Severity: args.Severity,
			Title:    args.Title,
			Summary:  args.Summary,
			Count:    1,
		})
	}
	return q.publishErr
}

// Digest mirrors the real store's filter contract for the options notify() uses —
// crucially it ENFORCES MaxItems (the real query LIMITs; a fake returning
// everything would mask a notify pass that fails to drain past one page).
func (q *fakeQueue) Digest(opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// The digest slice holds only un-marked events (NotifiedIsNull semantics —
	// markNotified removes acknowledged rows).
	var out []domain.QueueEvent
	for _, e := range q.digest {
		if opts.SeverityAtLeast != nil && domain.RankOf(e.Severity) < domain.RankOf(*opts.SeverityAtLeast) {
			continue
		}
		out = append(out, e)
	}
	if opts.MaxItems != nil && *opts.MaxItems < len(out) {
		out = out[:*opts.MaxItems]
	}
	return append([]domain.QueueEvent(nil), out...), nil
}

// MarkNotified mirrors the daemon.Queue contract: the acknowledgement is
// VERSION-CONDITIONAL. An event whose stored row no longer matches the digested
// snapshot (count or updated-at changed — a publisher updated it between Digest
// and the mark) stays in the digest for re-delivery.
func (q *fakeQueue) MarkNotified(evs []domain.QueueEvent) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make([]string, 0, len(evs))
	marked := map[string]bool{}
	for _, ack := range evs {
		for _, cur := range q.digest {
			if cur.ID != ack.ID {
				continue
			}
			if cur.Count == ack.Count && coalesceUpdated(cur) == coalesceUpdated(ack) {
				marked[ack.ID] = true
				ids = append(ids, ack.ID)
			}
			break
		}
	}
	q.markedIDs = append(q.markedIDs, ids)
	// Drop marked events from the digest so a re-poll won't re-deliver them.
	var keep []domain.QueueEvent
	for _, e := range q.digest {
		if !marked[e.ID] {
			keep = append(keep, e)
		}
	}
	q.digest = keep
	return nil
}

// bump models a publisher's dedupe update racing the notify pass: count+1,
// updatedAt advanced, content refreshed (the row stays unnotified).
func (q *fakeQueue) bump(id, title, summary string, at int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.digest {
		if q.digest[i].ID == id {
			q.digest[i].Count++
			q.digest[i].Title = title
			q.digest[i].Summary = summary
			q.digest[i].UpdatedAt = &at
			return
		}
	}
}

func coalesceUpdated(e domain.QueueEvent) int64 {
	if e.UpdatedAt != nil {
		return *e.UpdatedAt
	}
	return e.CreatedAt
}

type fakeMCP struct {
	connected bool
	results   map[string]MCPResult
	errs      map[string]error
	calls     []string

	// resource-subscription seam
	supportsSub  bool
	subscribed   []string
	unsubscribed []string
	subErr       error
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

func (m *fakeMCP) SupportsSubscribe() bool { return m.supportsSub }

func (m *fakeMCP) Subscribe(ctx context.Context, uri string) error {
	if m.subErr != nil {
		return m.subErr
	}
	m.subscribed = append(m.subscribed, uri)
	return nil
}

func (m *fakeMCP) Unsubscribe(ctx context.Context, uri string) error {
	m.unsubscribed = append(m.unsubscribed, uri)
	return nil
}

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

// fakeMemoryWriter captures episodic memories the watcher finalize path writes.
type fakeMemoryWriter struct {
	mu       sync.Mutex
	inserted []domain.MemoryRecord
	err      error
}

func (w *fakeMemoryWriter) InsertMemory(rec domain.MemoryRecord) (domain.MemoryRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return domain.MemoryRecord{}, w.err
	}
	if rec.ID == "" {
		rec.ID = "mem_fake"
	}
	w.inserted = append(w.inserted, rec)
	return rec, nil
}

func (w *fakeMemoryWriter) records() []domain.MemoryRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]domain.MemoryRecord(nil), w.inserted...)
}

func ctxFor(store Store, queue Queue, mcp MCP, model WatcherModel) *CheckContext {
	return &CheckContext{Ctx: context.Background(), Store: store, Queue: queue, MCP: mcp, Model: model, ProjectPath: "/proj"}
}

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }
func ptrBool(v bool) *bool    { return &v }
func ptrStr(v string) *string { return &v }
