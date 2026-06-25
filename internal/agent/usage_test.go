package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// usageRouter streams one plain answer and reports a single large-tier usage via
// FlushMeter (the Router-level meter the session drains in emitUsage), so the
// per-round UsageEvent assertions can be driven end-to-end through Send.
type usageRouter struct {
	model string
	usage *models.Usage
}

func (r *usageRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	if onToken != nil {
		onToken("hi")
	}
	return models.ChatResult{Content: "hi"}, nil
}
func (r *usageRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *usageRouter) ModelFor(domain.ModelTier) string { return r.model }
func (r *usageRouter) FlushMeter() []models.TierUsage {
	if r.usage == nil {
		return nil
	}
	return []models.TierUsage{tierFromUsage(domain.ModelLarge, r.model, r.usage)}
}

// tierFromUsage builds the per-tier rollup a real Router would produce for one
// call, pricing it with the same EstimateCostUsd the production accumulator uses.
func tierFromUsage(tier domain.ModelTier, model string, u *models.Usage) models.TierUsage {
	prompt := derefp(u.PromptTokens)
	completion := derefp(u.CompletionTokens)
	tu := models.TierUsage{
		Tier:             string(tier),
		Model:            models.BareModelID(model),
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		CachedTokens:     u.CachedTokens,
	}
	if u.TotalTokens != nil {
		tu.TotalTokens = *u.TotalTokens
	}
	if cost, ok := models.EstimateCostUsd(model, prompt, completion, derefp(u.CachedTokens)); ok {
		tu.CostUsd = &cost
	}
	return tu
}

func derefp(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// tierRouter streams one plain answer and returns a PRESET per-tier breakdown
// from FlushMeter, so emitUsage's cross-tier aggregation (sum tokens, sum known
// costs, cached-when-any) can be asserted directly end-to-end.
type tierRouter struct {
	tiers []models.TierUsage
}

func (r *tierRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	if onToken != nil {
		onToken("hi")
	}
	return models.ChatResult{Content: "hi"}, nil
}
func (r *tierRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *tierRouter) ModelFor(domain.ModelTier) string { return "glm-5p2" }
func (r *tierRouter) FlushMeter() []models.TierUsage   { return r.tiers }

// pricedTier builds a TierUsage with cost computed from the static pricing table.
func pricedTier(tier domain.ModelTier, model string, prompt, completion int) models.TierUsage {
	tu := models.TierUsage{
		Tier:             string(tier),
		Model:            model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	if cost, ok := models.EstimateCostUsd(model, prompt, completion, 0); ok {
		tu.CostUsd = &cost
	}
	return tu
}

// usageCaptureSink records the per-round UsageEvent for assertions.
type usageCaptureSink struct {
	NoopEventSink
	events []UsageEvent
}

func (s *usageCaptureSink) Usage(ev UsageEvent) { s.events = append(s.events, ev) }

func intp(n int) *int { return &n }

func sendOnce(t *testing.T, model string, usage *models.Usage) UsageEvent {
	t.Helper()
	sink := &usageCaptureSink{}
	deps := baseDeps(&usageRouter{model: model, usage: usage}, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("usage events = %d want 1", len(sink.events))
	}
	return sink.events[0]
}

func sendTiers(t *testing.T, tiers []models.TierUsage) UsageEvent {
	t.Helper()
	sink := &usageCaptureSink{}
	deps := baseDeps(&tierRouter{tiers: tiers}, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("usage events = %d want 1", len(sink.events))
	}
	return sink.events[0]
}

func TestUsageEventContextPressureAndCost(t *testing.T) {
	u := sendOnce(t, "minimax-m3", &models.Usage{
		PromptTokens: intp(1000), CompletionTokens: intp(200), TotalTokens: intp(1200),
	})
	if u.PromptTokens != 1000 || u.CompletionTokens != 200 || u.TotalTokens != 1200 {
		t.Fatalf("token counts = %+v", u)
	}
	if u.Model != "minimax-m3" || u.Tier != string(domain.ModelLarge) {
		t.Fatalf("model/tier = %q/%q", u.Model, u.Tier)
	}
	// Context pressure is measured against the auto-compact threshold.
	if u.ContextThreshold != domain.AutoCompactTokenThreshold {
		t.Fatalf("contextThreshold = %d want %d", u.ContextThreshold, domain.AutoCompactTokenThreshold)
	}
	// ContextTokens now reports the REAL provider prompt_tokens (tool schemas
	// included), not the tool-blind char estimate, so it equals the summed prompt
	// tokens whenever the provider reported any.
	if u.ContextTokens != 1000 {
		t.Fatalf("contextTokens = %d want 1000 (real prompt_tokens)", u.ContextTokens)
	}
	// minimax-m3 is priced, so cost is a concrete positive number.
	if u.CostUsd == nil || *u.CostUsd <= 0 {
		t.Fatalf("costUsd = %v want > 0", u.CostUsd)
	}
}

func TestUsageEventStripsDeepSeekAccountPath(t *testing.T) {
	u := sendOnce(t, "accounts/deepseek/models/minimax-m3", &models.Usage{
		PromptTokens: intp(100), CompletionTokens: intp(20), TotalTokens: intp(120),
	})
	if u.Model != "minimax-m3" {
		t.Fatalf("model id not stripped: %q", u.Model)
	}
	// Cost is still computed from the path-stripped priced model.
	if u.CostUsd == nil || *u.CostUsd <= 0 {
		t.Fatalf("costUsd = %v want > 0", u.CostUsd)
	}
}

func TestUsageEventCostUndefinedWhenNoUsage(t *testing.T) {
	u := sendOnce(t, "minimax-m3", nil) // no usage chunk
	// Context pressure is still meaningful (estimated over our own buffer)...
	if u.ContextTokens <= 0 {
		t.Fatalf("contextTokens = %d want > 0", u.ContextTokens)
	}
	// ...but cost is "no data", not a misleading zero.
	if u.CostUsd != nil {
		t.Fatalf("costUsd = %v want nil (no data)", *u.CostUsd)
	}
}

// The UsageEvent rolls up EVERY tier the meter drained — the large stream PLUS
// small-tier background work — so the aggregate token counts and cost are the
// sum across tiers, and the per-tier breakdown rides along in ev.Tiers.
func TestUsageEventIncludesAllTiers(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	small := pricedTier(domain.ModelSmall, "deepseek-v4-flash", 500, 100)
	ev := sendTiers(t, []models.TierUsage{large, small})
	if len(ev.Tiers) != 2 {
		t.Fatalf("tiers = %d want 2", len(ev.Tiers))
	}
	if ev.PromptTokens != 1500 || ev.CompletionTokens != 300 || ev.TotalTokens != 1800 {
		t.Fatalf("aggregate tokens = %+v", ev)
	}
	if large.CostUsd == nil || small.CostUsd == nil {
		t.Fatal("both tiers should be priced")
	}
	wantCost := *large.CostUsd + *small.CostUsd
	if ev.CostUsd == nil || *ev.CostUsd != wantCost {
		t.Fatalf("costUsd = %v want %v", ev.CostUsd, wantCost)
	}
}

// CachedTokens stays nil unless at least one tier reported cached prompt tokens
// (so the UI shows "no data", never a false 0).
func TestUsageEventCachedNilWhenNoTierReports(t *testing.T) {
	ev := sendTiers(t, []models.TierUsage{
		pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200),
		pricedTier(domain.ModelSmall, "deepseek-v4-flash", 500, 100),
	})
	if ev.CachedTokens != nil {
		t.Fatalf("cachedTokens = %v want nil", *ev.CachedTokens)
	}
}

// When any tier reports cached tokens, the aggregate is the sum across the tiers
// that reported (tiers with no cached report contribute nothing).
func TestUsageEventCachedSumWhenAnyTierReports(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	cached := 40
	large.CachedTokens = &cached
	small := pricedTier(domain.ModelSmall, "deepseek-v4-flash", 500, 100) // no cached report
	ev := sendTiers(t, []models.TierUsage{large, small})
	if ev.CachedTokens == nil || *ev.CachedTokens != 40 {
		t.Fatalf("cachedTokens = %v want 40", ev.CachedTokens)
	}
}

// The cache-hit ratio is cachedTotal/PromptTokens with BOTH summed across every
// tier (issue #262). Here large reports 400 cached of its own 1000 prompt while
// small reports 600 prompt and no cache, so the ratio is 400/(1000+600)=0.25 —
// proving the denominator is the cross-tier aggregate, not the large tier alone
// (which would wrongly read 0.4).
func TestUsageEventCacheHitRatioPresentWhenCachedReported(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	cached := 400
	large.CachedTokens = &cached
	small := pricedTier(domain.ModelSmall, "deepseek-v4-flash", 600, 100) // no cached report
	ev := sendTiers(t, []models.TierUsage{large, small})
	if ev.CacheHitRatio == nil {
		t.Fatal("cacheHitRatio = nil want 0.25")
	}
	if *ev.CacheHitRatio != 0.25 {
		t.Fatalf("cacheHitRatio = %v want 0.25 (400/1600, cross-tier denominator)", *ev.CacheHitRatio)
	}
}

// With no tier reporting cached tokens the ratio is nil — "no data", never a
// misleading 0.0 (which would read as "cache always missed").
func TestUsageEventCacheHitRatioNilWhenNoCachedData(t *testing.T) {
	ev := sendTiers(t, []models.TierUsage{
		pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200),
		pricedTier(domain.ModelSmall, "deepseek-v4-flash", 500, 100),
	})
	if ev.CacheHitRatio != nil {
		t.Fatalf("cacheHitRatio = %v want nil (no cached data)", *ev.CacheHitRatio)
	}
}

// A reported cached=0 is a real data point (provider confirmed zero cache hits), so
// the ratio is a non-nil 0.0 — distinct from nil (no tier reported at all).
func TestUsageEventCacheHitRatioZeroWhenCachedReportsZero(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	zero := 0
	large.CachedTokens = &zero
	ev := sendTiers(t, []models.TierUsage{large})
	if ev.CacheHitRatio == nil {
		t.Fatal("cacheHitRatio = nil want 0.0 (provider reported zero cache hits)")
	}
	if *ev.CacheHitRatio != 0 {
		t.Fatalf("cacheHitRatio = %v want 0", *ev.CacheHitRatio)
	}
}

// Zero prompt tokens must not divide-by-zero: even with a cached report, a
// PromptTokens==0 round yields a nil ratio (and a NaN/Inf would be a bug).
func TestUsageEventCacheHitRatioNilWhenZeroPrompt(t *testing.T) {
	tier := pricedTier(domain.ModelLarge, "glm-5p2", 0, 0)
	cached := 0
	tier.CachedTokens = &cached
	ev := sendTiers(t, []models.TierUsage{tier})
	if ev.CacheHitRatio != nil {
		t.Fatalf("cacheHitRatio = %v want nil (PromptTokens==0 guard)", *ev.CacheHitRatio)
	}
}

// Both tiers reporting cached tokens contribute to the NUMERATOR (not just the
// large tier): large=400 + small=200 over 1000+600 prompt → 600/1600=0.375. Guards
// against a large-tier-only numerator bug that the single-cached-tier test can't
// catch (there the numerator happens to equal the large tier's contribution).
func TestUsageEventCacheHitRatioSumsCachedAcrossTiers(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	largeCached := 400
	large.CachedTokens = &largeCached
	small := pricedTier(domain.ModelSmall, "deepseek-v4-flash", 600, 100)
	smallCached := 200
	small.CachedTokens = &smallCached
	ev := sendTiers(t, []models.TierUsage{large, small})
	if ev.CacheHitRatio == nil {
		t.Fatal("cacheHitRatio = nil want 0.375")
	}
	if *ev.CacheHitRatio != 0.375 {
		t.Fatalf("cacheHitRatio = %v want 0.375 (600/1600, cross-tier numerator)", *ev.CacheHitRatio)
	}
}

// The ratio is exposed RAW, never clamped to 1.0: a provider anomaly where cached
// exceeds prompt (cached=1200 of 1000) surfaces as 1.2 so the bad data stays
// visible as a diagnostic rather than being silently hidden.
func TestUsageEventCacheHitRatioNotClampedAboveOne(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)
	cached := 1200
	large.CachedTokens = &cached
	ev := sendTiers(t, []models.TierUsage{large})
	if ev.CacheHitRatio == nil {
		t.Fatal("cacheHitRatio = nil want 1.2")
	}
	if *ev.CacheHitRatio <= 1.0 {
		t.Fatalf("cacheHitRatio = %v want > 1.0 (raw, not clamped)", *ev.CacheHitRatio)
	}
	if d := *ev.CacheHitRatio - 1.2; d < -1e-9 || d > 1e-9 {
		t.Fatalf("cacheHitRatio = %v want 1.2 (1200/1000)", *ev.CacheHitRatio)
	}
}

// The PromptTokens>0 guard fires even when cached is POSITIVE (an anomalous but
// possible report): a zero denominator yields nil, never a NaN/Inf ratio.
func TestUsageEventCacheHitRatioNilWhenZeroPromptPositiveCached(t *testing.T) {
	tier := pricedTier(domain.ModelLarge, "glm-5p2", 0, 0)
	cached := 7
	tier.CachedTokens = &cached
	ev := sendTiers(t, []models.TierUsage{tier})
	if ev.CacheHitRatio != nil {
		t.Fatalf("cacheHitRatio = %v want nil (PromptTokens==0 guard, even with cached>0)", *ev.CacheHitRatio)
	}
}

// A partial cost (some tier priced, some not) shows the KNOWN total rather than
// collapsing to "no data" — a rough running estimate is more useful than nothing.
func TestUsagePartialCostWhenOneTierUnpriced(t *testing.T) {
	large := pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200)           // priced
	unpriced := pricedTier(domain.ModelSmall, "mystery-model-x", 500, 100) // unknown rate → nil cost
	if large.CostUsd == nil {
		t.Fatal("large tier should be priced")
	}
	if unpriced.CostUsd != nil {
		t.Fatal("unknown-rate tier should have nil cost")
	}
	ev := sendTiers(t, []models.TierUsage{large, unpriced})
	if ev.CostUsd == nil || *ev.CostUsd != *large.CostUsd {
		t.Fatalf("costUsd = %v want partial %v", ev.CostUsd, large.CostUsd)
	}
	// Token counts still sum across BOTH tiers regardless of pricing.
	if ev.PromptTokens != 1500 || ev.CompletionTokens != 300 {
		t.Fatalf("tokens = %+v", ev)
	}
}

// emitUsage stashes the summed provider prompt_tokens on the session so the NEXT
// round's auto-compaction check can gate on the real context size (tool schemas
// included) rather than the tool-blind char estimate.
func TestEmitUsageStashesLastPromptTokens(t *testing.T) {
	deps := baseDeps(&usageRouter{model: "minimax-m3", usage: &models.Usage{
		PromptTokens: intp(1234), CompletionTokens: intp(50), TotalTokens: intp(1284),
	}}, &fakeTools{})
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 1234 {
		t.Fatalf("lastPromptTokens = %d want 1234 (the summed provider prompt_tokens)", got)
	}
}

// The compaction gate tracks the MAIN-THREAD (large-tier) prompt_tokens only — the
// small-tier background work (skill selection, watcher verdicts, and the auto-compact
// summary call) must NOT inflate it, or the summary's pre-compaction prompt would
// re-trip the trigger right after a compaction.
func TestEmitUsageStashesLargeTierPromptTokensOnly(t *testing.T) {
	deps := baseDeps(&tierRouter{tiers: []models.TierUsage{
		pricedTier(domain.ModelLarge, "glm-5p2", 1000, 200),
		pricedTier(domain.ModelSmall, "deepseek-v4-flash", 500, 100),
	}}, &fakeTools{})
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 1000 {
		t.Fatalf("lastPromptTokens = %d want 1000 (large tier only; small-tier work must not inflate the gate)", got)
	}
}

// A round with no usage (nil FlushMeter) must PRESERVE the last real figure rather than
// regress the stash to 0 — a nil flush means no metered call this round, not that the
// context shrank. The history only grows between resets, so the last figure stays a
// valid lower bound until a fresh round reports a new one.
func TestEmitUsageNilMeterPreservesExistingStash(t *testing.T) {
	deps := baseDeps(&usageRouter{model: "minimax-m3", usage: nil}, &fakeTools{})
	s := NewSession(deps)
	s.mu.Lock()
	s.lastPromptTokens = 5000
	s.mu.Unlock()
	s.emitUsage()
	s.mu.Lock()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 5000 {
		t.Fatalf("lastPromptTokens = %d want 5000 (a no-usage round must not regress the stash)", got)
	}
}

// When the meter reports no usage (nil FlushMeter), there is no real figure to stash:
// lastPromptTokens stays 0 and ContextTokens falls back to the positive char estimate
// so the footer never shows a misleading 0.
func TestEmitUsageNoUsageLeavesStashZeroAndEstimatesContext(t *testing.T) {
	deps := baseDeps(&usageRouter{model: "minimax-m3", usage: nil}, &fakeTools{})
	sink := &usageCaptureSink{}
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("lastPromptTokens = %d want 0 (no provider figure to stash)", got)
	}
	if len(sink.events) != 1 || sink.events[0].ContextTokens <= 0 {
		t.Fatalf("contextTokens should fall back to a positive estimate, got %+v", sink.events)
	}
}
