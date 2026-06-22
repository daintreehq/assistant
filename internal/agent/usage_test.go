package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// usageRouter streams one plain answer with a configurable usage + model id, so
// the per-round UsageEvent assertions can be driven end-to-end through Send.
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
	if u.ContextTokens <= 0 {
		t.Fatalf("contextTokens = %d want > 0", u.ContextTokens)
	}
	// minimax-m3 is priced, so cost is a concrete positive number.
	if u.CostUsd == nil || *u.CostUsd <= 0 {
		t.Fatalf("costUsd = %v want > 0", u.CostUsd)
	}
}

func TestUsageEventStripsFireworksAccountPath(t *testing.T) {
	u := sendOnce(t, "accounts/fireworks/models/minimax-m3", &models.Usage{
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
