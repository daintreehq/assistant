package mcp

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ctxParkingLow blocks in CallTool/ListTools until the context it is handed is done,
// then returns that context's error. This reproduces the ONLY shape in which the
// caller-deadline bug is reachable: the caller's budget must still be live when the
// call starts (an already-expired context fast-fails in governor.acquire and never
// reaches the degrade path) and must expire while the attempt is on the wire.
// armed is off during Connect (whose own tool discovery would otherwise park
// forever on a context that never completes) and switched on for the call under test.
//
// entered counts how many times an ARMED call actually reached the fake. Without it
// these tests can pass VACUOUSLY: if a loaded machine makes governor.acquire outlast
// the caller budget, the call fast-fails in the queue, never reaches the degrade
// path, and "still connected" is trivially true. Asserting entered>0 turns that
// silent loss of coverage into a visible failure.
type ctxParkingLow struct {
	*fakeLow
	armed   bool
	mu      sync.Mutex
	entered int
}

func (b *ctxParkingLow) enter() {
	b.mu.Lock()
	b.entered++
	b.mu.Unlock()
}

func (b *ctxParkingLow) entries() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entered
}

func (b *ctxParkingLow) CallTool(ctx context.Context, _ string, _ map[string]any) (rawResult, error) {
	if !b.armed {
		return rawResult{}, nil
	}
	b.enter()
	<-ctx.Done()
	return rawResult{}, ctx.Err()
}

func (b *ctxParkingLow) ListTools(ctx context.Context) ([]rawTool, error) {
	if !b.armed {
		return nil, nil
	}
	b.enter()
	<-ctx.Done()
	return nil, ctx.Err()
}

func newCtxParkingClient() (*Client, *ctxParkingLow) {
	low := &ctxParkingLow{fakeLow: &fakeLow{}}
	c := newInjected(low)
	// Drop the governor's 50ms pacing for this injected client. With pacing on, a
	// loaded machine can burn the caller budget in the ACQUIRE QUEUE, so the call
	// never reaches the transport and the test proves nothing (the entries() guard
	// below would then turn that into a flaky failure rather than a silent pass).
	c.gov = newGovernor(governorMaxConcurrent, 0)
	c.Connect(context.Background())
	low.armed = true
	return c, low
}

// The degrade decision must key on whether the CALLER's context is finished — for
// ANY reason — not merely on context.Canceled.
//
// markDegraded is process-wide and brutal: it nulls c.low, clears connected, and
// Closes the transport for every other consumer (every watcher, the async
// coordinator's 1s poll, any interactive turn). A DeadlineExceeded that came from a
// CALLER's own budget is not evidence the transport is unhealthy — the server may be
// perfectly alive and merely slower than that one caller was willing to wait.
//
// The failure this pins was self-inflicted and reachable from two live call sites:
// /doctor bounded its probes with a 5s context.WithTimeout against the attached session's
// LIVE client (whose own per-attempt budget is 20s), and the scheduler bounded each
// job with a 120s context.WithTimeout threaded into every watcher MCP read. In both
// cases a slow-but-alive server made the caller's deadline fire mid-attempt and
// degraded the shared session — so /doctor could cause the very outage it reported.
//
// Retries are pinned to 0 so the call lands on the degrade path directly. With
// retries left, a DeadlineExceeded is absorbed earlier by abortableSleep (which
// propagates the context error without degrading) — that branch masks the bug
// rather than fixing it, which is why the guard belongs at the degrade site.
func TestCallToolCallerDeadlineDoesNotDegrade(t *testing.T) {
	c, low := newCtxParkingClient()

	// The budget must exceed the governor's 50ms pacing interval: a shorter one
	// expires while the call is still queued in governor.acquire, which returns the
	// context error BEFORE any network attempt and never reaches the degrade path
	// at all (that shape is already safe, and would make this test vacuous).
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	if _, err := c.CallTool(ctx, "actions.getContext", nil, CallOptions{Retries: 0}); err == nil {
		t.Fatal("expected the deadline-exceeded call to surface an error")
	}
	if low.entries() == 0 {
		t.Fatal("the call never reached the transport (governor queue outlasted the budget) — this test proved nothing")
	}
	if !c.IsConnected() {
		t.Error("a CALLER-supplied deadline must NOT degrade the shared connection")
	}
}

// The listTools degrade path carries the same rule. force=true is required to reach
// it at all: listTools is cache-first, so a warm cache short-circuits before the
// wire. That caching is why /doctor's own ListTools(ctx, false) is usually safe —
// but a cold or force-refreshed list on a live client goes straight down this path,
// and it must not degrade on a caller-side budget either.
func TestListToolsCallerDeadlineDoesNotDegrade(t *testing.T) {
	c, low := newCtxParkingClient()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	if _, err := c.ListTools(ctx, true); err == nil {
		t.Fatal("expected the deadline-exceeded list to surface an error")
	}
	if low.entries() == 0 {
		t.Fatal("the call never reached the transport (governor queue outlasted the budget) — this test proved nothing")
	}
	if !c.IsConnected() {
		t.Error("a CALLER-supplied deadline must NOT degrade the shared connection")
	}
}

// The counterpart contract, restated so the fix above can't be "simplified" into
// never degrading at all: a transport failure that arrives while the caller's
// context is still CLEAN is real evidence the connection is wedged (the client's
// own per-attempt deadline, or a genuine transport error), and it MUST degrade so
// the reconnect path runs.
func TestCallToolTransportFailureStillDegrades(t *testing.T) {
	low := &fakeLow{callErrs: []error{context.DeadlineExceeded}}
	c := newInjected(low)
	c.Connect(context.Background())

	// Caller context is clean; only the (fake) per-attempt call failed.
	if _, err := c.CallTool(context.Background(), "actions.getContext", nil, CallOptions{Retries: 0}); err == nil {
		t.Fatal("expected the failing call to surface an error")
	}
	if c.IsConnected() {
		t.Error("a transport failure under a CLEAN caller context must still degrade")
	}
}
