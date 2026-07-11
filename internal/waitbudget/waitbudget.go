// Package waitbudget is the per-turn cumulative foreground-wait allowance shared by
// every blocking wait a tool performs inside one user turn (today: terminal.awaitAll's
// poll sleeps). The agent session mints ONE Budget per user turn — reset each turn,
// NOT each model round — and threads it through the turn context; waiting tools draw
// down the shared balance so a chain of long waits across several model rounds cannot
// hold a turn in the foreground indefinitely. When the balance is gone the tool stops
// waiting and reports it, steering the model to the async path (watchers/queue)
// instead of another blocking wait.
//
// It lives in its own tiny package (not internal/agent, not the tool packages) so the
// import direction stays clean: both the agent loop (producer) and tool
// implementations (consumers) import waitbudget; neither imports the other.
//
// Zero-value safety is load-bearing: a context WITHOUT a budget (other callers,
// tests, non-turn dispatch paths) yields a nil *Budget from From, and every method is
// nil-safe with "unbudgeted" semantics — Consume grants in full, Exhausted is false —
// so unwired callers behave exactly as before budgets existed.
package waitbudget

import (
	"context"
	"sync"
	"time"
)

type ctxKey struct{}

// TurnBudget is the standard cumulative foreground-wait allowance for one user turn.
// The agent session mints New(TurnBudget) at the top of every turn; it lives here —
// not in the session — so the tools that enforce it (terminal.awaitAll) can state the
// same number in their model-facing descriptions without a cross-package copy.
const TurnBudget = 120 * time.Second

// Budget is a mutex-guarded countdown of foreground-wait time. Safe for concurrent
// draw-down (parallel-safe tool batches can wait concurrently; each sleep is
// debited exactly once).
type Budget struct {
	mu        sync.Mutex
	remaining time.Duration
}

// New returns a Budget holding d of foreground-wait allowance (clamped at zero).
func New(d time.Duration) *Budget {
	if d < 0 {
		d = 0
	}
	return &Budget{remaining: d}
}

// With returns a context carrying b. A nil b returns ctx unchanged (unbudgeted).
func With(ctx context.Context, b *Budget) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, b)
}

// From extracts the turn's Budget from ctx; nil when the context carries none
// (the unbudgeted legacy behavior — all methods are nil-safe).
func From(ctx context.Context) *Budget {
	b, _ := ctx.Value(ctxKey{}).(*Budget)
	return b
}

// Consume debits up to d from the balance and returns the amount actually granted
// (the duration the caller may spend waiting). It never returns more than d and
// never leaves the balance negative. Unbudgeted (nil receiver): grants d in full.
func (b *Budget) Consume(d time.Duration) time.Duration {
	if b == nil || d <= 0 {
		return d
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	granted := d
	if granted > b.remaining {
		granted = b.remaining
	}
	b.remaining -= granted
	return granted
}

// Remaining reports the current balance. Unbudgeted (nil receiver): a large
// sentinel, so callers comparing against thresholds behave as "plenty left".
func (b *Budget) Remaining() time.Duration {
	if b == nil {
		return time.Duration(1<<63 - 1)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

// Exhausted reports whether the balance has hit zero. Unbudgeted (nil receiver):
// always false — an unwired caller is never cut off.
func (b *Budget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining <= 0
}
