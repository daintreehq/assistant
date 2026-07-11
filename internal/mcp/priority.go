package mcp

import "context"

// Priority classifies MCP traffic for the per-client governor (see governor.go).
// Every subsystem shares ONE governed client, so without classes four background
// polls can occupy all in-flight capacity while a user-facing tool call queues
// behind them. The class rides the context — callers tag their outermost ctx
// once (WithPriority) and every MCP call made under it inherits the class.
type Priority int

const (
	// PriorityInteractive is user-requested work a human is actively waiting on:
	// in-turn tool dispatch, including the awaitAll/extract poll loops that run
	// inside a foreground turn. It owns the reserved slot, goes to the head of
	// the queue for shared slots, and skips start pacing.
	PriorityInteractive Priority = iota
	// PriorityRefresh is useful-now-but-degradable work: boot reconcile,
	// discovery/doctor probes, preview polls — things that can fall back to
	// cached/stale state if they wait. This is the DEFAULT for untagged callers:
	// the safest middle ground, so an unclassified call neither jumps user work
	// nor gets parked behind maintenance.
	PriorityRefresh
	// PriorityBackground is maintenance traffic with no one waiting: watcher
	// scheduler ticks, the async coordinator's poll. It yields shared slots to
	// everything else, protected from permanent starvation by the governor's
	// aging rule.
	PriorityBackground
)

// numPriorities sizes per-class governor state. Keep in sync with the enum.
const numPriorities = 3

type priorityCtxKey struct{}

// WithPriority returns a ctx tagged with the traffic class for every MCP call
// made under it. Tag at the outermost caller (the turn dispatch entry, the
// scheduler/coordinator loop ctx, the boot path) so the class flows naturally
// into every nested read.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityCtxKey{}, p)
}

// priorityFromContext extracts the tagged class; untagged (or out-of-range)
// callers default to PriorityRefresh — see the constant's doc for why.
func priorityFromContext(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityCtxKey{}).(Priority); ok &&
		p >= PriorityInteractive && p <= PriorityBackground {
		return p
	}
	return PriorityRefresh
}
