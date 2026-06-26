package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// usageRouter streams one plain answer and reports usage on its ChatResult. The backend
// owns model routing now, so usage flows through the single backend.Usage on the respond
// result (mapped from the Router's ChatResult.Usage by the test adapter) — there is no
// per-tier FlushMeter rollup or client-side cost pricing anymore.
type usageRouter struct {
	model string
	usage *models.Usage
}

func (r *usageRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	if onToken != nil {
		onToken("hi")
	}
	return models.ChatResult{Content: "hi", Usage: r.usage}, nil
}
func (r *usageRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *usageRouter) ModelFor(domain.ModelTier) string { return r.model }
func (r *usageRouter) FlushMeter() []models.TierUsage   { return nil }

func derefp(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func intp(n int) *int { return &n }

// usageCaptureSink records the per-round UsageEvent for assertions.
type usageCaptureSink struct {
	NoopEventSink
	events []UsageEvent
}

func (s *usageCaptureSink) Usage(ev UsageEvent) { s.events = append(s.events, ev) }

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

func TestUsageEventTokenCountsAndContextPressure(t *testing.T) {
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
	// ContextTokens reports the REAL provider prompt_tokens (tool schemas included), so it
	// equals the reported prompt tokens whenever the provider reported any.
	if u.ContextTokens != 1000 {
		t.Fatalf("contextTokens = %d want 1000 (real prompt_tokens)", u.ContextTokens)
	}
}

func TestUsageEventContextFallsBackToEstimateWhenNoUsage(t *testing.T) {
	u := sendOnce(t, "minimax-m3", nil) // no usage chunk
	// Context pressure is still meaningful (estimated over our own buffer)...
	if u.ContextTokens <= 0 {
		t.Fatalf("contextTokens = %d want > 0", u.ContextTokens)
	}
	// ...and cost is "no data" (the backend owns pricing now; the CLI never fabricates it).
	if u.CostUsd != nil {
		t.Fatalf("costUsd = %v want nil (no client-side pricing)", *u.CostUsd)
	}
}

// CacheHitRatio is the cached/prompt share for the round's reported usage. Present when
// the provider reports cached prompt tokens.
func TestUsageEventCacheHitRatioPresentWhenCachedReported(t *testing.T) {
	u := sendOnce(t, "minimax-m3", &models.Usage{
		PromptTokens: intp(1000), CompletionTokens: intp(200), TotalTokens: intp(1200), CachedTokens: intp(400),
	})
	if u.CachedTokens == nil || *u.CachedTokens != 400 {
		t.Fatalf("cachedTokens = %v want 400", u.CachedTokens)
	}
	if u.CacheHitRatio == nil || *u.CacheHitRatio != 0.4 {
		t.Fatalf("cacheHitRatio = %v want 0.4 (400/1000)", u.CacheHitRatio)
	}
}

// With no cached prompt tokens reported the ratio is nil — "no data", never a misleading
// 0.0 (which would read as "cache always missed").
func TestUsageEventCacheHitRatioNilWhenNoCachedData(t *testing.T) {
	u := sendOnce(t, "minimax-m3", &models.Usage{
		PromptTokens: intp(1000), CompletionTokens: intp(200), TotalTokens: intp(1200),
	})
	if u.CachedTokens != nil {
		t.Fatalf("cachedTokens = %v want nil (none reported)", *u.CachedTokens)
	}
	if u.CacheHitRatio != nil {
		t.Fatalf("cacheHitRatio = %v want nil (no cached data)", *u.CacheHitRatio)
	}
}

// Zero prompt tokens must not divide-by-zero: even with a cached report, a PromptTokens==0
// round yields a nil ratio (and a NaN/Inf would be a bug).
func TestUsageEventCacheHitRatioNilWhenZeroPrompt(t *testing.T) {
	u := sendOnce(t, "minimax-m3", &models.Usage{
		PromptTokens: intp(0), CompletionTokens: intp(0), TotalTokens: intp(0), CachedTokens: intp(7),
	})
	if u.CacheHitRatio != nil {
		t.Fatalf("cacheHitRatio = %v want nil (PromptTokens==0 guard)", *u.CacheHitRatio)
	}
}

// The ratio is exposed RAW, never clamped to 1.0: a provider anomaly where cached exceeds
// prompt (cached=1200 of 1000) surfaces as 1.2 so the bad data stays visible.
func TestUsageEventCacheHitRatioNotClampedAboveOne(t *testing.T) {
	u := sendOnce(t, "minimax-m3", &models.Usage{
		PromptTokens: intp(1000), CompletionTokens: intp(200), TotalTokens: intp(1200), CachedTokens: intp(1200),
	})
	if u.CacheHitRatio == nil {
		t.Fatal("cacheHitRatio = nil want 1.2")
	}
	if d := *u.CacheHitRatio - 1.2; d < -1e-9 || d > 1e-9 {
		t.Fatalf("cacheHitRatio = %v want 1.2 (1200/1000)", *u.CacheHitRatio)
	}
}

// emitBackendUsage stashes the provider prompt_tokens on the session so the NEXT round's
// auto-compaction check can gate on the real context size (tool schemas included) rather
// than the tool-blind char estimate.
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
		t.Fatalf("lastPromptTokens = %d want 1234 (the provider prompt_tokens)", got)
	}
}

// A round with no usage (PromptTokens==0) must PRESERVE the last real figure rather than
// regress the stash to 0 — a missing figure means no metered call this round, not that the
// context shrank.
func TestEmitUsageNoUsagePreservesExistingStash(t *testing.T) {
	deps := baseDeps(&usageRouter{model: "minimax-m3", usage: nil}, &fakeTools{})
	s := NewSession(deps)
	s.mu.Lock()
	s.lastPromptTokens = 5000
	s.mu.Unlock()
	s.emitBackendUsage(backend.Usage{}, "minimax-m3")
	s.mu.Lock()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 5000 {
		t.Fatalf("lastPromptTokens = %d want 5000 (a no-usage round must not regress the stash)", got)
	}
}

// When the provider reports no usage, there is no real figure to stash: lastPromptTokens
// stays 0 and ContextTokens falls back to the positive char estimate so the footer never
// shows a misleading 0.
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
