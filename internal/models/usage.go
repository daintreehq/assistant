package models

// Router-level usage meter. Every Chat/Stream/JSON call folds its token usage
// into a per-(tier,model) accumulator; the session drains it once per streamed
// round (emitUsage → FlushMeter) and sums the tiers into ONE UsageEvent. This is
// what makes the cost footer reflect EVERY model call in a turn — the large
// thread's streaming spend AND the small-tier background work (skill selection,
// watcher verdicts, context extraction, summaries) that previously went unmetered
// because only the large Stream result was ever read.

import (
	"sort"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TierUsage is the per-(tier,model) token + cost rollup FlushMeter returns. The
// session sums these into the aggregate UsageEvent and also passes the slice
// through as the per-tier breakdown for the durable run-event log.
type TierUsage struct {
	Tier             string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens is the summed cached-prompt subset, nil when NO call in this
	// bucket reported it (so the UI shows "no data", never a misleading 0).
	CachedTokens *int
	// CostUsd is the estimated USD spend for this bucket, nil when the model has
	// no known rate (so the UI shows "$?" rather than a misleading $0.000).
	CostUsd *float64
}

// usageKey buckets usage by tier AND resolved model id. Keying on both keeps the
// per-tier breakdown honest if a tier's model id ever changes mid-session and
// lets FlushAndReset price each bucket with its own model.
type usageKey struct {
	tier  domain.ModelTier
	model string
}

// usageBucket is the running total for one usageKey. cached sums only across the
// calls that reported it; hasCached records whether any did, so a bucket with no
// cached report flushes a nil CachedTokens rather than a false 0.
type usageBucket struct {
	prompt     int
	completion int
	total      int
	cached     int
	hasCached  bool
}

// usageAccumulator is the Router-level meter: a mutex-guarded set of per-key
// buckets. Background goroutines (watcher/timer JSON calls) Add concurrently with
// the main thread's Stream, so every mutation takes the lock. FlushAndReset is
// called only from the single-threaded turn goroutine.
type usageAccumulator struct {
	mu      sync.Mutex
	buckets map[usageKey]*usageBucket
}

func newUsageAccumulator() *usageAccumulator {
	return &usageAccumulator{buckets: map[usageKey]*usageBucket{}}
}

// Add folds one completed call's usage into its (tier,model) bucket. A nil usage
// (the provider reported nothing) is ignored — a missing count must never become
// a 0 that pollutes the running total.
func (a *usageAccumulator) Add(tier domain.ModelTier, model string, u *Usage) {
	if u == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := usageKey{tier: tier, model: model}
	b := a.buckets[key]
	if b == nil {
		b = &usageBucket{}
		a.buckets[key] = b
	}
	prompt := usageDeref(u.PromptTokens)
	completion := usageDeref(u.CompletionTokens)
	b.prompt += prompt
	b.completion += completion
	if u.TotalTokens != nil {
		b.total += *u.TotalTokens
	} else {
		b.total += prompt + completion
	}
	if u.CachedTokens != nil {
		b.hasCached = true
		b.cached += *u.CachedTokens
	}
}

// FlushAndReset snapshots every bucket into a sorted []TierUsage (priced via
// EstimateCostUsd) and clears the accumulator for the next turn. The buckets are
// detached under the lock; pricing/sorting run after the lock is released so a
// concurrent Add never blocks on cost arithmetic. A background call landing
// mid-drain simply rolls into the NEXT flush — acceptable for a running estimate.
// Returns nil when nothing was metered.
func (a *usageAccumulator) FlushAndReset() []TierUsage {
	a.mu.Lock()
	snapshot := a.buckets
	a.buckets = map[usageKey]*usageBucket{}
	a.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}
	out := make([]TierUsage, 0, len(snapshot))
	for key, b := range snapshot {
		tu := TierUsage{
			Tier:             string(key.tier),
			Model:            BareModelID(key.model),
			PromptTokens:     b.prompt,
			CompletionTokens: b.completion,
			TotalTokens:      b.total,
		}
		if b.hasCached {
			cached := b.cached
			tu.CachedTokens = &cached
		}
		if cost, ok := EstimateCostUsd(key.model, b.prompt, b.completion, b.cached); ok {
			tu.CostUsd = &cost
		}
		out = append(out, tu)
	}
	// Deterministic order (tier, then model) so the durable run-event payload and
	// test assertions are stable regardless of map iteration order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// usageDeref reads an optional wire count as 0 when absent.
func usageDeref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
