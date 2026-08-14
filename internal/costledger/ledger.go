// Package costledger accumulates what this session has spent on the caller's own
// upstream key.
//
// Every model call the CLI makes is funded by the tester's OpenRouter key, and the
// prompt is large — so "what has this session cost me?" is a question they will ask, and
// the difference between a tester who trusts the tool and one who quietly stops using it
// is whether it can answer. The backend reports OpenRouter's own figures per call (it
// knows which of ~24 endpoints served it and what cache discount applied); this package
// does nothing but add them up honestly.
//
// The honesty is the hard part, and it is one rule: ABSENT MEANS UNKNOWN, NEVER FREE. A
// call whose cost the provider did not report, and a turn the backend flagged
// `complete: false`, both make the running total a LOWER BOUND rather than a sum. An
// accumulator that coerced either to zero would under-report someone's actual bill while
// presenting the result as a receipt — which is worse than showing nothing, because it
// would be believed.
//
// Scope is the PROCESS, deliberately. Nothing is persisted: a total that survived across
// launches would need a schema and would still be wrong the moment the supervisor daemon
// ran turns this process never saw. The OpenRouter dashboard is the authority on the
// bill; these figures are for proportion and trend — "the selector is 40% of my spend",
// "this session is getting expensive".
package costledger

import (
	"sort"
	"sync"

	"github.com/daintreehq/assistant/internal/backend"
)

// RespondOp is the Op value backend.Client reports for a conversation turn. Everything
// else is a utility task, named by its task id. Aliased from the producer rather than
// re-spelled here: two copies of a magic string is how a turn quietly starts being
// filed under "tasks".
const RespondOp = backend.RespondOp

// Ledger is the session's running total. Safe for concurrent use: cost events arrive
// from the turn goroutine, the watcher engine, the async coordinator and the scheduler,
// all of which can be running at once.
type Ledger struct {
	mu sync.Mutex

	observed float64 // sum of every amount actually reported
	calls    int     // billed calls seen, reported or not
	// unreported counts calls that happened and produced no figure. It is the ONLY
	// reason a total is a lower bound that a reader can act on — "the backend didn't
	// tell us about 3 of these" is a different statement from "3 of these were free".
	unreported int
	// incomplete counts calls the backend itself flagged as partial sums.
	incomplete int

	perOp map[string]opTotal

	cachedTokens int
	promptTokens int
}

type opTotal struct {
	amount     float64
	calls      int
	unreported int
	incomplete int
}

// New returns an empty ledger.
func New() *Ledger { return &Ledger{perOp: map[string]opTotal{}} }

// Record folds one billed call into the total. Safe to call with a zero-value event; a
// nil Amount is the "unknown" case and is counted as such rather than dropped.
func (l *Ledger) Record(ev backend.CostEvent) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perOp == nil {
		l.perOp = map[string]opTotal{}
	}

	op := ev.Op
	if op == "" {
		op = "unknown"
	}
	row := l.perOp[op]

	l.calls++
	row.calls++
	switch {
	case ev.Amount == nil:
		l.unreported++
		row.unreported++
	default:
		l.observed += *ev.Amount
		row.amount += *ev.Amount
	}
	// A reported-but-partial figure counts toward the sum AND toward the reason the sum
	// is a floor. Both are true at once. Tracked PER OP as well as globally: without
	// that, a single incomplete turn hedges the session line while the "turns" line
	// beside it still reads as exact — the same figure presented two contradictory ways
	// on one screen.
	if !ev.Complete {
		l.incomplete++
		row.incomplete++
	}
	l.perOp[op] = row

	l.cachedTokens += ev.CachedTokens
	l.promptTokens += ev.PromptTokens
}

// Snapshot is an immutable read of the ledger.
type Snapshot struct {
	// Observed is the sum of every reported figure, in USD.
	Observed float64
	// LowerBound is true when Observed is a floor rather than a total — some call
	// reported nothing, or the backend flagged a turn's own sum as partial. Render the
	// figure as "≥ $x" when this is set.
	LowerBound bool

	Calls      int
	Unreported int
	Incomplete int

	// Turns and Tasks split the two kinds of spend. They answer different questions —
	// "is my conversation expensive?" versus "is my supervision expensive?" — and the
	// second is the one a user has no other way to see, since utility tasks fire from
	// watchers and tools without ever appearing as a turn.
	Turns OpSummary
	Tasks OpSummary

	// ByOp is every op, most expensive first, for a detailed breakdown.
	ByOp []OpCost

	CachedTokens int
	PromptTokens int
}

// OpSummary is the rolled-up spend for one category of call.
type OpSummary struct {
	Amount     float64
	Calls      int
	Unreported int
	Incomplete int
}

// LowerBound reports whether this category's own figure is a floor rather than a sum.
func (s OpSummary) LowerBound() bool { return s.Unreported > 0 || s.Incomplete > 0 }

// OpCost is one op's line in the breakdown.
type OpCost struct {
	Op         string
	Amount     float64
	Calls      int
	Unreported int
	Incomplete int
}

// LowerBound reports whether this op's own figure is a floor rather than a sum.
func (c OpCost) LowerBound() bool { return c.Unreported > 0 || c.Incomplete > 0 }

// CacheHitRatio is the share of prompt tokens served from the backend's prompt cache,
// in [0,1], and false when no prompt tokens have been seen yet.
//
// It belongs next to the spend because it EXPLAINS the spend: the backend's whole prompt
// assembly exists to keep ~18k tokens of tool schemas byte-stable, and a collapse in this
// ratio is the first symptom of a regression that costs the user money directly.
func (s Snapshot) CacheHitRatio() (float64, bool) {
	if s.PromptTokens <= 0 {
		return 0, false
	}
	return float64(s.CachedTokens) / float64(s.PromptTokens), true
}

// Snapshot reads the current totals.
func (l *Ledger) Snapshot() Snapshot {
	if l == nil {
		return Snapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	s := Snapshot{
		Observed:     l.observed,
		LowerBound:   l.unreported > 0 || l.incomplete > 0,
		Calls:        l.calls,
		Unreported:   l.unreported,
		Incomplete:   l.incomplete,
		CachedTokens: l.cachedTokens,
		PromptTokens: l.promptTokens,
	}
	for op, row := range l.perOp {
		s.ByOp = append(s.ByOp, OpCost{
			Op: op, Amount: row.amount, Calls: row.calls,
			Unreported: row.unreported, Incomplete: row.incomplete,
		})
		target := &s.Tasks
		if op == RespondOp {
			target = &s.Turns
		}
		target.Amount += row.amount
		target.Calls += row.calls
		target.Unreported += row.unreported
		target.Incomplete += row.incomplete
	}
	// Most expensive first, name as the tiebreak so the order is stable across reads of
	// an unchanged ledger (map iteration is not).
	sort.Slice(s.ByOp, func(i, j int) bool {
		if s.ByOp[i].Amount != s.ByOp[j].Amount {
			return s.ByOp[i].Amount > s.ByOp[j].Amount
		}
		return s.ByOp[i].Op < s.ByOp[j].Op
	})
	return s
}

// Reset clears the ledger — used by /clear, which starts a fresh conversation and should
// not carry the previous one's bill into it.
func (l *Ledger) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Field by field, never `*l = Ledger{}`: that would overwrite the mutex we are
	// currently holding with a fresh unlocked one, and the deferred Unlock would then
	// release a lock nobody took.
	l.observed = 0
	l.calls = 0
	l.unreported = 0
	l.incomplete = 0
	l.cachedTokens = 0
	l.promptTokens = 0
	l.perOp = map[string]opTotal{}
}
