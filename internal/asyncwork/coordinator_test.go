package asyncwork

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// fakeReader scripts per-tick statuses. Each ReadStatuses call consumes the next
// scripted frame (the last frame repeats once the script is exhausted). The
// roster backs the coordinator's ListTerminals absence check.
type fakeReader struct {
	mu        sync.Mutex
	connected bool
	frames    []StatusReadResult
	idx       int
	roster    []string
	rosterOK  bool
}

func (r *fakeReader) Connected() bool { return r.connected }

func (r *fakeReader) ReadStatuses(_ context.Context, _ []string) StatusReadResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return StatusReadResult{OK: false}
	}
	f := r.frames[r.idx]
	if r.idx < len(r.frames)-1 {
		r.idx++
	}
	return f
}

func (r *fakeReader) ListTerminals(context.Context) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roster, r.rosterOK
}

type publishedEvent struct {
	args domain.QueuePublishArgs
}

type fakeQueue struct {
	mu       sync.Mutex
	events   []publishedEvent
	failNext int // fail this many publishes before succeeding
}

func (q *fakeQueue) Publish(_ context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failNext > 0 {
		q.failNext--
		return domain.QueueEvent{}, errors.New("queue unavailable")
	}
	q.events = append(q.events, publishedEvent{args: args})
	return domain.QueueEvent{ID: "evt_test", Source: args.Source, Severity: args.Severity, Title: args.Title, Summary: args.Summary, Target: args.Target}, nil
}

func (q *fakeQueue) all() []publishedEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]publishedEvent, len(q.events))
	copy(out, q.events)
	return out
}

// fakeStore records claim patches; refuse simulates a lost claim (cancel won);
// errClaims simulates transient storage errors; onClaim (when set) runs before
// a successful claim commits — the seam for injecting a mid-pass race.
type fakeStore struct {
	mu        sync.Mutex
	claims    map[string][]map[string]any
	updates   map[string][]map[string]any
	refuse    map[string]bool
	errClaims int // return (false, err) for this many claims
	onClaim   func(id string, patch map[string]any)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		claims:  map[string][]map[string]any{},
		updates: map[string][]map[string]any{},
		refuse:  map[string]bool{},
	}
}

func (s *fakeStore) ClaimLiveAsyncInvocation(id string, patch map[string]any) (bool, error) {
	s.mu.Lock()
	if s.errClaims > 0 {
		s.errClaims--
		s.mu.Unlock()
		return false, errors.New("db busy")
	}
	if s.refuse[id] {
		s.mu.Unlock()
		return false, nil
	}
	s.claims[id] = append(s.claims[id], patch)
	hook := s.onClaim
	s.mu.Unlock()
	if hook != nil {
		hook(id, patch)
	}
	return true, nil
}

func (s *fakeStore) UpdateAsyncInvocation(id string, patch map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates[id] = append(s.updates[id], patch)
	return nil
}

func (s *fakeStore) lastStatus(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	patches := s.claims[id]
	for i := len(patches) - 1; i >= 0; i-- {
		if st, ok := patches[i]["status"].(string); ok {
			return st
		}
	}
	return ""
}

func (s *fakeStore) eventStamps(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.updates[id] {
		if _, ok := p["queueEventId"]; ok {
			n++
		}
	}
	return n
}

// harness bundles a coordinator with manually-driven ticks (Start is never
// called; Tick(ctx, now) is invoked directly with an explicit clock).
type harness struct {
	c      *Coordinator
	reader *fakeReader
	queue  *fakeQueue
	store  *fakeStore
	notify *int
}

func newHarness(frames []StatusReadResult) *harness {
	reader := &fakeReader{connected: true, frames: frames, rosterOK: true, roster: []string{"term-1", "term-2"}}
	queue := &fakeQueue{}
	store := newFakeStore()
	notified := 0
	c := New(Deps{
		Reader:        reader,
		Queue:         queue,
		Store:         store,
		Notify:        func() { notified++ },
		SettleGraceMS: 2500,
	})
	// Mark started so Register accepts without spinning up the ticker goroutine.
	c.stateMu.Lock()
	c.started = true
	c.stateMu.Unlock()
	return &harness{c: c, reader: reader, queue: queue, store: store, notify: &notified}
}

func inv(id, group string, createdAt, expiresAt int64) domain.AsyncInvocationRecord {
	return domain.AsyncInvocationRecord{
		ID: id, ToolName: "terminal.await.async", Title: "job " + id, GroupID: group,
		SessionID: "ses_test", TerminalIdsJson: `["term-1"]`,
		Status: domain.AsyncRunning, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
}

func frame(ok bool, entries map[string]TerminalStatus) StatusReadResult {
	return StatusReadResult{OK: ok, ByID: entries}
}

func TestCoordinatorSingleInvocationLifecycle(t *testing.T) {
	// Tick 1: working (latches seenWorking). Tick 2: waiting → settles.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "working"}}),
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "waiting"}}),
	})
	rec := inv("asy_1", "run_a", 1_000, 100_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // working
	if got := h.store.lastStatus("asy_1"); got != "" {
		t.Fatalf("premature transition to %q", got)
	}
	// waiting-after-working → settles into the coalescing window. Run-scoped
	// groups ALWAYS wait out the grace (a same-turn sibling may not have
	// registered yet), so no publish until settleAt passes.
	h.c.Tick(ctx, 3_000)
	if len(h.queue.all()) != 0 {
		t.Fatal("run-scoped group published before the grace elapsed")
	}
	h.c.Tick(ctx, 6_000) // settleAt (3000+2500) passed → publish
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	ev := events[0].args
	if ev.Source != domain.SourceAsyncTool || ev.Severity != domain.SeverityAttention {
		t.Errorf("event source/severity = %s/%s", ev.Source, ev.Severity)
	}
	if !strings.Contains(ev.Title, "job asy_1") {
		t.Errorf("title %q missing invocation title", ev.Title)
	}
	if ev.Target == nil || ev.Target.TerminalID != "term-1" || ev.Target.AsyncInvocationID != "asy_1" {
		t.Errorf("target = %+v, want terminal+async ids", ev.Target)
	}
	if got := h.store.lastStatus("asy_1"); got != string(domain.AsyncSucceeded) {
		t.Errorf("final status = %q, want succeeded", got)
	}
	if h.store.eventStamps("asy_1") != 1 {
		t.Errorf("queueEventId stamps = %d, want 1", h.store.eventStamps("asy_1"))
	}
	if *h.notify != 1 {
		t.Errorf("notify called %d times, want 1", *h.notify)
	}
	if h.c.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d after publish, want 0", h.c.ActiveCount())
	}
}

func TestCoordinatorFailedExitPublishesFailed(t *testing.T) {
	code := 2
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "exited", ExitCode: &code}}),
	})
	rec := inv("asy_f", "run_f", 1_000, 100_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // exited → settles (settleAt 4500)
	h.c.Tick(ctx, 5_000) // grace elapsed → publish
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if !strings.Contains(events[0].args.Title, "failures") {
		t.Errorf("title %q should mention failures", events[0].args.Title)
	}
	if !strings.Contains(events[0].args.Summary, "exited with code 2") {
		t.Errorf("summary %q should carry the exit code", events[0].args.Summary)
	}
	if got := h.store.lastStatus("asy_f"); got != string(domain.AsyncFailed) {
		t.Errorf("final status = %q, want failed", got)
	}
}

func TestCoordinatorGroupCoalescesSiblings(t *testing.T) {
	// Two invocations in one group over different terminals. term-1 settles on
	// tick 2; term-2 settles on tick 3 (inside the 2.5s grace) → ONE event.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "working"}, "term-2": {AgentState: "working"}}),
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "waiting"}, "term-2": {AgentState: "working"}}),
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "waiting"}, "term-2": {AgentState: "waiting"}}),
	})
	a := inv("asy_a", "run_g", 1_000, 100_000)
	b := domain.AsyncInvocationRecord{
		ID: "asy_b", ToolName: "terminal.await.async", Title: "job asy_b", GroupID: "run_g",
		SessionID: "ses_test", TerminalIdsJson: `["term-2"]`,
		Status: domain.AsyncRunning, CreatedAt: 1_100, ExpiresAt: 100_000,
	}
	if err := h.c.Register(a, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Register(b, []string{"term-2"}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // both working
	h.c.Tick(ctx, 3_000) // a settles → settling (settleAt 5500); b still working
	if len(h.queue.all()) != 0 {
		t.Fatalf("published before the grace/sibling settled")
	}
	h.c.Tick(ctx, 4_000) // b settles too (settleAt 6500) — grace still holds
	if len(h.queue.all()) != 0 {
		t.Fatalf("run-scoped group published before the grace elapsed")
	}
	h.c.Tick(ctx, 5_600) // a's settleAt passed → whole group publishes together
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1 grouped", len(events))
	}
	ev := events[0].args
	if !strings.Contains(ev.Title, "2 async operations") {
		t.Errorf("grouped title = %q", ev.Title)
	}
	if !strings.Contains(ev.Summary, "asy_a") || !strings.Contains(ev.Summary, "asy_b") {
		t.Errorf("grouped summary %q missing member ids", ev.Summary)
	}
	if got := h.store.lastStatus("asy_a"); got != string(domain.AsyncSucceeded) {
		t.Errorf("asy_a final = %q", got)
	}
	if got := h.store.lastStatus("asy_b"); got != string(domain.AsyncSucceeded) {
		t.Errorf("asy_b final = %q", got)
	}
	if *h.notify != 1 {
		t.Errorf("notify called %d times, want 1 (one grouped delivery)", *h.notify)
	}
}

func TestCoordinatorGraceWaitsForPollingSibling(t *testing.T) {
	// a settles while b keeps working past the grace: a publishes alone once its
	// settleAt passes; b publishes later.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "waiting"}, "term-2": {AgentState: "working"}}),
	})
	a := inv("asy_a", "run_g", 1_000, 900_000)
	a.TerminalIdsJson = `["term-1"]`
	b := inv("asy_b", "run_g", 1_000, 900_000)
	b.ID, b.Title, b.TerminalIdsJson = "asy_b", "job asy_b", `["term-2"]`
	// term-1 reads waiting with no seenWorking: rely on the spawn grace instead —
	// createdAt far in the past so msSinceSpawn >= FinishSettleGraceMS.
	if err := h.c.Register(a, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Register(b, []string{"term-2"}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := int64(1_000 + domain.FinishSettleGraceMS + 1_000)
	h.c.Tick(ctx, now) // a settles (grace path) → settling; b still polling
	if len(h.queue.all()) != 0 {
		t.Fatal("published while the sibling still polls and the grace has not elapsed")
	}
	h.c.Tick(ctx, now+1_000) // inside grace, b still polling → still held
	if len(h.queue.all()) != 0 {
		t.Fatal("published inside the grace window")
	}
	h.c.Tick(ctx, now+3_000) // grace (2.5s) elapsed → a publishes alone
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1 (a alone)", len(events))
	}
	if !strings.Contains(events[0].args.Summary, "asy_a") || strings.Contains(events[0].args.Summary, "asy_b") {
		t.Errorf("summary %q should cover only asy_a", events[0].args.Summary)
	}
	// The grace-based settle (no observed working phase) travels as an honest
	// verification nudge, not gospel.
	if !strings.Contains(events[0].args.Summary, "verify the output") {
		t.Errorf("grace-settled summary %q should carry the verify annotation", events[0].args.Summary)
	}
	if h.c.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (b still polling)", h.c.ActiveCount())
	}
}

func TestCoordinatorExpiryPublishesExpired(t *testing.T) {
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "working"}}),
	})
	rec := inv("asy_x", "run_x", 1_000, 5_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // working
	h.c.Tick(ctx, 6_000) // past expiresAt → settles expired (settleAt 8500)
	h.c.Tick(ctx, 9_000) // grace elapsed → publish
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if !strings.Contains(events[0].args.Title, "timed out") {
		t.Errorf("title %q should say timed out", events[0].args.Title)
	}
	if !strings.Contains(events[0].args.Summary, "still working") {
		t.Errorf("summary %q should mark the unsettled terminal", events[0].args.Summary)
	}
	if got := h.store.lastStatus("asy_x"); got != string(domain.AsyncExpired) {
		t.Errorf("final status = %q, want expired", got)
	}
}

func TestCoordinatorLostClaimDropsWithoutPublish(t *testing.T) {
	// async.cancel finalized the row first: the settling claim fails → the
	// coordinator must drop the invocation silently (no event, no notify).
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}}),
	})
	h.store.refuse["asy_c"] = true
	rec := inv("asy_c", "run_c", 1_000, 100_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000)
	h.c.Tick(ctx, 3_000)
	if len(h.queue.all()) != 0 {
		t.Fatal("published a completion for a cancelled invocation")
	}
	if h.c.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0 (dropped)", h.c.ActiveCount())
	}
	if *h.notify != 0 {
		t.Errorf("notify called %d times, want 0", *h.notify)
	}
}

func TestCoordinatorCancelDuringGraceDropsOnlyThatMember(t *testing.T) {
	// Both group members settle; member a is cancelled during the coalescing
	// window (its finalize claim loses). Finalize-before-publish means the event
	// covers ONLY b — never a completion wake for cancelled work.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}, "term-2": {AgentState: "working"}}),
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}, "term-2": {AgentState: "completed"}}),
	})
	a := inv("asy_a", "run_g", 1_000, 900_000)
	b := inv("asy_b", "run_g", 1_100, 900_000)
	b.ID, b.Title, b.TerminalIdsJson = "asy_b", "job asy_b", `["term-2"]`
	if err := h.c.Register(a, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Register(b, []string{"term-2"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // a settles → settling (settleAt 4500); b still polling → grace holds
	if len(h.queue.all()) != 0 {
		t.Fatal("published early")
	}
	// async.cancel wins a's row during the window: further claims on it lose.
	h.store.refuse["asy_a"] = true
	h.c.Tick(ctx, 3_000) // b settles (settleAt 5500) — grace still holds
	h.c.Tick(ctx, 4_600) // a's settleAt passed → finalize: a loses, b publishes
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if strings.Contains(events[0].args.Summary, "asy_a") {
		t.Errorf("cancelled member leaked into the event: %q", events[0].args.Summary)
	}
	if !strings.Contains(events[0].args.Summary, "asy_b") {
		t.Errorf("surviving member missing from the event: %q", events[0].args.Summary)
	}
	if h.c.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", h.c.ActiveCount())
	}
}

func TestCoordinatorClaimErrorRetriesNextTick(t *testing.T) {
	// A transient storage error on the settling claim must NOT drop the
	// invocation — outcomes stay latched and the next tick retries the write.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}}),
	})
	h.store.errClaims = 1
	rec := inv("asy_r", "run_r", 1_000, 100_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // settle → claim errors → stays tracked
	if h.c.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d after claim error, want 1 (retry)", h.c.ActiveCount())
	}
	if len(h.queue.all()) != 0 {
		t.Fatal("published despite the failed settling claim")
	}
	h.c.Tick(ctx, 3_000) // store recovered → settle (settleAt 5500)
	h.c.Tick(ctx, 6_000) // grace elapsed → publish
	if len(h.queue.all()) != 1 {
		t.Fatalf("published %d events after recovery, want 1", len(h.queue.all()))
	}
	if got := h.store.lastStatus("asy_r"); got != string(domain.AsyncSucceeded) {
		t.Errorf("final status = %q, want succeeded", got)
	}
}

func TestCoordinatorPublishFailureRetriesWithoutRefinalizing(t *testing.T) {
	// A publish failure after finalize keeps the member tracked as finalized:
	// the next tick retries ONLY the publish (one event total, one finalize).
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}}),
	})
	h.queue.failNext = 1
	rec := inv("asy_p", "run_p", 1_000, 100_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // settle (settleAt 4500)
	h.c.Tick(ctx, 4_600) // due → finalize OK, publish fails → stays tracked
	if len(h.queue.all()) != 0 {
		t.Fatal("event recorded despite publish failure")
	}
	if h.c.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1 (publish retry pending)", h.c.ActiveCount())
	}
	h.c.Tick(ctx, 5_600) // publish retried and succeeds
	if len(h.queue.all()) != 1 {
		t.Fatalf("published %d events, want 1", len(h.queue.all()))
	}
	// Exactly one terminal-status claim (settling) + one finalize claim — the
	// retry must not re-claim a row it already finalized (it would lose to its
	// own write and drop the completion).
	if got := h.store.lastStatus("asy_p"); got != string(domain.AsyncSucceeded) {
		t.Errorf("final status = %q, want succeeded", got)
	}
	if *h.notify != 1 {
		t.Errorf("notify = %d, want 1", *h.notify)
	}
}

func TestCoordinatorRosterConfirmsGoneTerminal(t *testing.T) {
	// A terminal missing from getStatus settles ONLY once the roster confirms it
	// is gone — then it completes as finished with the gone annotation.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{}),
	})
	h.reader.roster = []string{"term-other"} // term-1 not listed → confirmed gone
	rec := inv("asy_g", "run_g", 1_000, 900_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 20_000) // missing + roster check due → gone → settle (settleAt 22500)
	h.c.Tick(ctx, 23_000) // grace elapsed → publish
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if !strings.Contains(events[0].args.Summary, "terminal is gone") {
		t.Errorf("summary %q should carry the gone annotation", events[0].args.Summary)
	}
}

func TestCoordinatorMissingWithoutRosterProofKeepsPolling(t *testing.T) {
	// Missing from getStatus but still on the roster (or roster unreadable) is a
	// partial read, NOT an exit: the invocation keeps polling — even long past
	// the grace — instead of fabricating a finish.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{}),
	})
	h.reader.roster = []string{"term-1"} // still listed → absence unproven
	rec := inv("asy_m", "run_m", 1_000, 900_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, domain.FinishSettleGraceMS+10_000)
	if got := h.store.lastStatus("asy_m"); got != "" {
		t.Fatalf("transitioned to %q on an unproven absence", got)
	}
	if h.c.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1", h.c.ActiveCount())
	}
}

func TestCoordinatorRunAsyncUsesLongerGrace(t *testing.T) {
	// terminal.run.async sends to an agent that is usually ALREADY waiting: a
	// never-seen-working idle must NOT settle at the shared 20s grace (the
	// command may simply not have been picked up); it settles only past the
	// longer run.async grace, annotated for verification.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "waiting"}}),
	})
	rec := inv("asy_ra", "run_ra", 1_000, 900_000)
	rec.ToolName = "terminal.run.async"
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 1_000+domain.FinishSettleGraceMS+5_000) // past 20s, before 60s
	if got := h.store.lastStatus("asy_ra"); got != "" {
		t.Fatalf("run.async settled at the shared grace (%q) — must wait for the longer one", got)
	}
	h.c.Tick(ctx, 1_000+runAsyncNeverWorkedGraceMS+2_000) // past the run.async grace → settles
	h.c.Tick(ctx, 1_000+runAsyncNeverWorkedGraceMS+5_000) // coalescing grace elapsed → publish
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if !strings.Contains(events[0].args.Summary, "verify the output") {
		t.Errorf("summary %q should carry the never-worked verification note", events[0].args.Summary)
	}
}

func TestCoordinatorSelfGroupedPublishesWithoutGrace(t *testing.T) {
	// A self-grouped invocation (no run id — GroupID == its own id) has no
	// possible siblings, so it publishes the same pass it settles: waiting out
	// the grace would coalesce nothing and only delay the wake.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}}),
	})
	rec := inv("asy_s", "", 1_000, 900_000)
	rec.GroupID = "asy_s" // self-grouped (what storage stamps when no run id)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	h.c.Tick(context.Background(), 2_000)
	if len(h.queue.all()) != 1 {
		t.Fatalf("published %d events, want 1 (same-pass fast path)", len(h.queue.all()))
	}
}

func TestCoordinatorLateSiblingStillCoalesces(t *testing.T) {
	// The first invocation settles instantly (terminal already completed) BEFORE
	// its same-turn sibling registers (sequential dispatch + a confirmation can
	// sit between the two starters). The run-scoped grace holds the publish, so
	// the late sibling still lands in the SAME wake event.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}, "term-2": {AgentState: "completed"}}),
	})
	a := inv("asy_a", "run_g", 1_000, 900_000)
	if err := h.c.Register(a, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 1_500) // a settles alone (settleAt 4000) — grace holds the publish
	if len(h.queue.all()) != 0 {
		t.Fatal("published before the late sibling could register")
	}
	b := inv("asy_b", "run_g", 2_000, 900_000)
	b.ID, b.Title, b.TerminalIdsJson = "asy_b", "job asy_b", `["term-2"]`
	if err := h.c.Register(b, []string{"term-2"}); err != nil {
		t.Fatal(err)
	}
	h.c.Tick(ctx, 3_000) // b settles (settleAt 5500)
	h.c.Tick(ctx, 4_100) // a's settleAt passed → both publish together
	events := h.queue.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1 grouped", len(events))
	}
	if !strings.Contains(events[0].args.Summary, "asy_a") || !strings.Contains(events[0].args.Summary, "asy_b") {
		t.Errorf("grouped summary %q should cover both siblings", events[0].args.Summary)
	}
}

func TestCoordinatorCancelAllRetractsInFlightPublish(t *testing.T) {
	// /clear lands BETWEEN the finalize claims and the publish (the one window
	// the claim guard cannot cover): the generation re-check must drop the event
	// so the cleared conversation is never woken.
	h := newHarness([]StatusReadResult{
		frame(true, map[string]TerminalStatus{"term-1": {AgentState: "completed"}}),
	})
	rec := inv("asy_cl", "run_cl", 1_000, 900_000)
	if err := h.c.Register(rec, []string{"term-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.c.Tick(ctx, 2_000) // settles (settleAt 4500)
	// Arm the race: the FINALIZE claim (the next successful claim) triggers
	// CancelAll before the pass reaches Queue.Publish.
	h.store.onClaim = func(id string, patch map[string]any) {
		if st, _ := patch["status"].(string); st == string(domain.AsyncSucceeded) {
			h.c.CancelAll()
		}
	}
	h.c.Tick(ctx, 4_600) // due → finalize fires the clear → publish retracted
	if len(h.queue.all()) != 0 {
		t.Fatal("a completion was published for work /clear dropped")
	}
	if *h.notify != 0 {
		t.Errorf("notify = %d, want 0", *h.notify)
	}
	if h.c.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0 after CancelAll", h.c.ActiveCount())
	}
}

func TestCoordinatorRegisterRequiresStarted(t *testing.T) {
	h := newHarness(nil)
	h.c.stateMu.Lock()
	h.c.started = false
	h.c.stateMu.Unlock()
	if err := h.c.Register(inv("asy_n", "", 1, 2), []string{"term-1"}); err == nil {
		t.Fatal("Register accepted work while the coordinator was not started")
	}
}
