package mcp

// Global per-client MCP request governor.
//
// Every subsystem in the assistant talks to Daintree through ONE *mcp.Client
// (the daemon watcher ticks, the async coordinator's 1s poll, in-turn tool
// dispatch including awaitAll/extract poll loops, the cockpit's preview poller,
// boot reconcile, doctor). None of those callers coordinate with each other, so
// their independent "cheap" reads can align into a burst — e.g. ten due
// watchers fanning out on the same 3s tick while the async coordinator and the
// preview poller fire in the same instant — and a burst of concurrent requests
// is exactly what can knock over Daintree's local MCP server. The governor is
// the single choke point that makes over-pressure structurally impossible,
// regardless of how many callers exist above it:
//
//   - a small in-flight cap (slots): at most governorMaxConcurrent requests on
//     the wire at once, everything else queues;
//   - start pacing (minInterval): consecutive request STARTS are spaced out, so
//     a queued burst drains as a steady trickle instead of a thundering herd.
//
// Both waits are ctx-abortable, so a cancelled turn or a daemon item deadline
// never blocks on the queue. The slot is held only for the duration of ONE
// network attempt — retry backoff sleeps happen OUTSIDE the governor — so a
// throttled/failing read never pins capacity while it waits.

import (
	"context"
	"sync"
	"time"
)

// Governor defaults. Vars (not consts) so tests can shrink them. Sizing
// rationale: Daintree's MCP is a local single-user server — the failure mode is
// simultaneous bursts, not sustained volume. 4 in-flight keeps a watcher-tick
// fan-out from landing all at once while leaving headroom so interactive turn
// calls are never starved behind background polls; 50ms start spacing caps the
// drain rate at ~20 req/s, which a healthy local server absorbs trivially but
// which turns a 30-call burst into a >1.5s trickle.
var (
	governorMaxConcurrent = 4
	governorMinInterval   = 50 * time.Millisecond
)

// governor serializes access to one MCP server: an in-flight semaphore plus
// paced start times. One instance per Client (the primary Daintree client and
// the docs client each get their own — pressure on one server must not queue
// calls to the other).
type governor struct {
	slots       chan struct{}
	minInterval time.Duration

	mu          sync.Mutex
	nextStartAt time.Time
}

func newGovernor(maxConcurrent int, minInterval time.Duration) *governor {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if minInterval < 0 {
		minInterval = 0
	}
	return &governor{
		slots:       make(chan struct{}, maxConcurrent),
		minInterval: minInterval,
	}
}

// acquire blocks until an in-flight slot is free AND this call's paced start
// time arrives. On success the caller MUST release(). Returns ctx.Err()
// (holding nothing) when the caller is cancelled or times out while queued —
// the caller propagates it like an aborted call, never as a transport failure.
func (g *governor) acquire(ctx context.Context) error {
	// Deterministic fast-fail for an already-aborted caller: without this, the
	// select below races a free slot against ctx.Done() and an aborted caller
	// could still put one request on the wire.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if g.minInterval > 0 {
		// Reserve the next start tick under the lock, then sleep to it OUTSIDE the
		// lock. Reserving (rather than re-checking after the sleep) guarantees two
		// queued callers can never collapse onto the same start time.
		g.mu.Lock()
		now := time.Now()
		start := g.nextStartAt
		if start.Before(now) {
			start = now
		}
		g.nextStartAt = start.Add(g.minInterval)
		g.mu.Unlock()
		if wait := time.Until(start); wait > 0 {
			if err := abortableSleep(ctx, wait); err != nil {
				// The reserved start tick is deliberately NOT returned to the pacer: a
				// burst of cancelled callers leaving small holes is extra backpressure
				// in exactly the situations (slow server, expiring deadlines) where
				// slowing down further is the desired behavior. Each hole is at most
				// one minInterval, so the cost is bounded and self-limiting.
				g.release()
				return err
			}
		}
	}
	return nil
}

// release frees the slot taken by a successful acquire.
func (g *governor) release() { <-g.slots }
