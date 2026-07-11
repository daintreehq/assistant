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
// TRAFFIC CLASSES (priority.go): slots are not first-come-first-served. The
// callers above split into Interactive (a user is waiting), Refresh (useful but
// degradable to cached state — the default), and Background (maintenance polls),
// conveyed via ctx (WithPriority). Weighted fairness, not absolute priority:
//
//   - one slot is RESERVED for Interactive: Refresh/Background may together
//     hold at most capShared = capTotal-1 slots, so background polls can never
//     occupy every slot while a user-facing call queues;
//   - when capacity frees, waiting Interactive work goes to the head of the
//     queue for the shared slots too (Interactive > Refresh > Background,
//     FIFO within a class);
//   - anti-starvation: a Refresh/Background waiter queued longer than
//     governorStarvationAge gets the next shared grant even over waiting
//     Interactive work (which still has the reserved slot, so user work keeps
//     flowing) — sustained Interactive load degrades Background to a trickle
//     but can never park it forever;
//   - Interactive skips the start pacing (a human is waiting; user-driven calls
//     are sparse, and the pacer exists to trickle background bursts, which
//     remain paced).
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
// which turns a 30-call burst into a >1.5s trickle. 2s starvation age is one
// watcher tick period-ish: long enough that Background genuinely yields to a
// busy turn, short enough that a poll is late, never lost.
var (
	governorMaxConcurrent = 4
	governorMinInterval   = 50 * time.Millisecond
	governorStarvationAge = 2 * time.Second
)

// govWaiter is one queued acquire. granted is written under governor.mu; ready
// is closed (once, under mu) at grant time so the waiter can select on it
// against its ctx.
type govWaiter struct {
	class   Priority
	since   time.Time
	ready   chan struct{}
	granted bool
}

// governor serializes access to one MCP server: a class-aware in-flight
// semaphore plus paced start times. One instance per Client (the primary
// Daintree client and the docs client each get their own — pressure on one
// server must not queue calls to the other).
type governor struct {
	minInterval time.Duration

	mu sync.Mutex
	// capTotal is every in-flight slot; Interactive may use all of them.
	// capShared is the ceiling for Refresh+Background holders combined —
	// capTotal-1 when there's room to reserve, leaving one Interactive-only
	// slot (capTotal==1 has nothing to reserve without deadlocking background).
	capTotal   int
	capShared  int
	held       int // all current holders
	heldShared int // Refresh/Background holders (bounded by capShared)
	// queues holds FIFO waiters per class, indexed by Priority.
	queues      [numPriorities][]*govWaiter
	nextStartAt time.Time
}

func newGovernor(maxConcurrent int, minInterval time.Duration) *governor {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if minInterval < 0 {
		minInterval = 0
	}
	shared := maxConcurrent
	if maxConcurrent >= 2 {
		shared = maxConcurrent - 1
	}
	return &governor{
		minInterval: minInterval,
		capTotal:    maxConcurrent,
		capShared:   shared,
	}
}

// acquire blocks until an in-flight slot is granted to this caller's traffic
// class (priorityFromContext(ctx)) AND — for Refresh/Background — this call's
// paced start time arrives. On success the caller MUST invoke the returned
// release exactly once. Returns ctx.Err() (holding nothing) when the caller is
// cancelled or times out while queued — the caller propagates it like an
// aborted call, never as a transport failure.
func (g *governor) acquire(ctx context.Context) (func(), error) {
	// Deterministic fast-fail for an already-aborted caller: without this, the
	// select below races a grant against ctx.Done() and an aborted caller could
	// still put one request on the wire.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	class := priorityFromContext(ctx)
	w := &govWaiter{class: class, since: time.Now(), ready: make(chan struct{})}

	g.mu.Lock()
	g.queues[class] = append(g.queues[class], w)
	g.grantLocked(time.Now())
	granted := w.granted
	g.mu.Unlock()

	if !granted {
		select {
		case <-w.ready:
		case <-ctx.Done():
			g.mu.Lock()
			if w.granted {
				// Granted in the same instant we were cancelled: hand the slot
				// straight back (and re-run grants) so it isn't leaked.
				g.releaseLocked(class)
			} else {
				g.removeWaiterLocked(w)
			}
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}

	release := func() {
		g.mu.Lock()
		g.releaseLocked(class)
		g.mu.Unlock()
	}

	// Start pacing — Refresh/Background only. Interactive skips it entirely (no
	// wait, no pacer tick consumed): a human is waiting, and the pacer's job is
	// trickling background bursts, which stay paced.
	if class != PriorityInteractive && g.minInterval > 0 {
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
				release()
				return nil, err
			}
		}
	}
	return release, nil
}

// releaseLocked frees one slot held by class and hands freed capacity straight
// to eligible waiters (direct handoff — a free slot never coexists with a
// grantable waiter, which is what lets arriving calls self-grant safely).
func (g *governor) releaseLocked(class Priority) {
	g.held--
	if class != PriorityInteractive {
		g.heldShared--
	}
	g.grantLocked(time.Now())
}

// grantLocked grants slots to queued waiters until no waiter is eligible.
func (g *governor) grantLocked(now time.Time) {
	for {
		w := g.pickLocked(now)
		if w == nil {
			return
		}
		// pickLocked only ever returns a class-queue head (FIFO within class).
		g.queues[w.class] = g.queues[w.class][1:]
		g.held++
		if w.class != PriorityInteractive {
			g.heldShared++
		}
		w.granted = true
		close(w.ready)
	}
}

// pickLocked chooses the next waiter to grant, or nil when none is eligible:
//
//  1. anti-starvation first: the OLDEST Refresh/Background waiter aged past
//     governorStarvationAge takes the next shared grant, even over waiting
//     Interactive work (which is never starved — it has the reserved slot);
//  2. Interactive next — head of the queue for shared capacity too, and the
//     only class allowed to consume the reserved slot (held may reach capTotal);
//  3. then Refresh, then Background, within shared capacity.
func (g *governor) pickLocked(now time.Time) *govWaiter {
	if g.held >= g.capTotal {
		return nil
	}
	sharedFree := g.heldShared < g.capShared
	if sharedFree {
		if w := g.oldestStarvedLocked(now); w != nil {
			return w
		}
	}
	if q := g.queues[PriorityInteractive]; len(q) > 0 {
		return q[0]
	}
	if sharedFree {
		if q := g.queues[PriorityRefresh]; len(q) > 0 {
			return q[0]
		}
		if q := g.queues[PriorityBackground]; len(q) > 0 {
			return q[0]
		}
	}
	return nil
}

// oldestStarvedLocked returns the oldest Refresh/Background waiter queued
// longer than governorStarvationAge, or nil. Only queue heads can qualify
// (FIFO within class ⇒ the head is the oldest of its class).
func (g *governor) oldestStarvedLocked(now time.Time) *govWaiter {
	var oldest *govWaiter
	for _, class := range [...]Priority{PriorityRefresh, PriorityBackground} {
		q := g.queues[class]
		if len(q) == 0 {
			continue
		}
		w := q[0]
		if now.Sub(w.since) <= governorStarvationAge {
			continue
		}
		if oldest == nil || w.since.Before(oldest.since) {
			oldest = w
		}
	}
	return oldest
}

// removeWaiterLocked drops a cancelled, not-yet-granted waiter from its queue.
func (g *governor) removeWaiterLocked(w *govWaiter) {
	q := g.queues[w.class]
	for i, x := range q {
		if x == w {
			g.queues[w.class] = append(q[:i], q[i+1:]...)
			return
		}
	}
}

// heldCount reports the current number of held slots (tests/diagnostics).
func (g *governor) heldCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

// queuedCount reports the number of waiters queued for class (tests).
func (g *governor) queuedCount(class Priority) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queues[class])
}
