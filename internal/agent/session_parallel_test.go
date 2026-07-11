package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// parallelFakeTools is a ToolRunner that ALSO implements the optional
// parallelSafeRunner capability, so runToolBatch dispatches its read-only calls
// concurrently. Each parallel-safe Dispatch blocks on a shared gate until `expect` of
// them are in flight at once — proving they truly overlap — then all release;
// non-parallel calls return immediately (the serial path, so they never wait for
// peers that will never arrive). maxInFlight is the peak concurrency observed. All
// shared state is mutex/Once-guarded so the fake is race-clean under `go test -race`.
type parallelFakeTools struct {
	parallelNames map[string]bool // internal names classified read-only (ParallelSafe)
	// mutationNames are the internal names classified as pre-authorized homogeneous
	// mutations (ParallelMutationSafe); conflictKey is the optional per-call
	// independence classifier (nil ⇒ every call independent). The fake always
	// implements parallelMutationRunner — an empty mutationNames map keeps every
	// mutating call on the serial path, mirroring a tool without the opt-in.
	mutationNames map[string]bool
	conflictKey   func(name string, args json.RawMessage) ([]string, bool)
	expect        int                // barrier size: releases once this many reads overlap
	failWith      *domain.ToolResult // when set, every dispatch returns this (else Ok)

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	dispatched  int
	seen        []string // dispatch ARRIVAL order (nondeterministic within a group)

	gate     chan struct{}
	gateOnce sync.Once
}

func (t *parallelFakeTools) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (t *parallelFakeTools) ResolveWireName(w string) string                 { return strings.ReplaceAll(w, "__", ".") }
func (t *parallelFakeTools) ParallelSafe(name string) bool                   { return t.parallelNames[name] }
func (t *parallelFakeTools) ParallelMutationSafe(name string) bool           { return t.mutationNames[name] }

func (t *parallelFakeTools) ParallelConflictKey(name string, args json.RawMessage) ([]string, bool) {
	if t.conflictKey != nil {
		return t.conflictKey(name, args)
	}
	return nil, true
}

func (t *parallelFakeTools) resultFor(name string) domain.ToolResult {
	if t.failWith != nil {
		return *t.failWith
	}
	return domain.Ok("ok:"+name, nil)
}

func (t *parallelFakeTools) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.mu.Lock()
	t.dispatched++
	t.seen = append(t.seen, name)
	// A serial (non-groupable) call returns straight away: it runs alone, so waiting
	// for `expect` peers would deadlock.
	if !t.parallelNames[name] && !t.mutationNames[name] {
		t.mu.Unlock()
		return t.resultFor(name)
	}
	t.inFlight++
	if t.inFlight > t.maxInFlight {
		t.maxInFlight = t.inFlight
	}
	reached := t.inFlight >= t.expect
	t.mu.Unlock()

	if reached {
		t.gateOnce.Do(func() { close(t.gate) })
	}
	// Block until the barrier fills (all peers arrived) — with a timeout so a serial
	// REGRESSION fails the maxInFlight assertion instead of hanging the suite forever.
	select {
	case <-t.gate:
	case <-ctx.Done():
	case <-time.After(time.Second):
	}

	t.mu.Lock()
	t.inFlight--
	t.mu.Unlock()
	return t.resultFor(name)
}

// assertToolOrder checks the transcript's tool replies appear in exactly wantIDs order
// — the ordered bookkeeping must be deterministic (call order) even though the calls
// finished in an arbitrary order.
func assertToolOrder(t *testing.T, msgs []models.ChatMessage, wantIDs []string) {
	t.Helper()
	var got []string
	for _, m := range msgs {
		if m.Role == "tool" {
			got = append(got, m.ToolCallID)
		}
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("tool replies = %v, want order %v", got, wantIDs)
	}
	for i := range wantIDs {
		if got[i] != wantIDs[i] {
			t.Fatalf("tool reply order = %v, want %v", got, wantIDs)
		}
	}
}

// TestReadOnlyBatchDispatchesConcurrently is the core win: a batch of read-only calls
// (e.g. several terminal.extract summaries) runs in parallel, not one-at-a-time, so N
// backend round-trips collapse into ~one wall-clock wait.
func TestReadOnlyBatchDispatchesConcurrently(t *testing.T) {
	tools := &parallelFakeTools{
		parallelNames: map[string]bool{"terminal.extract": true},
		expect:        3,
		gate:          make(chan struct{}),
	}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{
				toolCall("a", "terminal__extract", `{"terminalIds":["t1"]}`),
				toolCall("b", "terminal__extract", `{"terminalIds":["t2"]}`),
				toolCall("c", "terminal__extract", `{"terminalIds":["t3"]}`),
			}},
			{Content: "final"},
		},
	}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.maxInFlight != 3 {
		t.Fatalf("peak concurrent dispatch = %d, want 3 (a read-only batch must run in parallel)", tools.maxInFlight)
	}
	if tools.dispatched != 3 {
		t.Fatalf("dispatched %d calls, want 3", tools.dispatched)
	}
	// Transcript still records all three replies in CALL order, whichever finished first.
	assertToolOrder(t, s.Messages(), []string{"a", "b", "c"})
}

// TestMixedBatchKeepsNonReadCallsSerial guards the safety boundary: a mutating call in
// the middle of a batch splits the parallel run. Only the leading contiguous run of
// reads overlaps; the write is a hard serial boundary and the trailing lone read is
// dispatched serially — so mutating/confirming tools keep today's ordering.
func TestMixedBatchKeepsNonReadCallsSerial(t *testing.T) {
	tools := &parallelFakeTools{
		// Only extract is read-only; the spawn is a mutating call (serial).
		parallelNames: map[string]bool{"terminal.extract": true},
		expect:        2, // the leading read run is the only group that overlaps
		gate:          make(chan struct{}),
	}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{
				toolCall("a", "terminal__extract", `{"terminalIds":["t1"]}`),
				toolCall("b", "terminal__extract", `{"terminalIds":["t2"]}`),
				toolCall("w", "agentTask__spawnForEdits", `{"task":"x"}`),
				toolCall("c", "terminal__extract", `{"terminalIds":["t3"]}`),
			}},
			{Content: "final"},
		},
	}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Peak concurrency is 2 (leading reads), never 3 — the write blocks a-b from merging
	// with c, and c is a lone read on the serial path.
	if tools.maxInFlight != 2 {
		t.Fatalf("peak concurrent dispatch = %d, want 2 (a mutating call must split the read run)", tools.maxInFlight)
	}
	if tools.dispatched != 4 {
		t.Fatalf("dispatched %d calls, want 4", tools.dispatched)
	}
	assertToolOrder(t, s.Messages(), []string{"a", "b", "w", "c"})
}

// TestParallelGroupBoundedBySemaphore proves the fan-out cap holds: with more
// read-only calls than maxParallelToolDispatch, peak concurrency never exceeds the cap
// (so a huge extract batch can't burst the Daintree MCP getOutput throttle), yet every
// call still runs.
func TestParallelGroupBoundedBySemaphore(t *testing.T) {
	nCalls := maxParallelToolDispatch + 2
	tools := &parallelFakeTools{
		parallelNames: map[string]bool{"terminal.extract": true},
		expect:        maxParallelToolDispatch, // gate opens once the cap is saturated
		gate:          make(chan struct{}),
	}
	calls := make([]models.ToolCallRequest, nCalls)
	for i := range calls {
		calls[i] = toolCall(itoa(i), "terminal__extract", `{"terminalIds":["t`+itoa(i)+`"]}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.maxInFlight != maxParallelToolDispatch {
		t.Fatalf("peak concurrent dispatch = %d, want exactly %d (semaphore must cap the fan-out)", tools.maxInFlight, maxParallelToolDispatch)
	}
	if tools.dispatched != nCalls {
		t.Fatalf("dispatched %d calls, want %d (every call must still run under the cap)", tools.dispatched, nCalls)
	}
}

// TestParallelGroupCircuitBreakerFold proves the circuit breaker folds a concurrent
// group deterministically, exactly like the serial loop: three byte-identical failing
// reads in ONE batch reach the abort threshold (RepeatFailureAbort) at the group
// boundary and stop the turn, regardless of which finished first.
func TestParallelGroupCircuitBreakerFold(t *testing.T) {
	failing := domain.Fail("BOOM", "it broke", domain.Unrecoverable())
	tools := &parallelFakeTools{
		parallelNames: map[string]bool{"terminal.extract": true},
		expect:        domain.RepeatFailureAbort,
		gate:          make(chan struct{}),
		failWith:      &failing,
	}
	// Three identical calls (same tool + same args) → same failure signature → the
	// per-signature counter folds to 3 across the group.
	same := `{"terminalIds":["t1"]}`
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{
			toolCall("a", "terminal__extract", same),
			toolCall("b", "terminal__extract", same),
			toolCall("c", "terminal__extract", same),
		}},
		{Content: "final"},
	}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Stopped: called ") {
		t.Fatalf("expected circuit-breaker abort from the folded group, got %q", reply)
	}
	if tools.dispatched != domain.RepeatFailureAbort {
		t.Fatalf("dispatched %d times, want %d (all folded in one batch)", tools.dispatched, domain.RepeatFailureAbort)
	}
}

// TestQuestionBatchStaysSerial guards the ask-alone rule against the parallel path: a
// batch that carries a user.askMultipleChoice must NOT gather its extract siblings
// into a concurrent group — they are synthetic skips that must never dispatch, so only
// the question itself reaches the runner.
func TestQuestionBatchStaysSerial(t *testing.T) {
	tools := &parallelFakeTools{
		parallelNames: map[string]bool{"terminal.extract": true},
		expect:        1,
		gate:          make(chan struct{}),
	}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{
			toolCall("a", "terminal__extract", `{"terminalIds":["t1"]}`),
			toolCall("q", "user__askMultipleChoice", `{"question":"pick","options":[{"text":"x"},{"text":"y"}]}`),
			toolCall("b", "terminal__extract", `{"terminalIds":["t2"]}`),
		}},
		{Content: "final"},
	}}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.dispatched != 1 {
		t.Fatalf("dispatched %d calls, want 1 (only the question; skipped siblings must never dispatch)", tools.dispatched)
	}
	if len(tools.seen) != 1 || tools.seen[0] != questionToolName {
		t.Fatalf("dispatched %v, want only %q", tools.seen, questionToolName)
	}
	assertToolOrder(t, s.Messages(), []string{"a", "q", "b"})
}

// streamSink records ToolResult events (thread-safe) and signals each settled call ID
// on a buffered channel, so a test worker can block until a sibling's result has been
// EMITTED — proving completions stream live instead of flushing after the whole group.
type streamSink struct {
	NoopEventSink
	mu      sync.Mutex
	results []ToolResultEvent
	settled chan string
}

func (s *streamSink) ToolResult(ev ToolResultEvent) {
	s.mu.Lock()
	s.results = append(s.results, ev)
	s.mu.Unlock()
	s.settled <- ev.ID
}

func (s *streamSink) resultsByID() map[string]ToolResultEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ToolResultEvent, len(s.results))
	for _, ev := range s.results {
		out[ev.ID] = ev
	}
	return out
}

// firstFinisherTools completes the call on `fastID` immediately; every other
// parallel-safe call blocks until the sink reports fastID's ToolResult event was
// emitted (or times out, failing the assertions rather than hanging).
type firstFinisherTools struct {
	parallelNames map[string]bool
	fastID        string
	sink          *streamSink

	mu        sync.Mutex
	unblocked bool
}

func (t *firstFinisherTools) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (t *firstFinisherTools) ResolveWireName(w string) string {
	return strings.ReplaceAll(w, "__", ".")
}
func (t *firstFinisherTools) ParallelSafe(name string) bool { return t.parallelNames[name] }

func (t *firstFinisherTools) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	if turn.CallID == t.fastID {
		return domain.Ok("fast", nil)
	}
	// Wait until the FAST call's result event has been emitted by the turn goroutine.
	// If completions were only flushed after the whole group (the wg.Wait() shape),
	// this would time out — the turn goroutine would still be waiting for THIS call.
	deadline := time.After(time.Second)
	for {
		select {
		case id := <-t.sink.settled:
			if id == t.fastID {
				t.mu.Lock()
				t.unblocked = true
				t.mu.Unlock()
				return domain.Ok("slow", nil)
			}
		case <-deadline:
			return domain.Fail("TIMEOUT", "fast sibling's result was never streamed")
		case <-ctx.Done():
			return domain.Fail("CANCELLED", "ctx done")
		}
	}
}

// TestParallelGroupStreamsCompletionsLive is the live-settle contract: a parallel
// group must emit each member's ToolResult the moment it completes — the fast member's
// ✓ appears while the slow member is still running — and every event carries the
// member's OWN completion time, not the group's (the slow row's EndedAt is later than
// the fast row's).
func TestParallelGroupStreamsCompletionsLive(t *testing.T) {
	sink := &streamSink{settled: make(chan string, 8)}
	tools := &firstFinisherTools{
		parallelNames: map[string]bool{"terminal.extract": true},
		fastID:        "fast",
		sink:          sink,
	}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{
			toolCall("fast", "terminal__extract", `{"terminalIds":["t1"]}`),
			toolCall("slow", "terminal__extract", `{"terminalIds":["t2"]}`),
		}},
		{Content: "final"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want %q", reply, "final")
	}
	tools.mu.Lock()
	unblocked := tools.unblocked
	tools.mu.Unlock()
	if !unblocked {
		t.Fatal("slow call never saw the fast call's streamed ToolResult — completions are not emitted live")
	}
	byID := sink.resultsByID()
	fastEv, ok1 := byID["fast"]
	slowEv, ok2 := byID["slow"]
	if !ok1 || !ok2 {
		t.Fatalf("missing ToolResult events: %v", byID)
	}
	if !fastEv.Result.Ok || !slowEv.Result.Ok {
		t.Fatalf("both members must succeed: fast=%v slow=%v", fastEv.Result, slowEv.Result)
	}
	// Per-member endedAt: the slow member settled strictly after the fast one, so its
	// event must carry a LATER (or equal, at ms resolution) completion time — not the
	// single post-group timestamp the old wg.Wait() shape stamped on every row.
	if slowEv.EndedAt < fastEv.EndedAt {
		t.Fatalf("slow EndedAt (%d) predates fast EndedAt (%d) — endedAt must be captured per member at completion", slowEv.EndedAt, fastEv.EndedAt)
	}
	// And the transcript still lists fast before slow (call order), regardless of timing.
	assertToolOrder(t, s.Messages(), []string{"fast", "slow"})
}

// TestWaitBearingExtractNotParallelSafe guards the barrier boundary on an OPTED-IN tool:
// terminal.extract is Parallelizable, but an extract carrying a `wait` condition is a
// BARRIER (it polls until the terminal settles), so a later call in the batch may depend
// on it — it must run serially. Only a pure no-wait snapshot read parallelizes.
func TestWaitBearingExtractNotParallelSafe(t *testing.T) {
	tools := &parallelFakeTools{parallelNames: map[string]bool{"terminal.extract": true}}
	s := NewSession(baseDeps(&fakeRouter{}, tools))

	plain := toolCall("a", "terminal__extract", `{"terminalIds":["t1"],"instruction":"read the answer"}`)
	if !s.callParallelSafe(plain, nil) {
		t.Error("a plain no-wait extract must be parallel-safe")
	}
	withWait := toolCall("b", "terminal__extract", `{"terminalIds":["t1"],"wait":{"noOutputForMs":1000}}`)
	if s.callParallelSafe(withWait, nil) {
		t.Error("a wait-bearing extract is a barrier and must NOT be parallel-safe")
	}
	// A null wait is not a barrier.
	nullWait := toolCall("c", "terminal__extract", `{"terminalIds":["t1"],"instruction":"x","wait":null}`)
	if !s.callParallelSafe(nullWait, nil) {
		t.Error(`"wait":null is not a barrier and must stay parallel-safe`)
	}
	// Degenerate args stay serial: the truncation/invalid-args paths must keep their
	// exact ordering, so an empty or {} call never joins a group.
	empty := toolCall("d", "terminal__extract", `{}`)
	if s.callParallelSafe(empty, nil) {
		t.Error("empty args must not be parallel-safe (serial truncation/validation path)")
	}
}

// spawnCall builds a spawn-shaped tool call for the mutation-cohort tests.
func spawnCall(id, args string) models.ToolCallRequest {
	return toolCall(id, "agentTask__spawnForEdits", args)
}

// TestSpawnFanOutDispatchesConcurrently is the mutation-cohort win: a batch of
// same-name, pre-authorized, independent spawn calls runs as one concurrent cohort
// instead of N serial ~5s launches — while the transcript still folds in call order.
func TestSpawnFanOutDispatchesConcurrently(t *testing.T) {
	tools := &parallelFakeTools{
		mutationNames: map[string]bool{"agentTask.spawnForEdits": true},
		expect:        3,
		gate:          make(chan struct{}),
	}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{
			spawnCall("a", `{"title":"one","taskPrompt":"p1"}`),
			spawnCall("b", `{"title":"two","taskPrompt":"p2"}`),
			spawnCall("c", `{"title":"three","taskPrompt":"p3"}`),
		}},
		{Content: "final"},
	}}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.maxInFlight != 3 {
		t.Fatalf("peak concurrent dispatch = %d, want 3 (an independent spawn fan-out must run as one cohort)", tools.maxInFlight)
	}
	if tools.dispatched != 3 {
		t.Fatalf("dispatched %d calls, want 3", tools.dispatched)
	}
	assertToolOrder(t, s.Messages(), []string{"a", "b", "c"})
}

// TestSpawnFanOutBoundedByCohortCap proves a long spawn run dispatches as
// successive ≤maxParallelMutationDispatch cohorts (the breaker/cancel cadence),
// never one unbounded group.
func TestSpawnFanOutBoundedByCohortCap(t *testing.T) {
	nCalls := maxParallelMutationDispatch + 2
	tools := &parallelFakeTools{
		mutationNames: map[string]bool{"agentTask.spawnForEdits": true},
		expect:        maxParallelMutationDispatch,
		gate:          make(chan struct{}),
	}
	calls := make([]models.ToolCallRequest, nCalls)
	for i := range calls {
		calls[i] = spawnCall(itoa(i), `{"title":"t`+itoa(i)+`","taskPrompt":"p"}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.maxInFlight != maxParallelMutationDispatch {
		t.Fatalf("peak concurrent dispatch = %d, want exactly %d (the mutation cohort cap)", tools.maxInFlight, maxParallelMutationDispatch)
	}
	if tools.dispatched != nCalls {
		t.Fatalf("dispatched %d calls, want %d (the overflow wave must still run)", tools.dispatched, nCalls)
	}
}

// TestSpawnDuplicateArgsSplitCohort guards the idempotency-saga race: a spawn whose
// canonicalized args byte-match an earlier cohort member must fall out of the group
// and run serially AFTER it, where the saga lookup sees the first insert (a clean
// idempotent hit instead of a concurrent check-then-insert race).
func TestSpawnDuplicateArgsSplitCohort(t *testing.T) {
	tools := &parallelFakeTools{
		mutationNames: map[string]bool{"agentTask.spawnForEdits": true},
		expect:        2, // only the two distinct spawns overlap
		gate:          make(chan struct{}),
	}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{
			spawnCall("a", `{"title":"same","taskPrompt":"p"}`),
			spawnCall("b", `{"title":"other","taskPrompt":"p"}`),
			// Key order differs from "a" but canonicalizes identically — still a dup.
			spawnCall("c", `{"taskPrompt":"p","title":"same"}`),
		}},
		{Content: "final"},
	}}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.maxInFlight != 2 {
		t.Fatalf("peak concurrent dispatch = %d, want 2 (the duplicate must not join the cohort)", tools.maxInFlight)
	}
	if tools.dispatched != 3 {
		t.Fatalf("dispatched %d calls, want 3 (the duplicate still runs, serially)", tools.dispatched)
	}
	assertToolOrder(t, s.Messages(), []string{"a", "b", "c"})
}

// TestMutationRunEndBoundaries pins every cohort boundary in one place: the cap, a
// name change, a duplicate, a conflict-key collision, a cohort-refusing call
// (ok=false), and a tool without the opt-in.
func TestMutationRunEndBoundaries(t *testing.T) {
	tools := &parallelFakeTools{
		mutationNames: map[string]bool{"agentTask.spawnForEdits": true},
		conflictKey: func(_ string, args json.RawMessage) ([]string, bool) {
			var m map[string]string
			if json.Unmarshal(args, &m) != nil {
				return nil, false
			}
			if m["solo"] != "" {
				return nil, false // a call that refuses cohort membership entirely
			}
			if m["wt"] != "" {
				// Two dimensions, mirroring the real spawn classifier's shape.
				return []string{"title:" + m["title"], "wt:" + m["wt"]}, true
			}
			return []string{"title:" + m["title"]}, true
		},
	}
	s := NewSession(baseDeps(&fakeRouter{}, tools))

	// Distinct independent calls group up to the cap; the overflow starts the next cohort.
	long := make([]models.ToolCallRequest, maxParallelMutationDispatch+2)
	for i := range long {
		long[i] = spawnCall(itoa(i), `{"title":"t`+itoa(i)+`"}`)
	}
	if e := s.mutationRunEnd(long, 0, nil); e != maxParallelMutationDispatch {
		t.Errorf("cap: run end = %d, want %d", e, maxParallelMutationDispatch)
	}

	// A different tool name ends the cohort (homogeneous means SAME tool only).
	mixed := []models.ToolCallRequest{
		spawnCall("a", `{"title":"1"}`),
		toolCall("x", "timer__create", `{"title":"2"}`),
		spawnCall("b", `{"title":"3"}`),
	}
	if e := s.mutationRunEnd(mixed, 0, nil); e != 1 {
		t.Errorf("name change: run end = %d, want 1", e)
	}

	// A collision on ANY conflict dimension ends the cohort; distinct keys coexist.
	conf := []models.ToolCallRequest{
		spawnCall("a", `{"title":"1","wt":"w1"}`),
		spawnCall("b", `{"title":"2","wt":"w2"}`),
		spawnCall("c", `{"title":"3","wt":"w1"}`), // distinct title, shared worktree
	}
	if e := s.mutationRunEnd(conf, 0, nil); e != 2 {
		t.Errorf("conflict collision: run end = %d, want 2", e)
	}
	// The second dimension alone also conflicts: same title, distinct worktrees.
	confTitle := []models.ToolCallRequest{
		spawnCall("a", `{"title":"same","wt":"w1"}`),
		spawnCall("b", `{"title":"same","wt":"w2"}`),
	}
	if e := s.mutationRunEnd(confTitle, 0, nil); e != 1 {
		t.Errorf("identity collision: run end = %d, want 1", e)
	}

	// ok=false keeps the call out of any cohort — even as the leading call.
	refuse := []models.ToolCallRequest{
		spawnCall("a", `{"solo":"yes"}`),
		spawnCall("b", `{"title":"2"}`),
	}
	if e := s.mutationRunEnd(refuse, 0, nil); e != 0 {
		t.Errorf("cohort-refusing lead: run end = %d, want 0", e)
	}

	// A tool without the ParallelMutationSafe opt-in never forms a cohort.
	other := []models.ToolCallRequest{
		toolCall("x", "timer__create", `{"title":"1"}`),
		toolCall("y", "timer__create", `{"title":"2"}`),
	}
	if e := s.mutationRunEnd(other, 0, nil); e != 0 {
		t.Errorf("no opt-in: run end = %d, want 0", e)
	}
}
