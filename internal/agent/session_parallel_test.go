package agent

import (
	"context"
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
	parallelNames map[string]bool    // internal names classified read-only (ParallelSafe)
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
	// A serial (non-parallel-safe) call returns straight away: it runs alone, so waiting
	// for `expect` peers would deadlock.
	if !t.parallelNames[name] {
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
				toolCall("w", "agentTask__spawnForEdits", `{}`),
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
// group deterministically, exactly like the old serial loop: three byte-identical
// failing reads in ONE batch reach the abort threshold (RepeatFailureAbort) and stop
// the turn, regardless of which finished first.
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

// TestCoarseBreakerAbortsArgVariedUnrecoverableLoop is the regression for the real
// meltdown: the model paged a pruned artifact — same tool, same UNRECOVERABLE error
// (ARTIFACT_NOT_FOUND), but a NEW offset each call. Every call has a distinct fine
// (tool+args+code) signature, so the exact-args breaker never trips; the turn burned
// dozens of failing calls until the user cancelled. The coarse breaker (tool+code,
// unrecoverable only) must catch it at CoarseRepeatFailureAbort.
func TestCoarseBreakerAbortsArgVariedUnrecoverableLoop(t *testing.T) {
	gone := domain.Fail("ARTIFACT_NOT_FOUND", "No artifact found; pruned by retention.", domain.Unrecoverable())
	tools := &fakeTools{result: gone}
	// One batch, CoarseRepeatFailureAbort calls, each a DIFFERENT offset → distinct fine
	// signatures, identical coarse signature.
	calls := make([]models.ToolCallRequest, domain.CoarseRepeatFailureAbort)
	for i := range calls {
		calls[i] = toolCall("c"+itoa(i), "artifact__read", `{"artifactId":"artifact-x","offset":`+itoa(i*3500)+`}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Stopped: called ") || !strings.Contains(reply, "unrecoverable") {
		t.Fatalf("expected coarse unrecoverable-loop abort, got %q", reply)
	}
	if !IsWakeFailureReply(reply) {
		t.Fatal("coarse breaker reply must be a wake-failure sentinel")
	}
}

// TestCoarseBreakerIgnoresRecoverableErrors guards against over-aborting: a RECOVERABLE
// error repeated with varied args (e.g. a transient rate limit across several reads)
// must NOT trip the coarse breaker — the model may legitimately retry transient
// failures, so the turn proceeds.
func TestCoarseBreakerIgnoresRecoverableErrors(t *testing.T) {
	transient := domain.Fail("MCP_RATE_LIMITED", "slow down") // Recoverable defaults true
	tools := &fakeTools{result: transient}
	calls := make([]models.ToolCallRequest, domain.CoarseRepeatFailureAbort+2)
	for i := range calls {
		calls[i] = toolCall("c"+itoa(i), "terminal__read", `{"terminalId":"t`+itoa(i)+`"}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("recoverable errors must not trip the coarse breaker; got %q", reply)
	}
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
}
