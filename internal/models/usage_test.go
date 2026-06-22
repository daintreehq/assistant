package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func ip(n int) *int { return &n }

// The stream captures token usage (incl. cached_tokens) from the final usage-only
// chunk (empty choices) that the endpoint sends when include_usage is set.
func TestChatStreamCapturesUsageWithCachedTokens(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	res, err := c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "hi")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hello" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Usage == nil {
		t.Fatal("usage is nil")
	}
	u := res.Usage
	if u.PromptTokens == nil || *u.PromptTokens != 100 ||
		u.CompletionTokens == nil || *u.CompletionTokens != 50 ||
		u.TotalTokens == nil || *u.TotalTokens != 150 ||
		u.CachedTokens == nil || *u.CachedTokens != 40 {
		t.Fatalf("usage = %+v", u)
	}
	// include_usage must have been requested.
	if so, ok := body["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v", body["stream_options"])
	}
}

// When no usage chunk arrives, usage stays nil (a missing count is nil, not a
// misleading zero).
func TestChatStreamUndefinedUsageWhenNoUsageChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	res, err := c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "hi")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != nil {
		t.Fatalf("usage = %+v, want nil", res.Usage)
	}
}

// --- Router-level usage meter (usage.go) ---

// The accumulator sums repeated calls for the same (tier,model) into one bucket
// and prices it from the static rate table.
func TestUsageAccumulatorAggregates(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(100), CompletionTokens: ip(20), TotalTokens: ip(120)})
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(50), CompletionTokens: ip(10), TotalTokens: ip(60)})
	tiers := a.FlushAndReset()
	if len(tiers) != 1 {
		t.Fatalf("tiers = %d want 1", len(tiers))
	}
	tu := tiers[0]
	if tu.Tier != string(domain.ModelLarge) || tu.Model != "glm-5p2" {
		t.Fatalf("tier/model = %q/%q", tu.Tier, tu.Model)
	}
	if tu.PromptTokens != 150 || tu.CompletionTokens != 30 || tu.TotalTokens != 180 {
		t.Fatalf("aggregate = %+v", tu)
	}
	if tu.CostUsd == nil || *tu.CostUsd <= 0 {
		t.Fatalf("costUsd = %v want > 0", tu.CostUsd)
	}
}

// A nil usage (provider reported nothing) is ignored — no bucket, no false 0.
func TestUsageAccumulatorIgnoresNil(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelLarge, "glm-5p2", nil)
	if tiers := a.FlushAndReset(); tiers != nil {
		t.Fatalf("tiers = %+v want nil", tiers)
	}
}

// Distinct tiers bucket separately and flush in deterministic (tier,model) order.
func TestUsageAccumulatorSeparatesTiers(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelSmall, "deepseek-v4-flash", &Usage{PromptTokens: ip(10), CompletionTokens: ip(2)})
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(30), CompletionTokens: ip(5)})
	tiers := a.FlushAndReset()
	if len(tiers) != 2 {
		t.Fatalf("tiers = %d want 2", len(tiers))
	}
	// Sorted by tier string: "large" < "small".
	if tiers[0].Tier != string(domain.ModelLarge) || tiers[1].Tier != string(domain.ModelSmall) {
		t.Fatalf("order = %q,%q", tiers[0].Tier, tiers[1].Tier)
	}
}

// FlushAndReset drains the accumulator: a second flush returns nothing.
func TestUsageAccumulatorFlushResets(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(100), CompletionTokens: ip(20)})
	if tiers := a.FlushAndReset(); len(tiers) != 1 {
		t.Fatalf("first flush = %d want 1", len(tiers))
	}
	if tiers := a.FlushAndReset(); tiers != nil {
		t.Fatalf("second flush = %+v want nil", tiers)
	}
}

// CachedTokens is nil unless some call in the bucket reported it.
func TestUsageAccumulatorCachedTokensNilWhenNoneReported(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(100), CompletionTokens: ip(20)})
	tu := a.FlushAndReset()[0]
	if tu.CachedTokens != nil {
		t.Fatalf("cachedTokens = %v want nil", *tu.CachedTokens)
	}
}

// A reported cached count of 0 is distinct from "no data": the pointer is set.
func TestUsageAccumulatorCachedTokensZeroWhenZeroReported(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelLarge, "glm-5p2", &Usage{PromptTokens: ip(100), CompletionTokens: ip(20), CachedTokens: ip(0)})
	tu := a.FlushAndReset()[0]
	if tu.CachedTokens == nil || *tu.CachedTokens != 0 {
		t.Fatalf("cachedTokens = %v want 0", tu.CachedTokens)
	}
}

// Concurrent Adds (the watcher/timer-goroutine case) are race-free and sum
// correctly. Run under -race.
func TestUsageAccumulatorConcurrent(t *testing.T) {
	a := newUsageAccumulator()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Add(domain.ModelSmall, "deepseek-v4-flash", &Usage{PromptTokens: ip(10), CompletionTokens: ip(2)})
		}()
	}
	wg.Wait()
	tiers := a.FlushAndReset()
	if len(tiers) != 1 {
		t.Fatalf("tiers = %d want 1", len(tiers))
	}
	if tiers[0].PromptTokens != n*10 || tiers[0].CompletionTokens != n*2 {
		t.Fatalf("aggregate = %+v", tiers[0])
	}
}

// A streamed call's usage flows through the Router meter and drains via
// FlushMeter, tagged with the streamed tier's resolved model.
func TestRouterFlushMeterAfterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "glm-5p2", MediumModel: "glm-5p2", SmallModel: "deepseek-v4-flash"}, newTestClient(srv.URL), nil)
	if _, err := r.Stream(context.Background(), domain.ModelLarge,
		ChatOptions{Messages: []ChatMessage{TextMessage("user", "hi")}}, nil); err != nil {
		t.Fatal(err)
	}
	tiers := r.FlushMeter()
	if len(tiers) != 1 {
		t.Fatalf("tiers = %d want 1", len(tiers))
	}
	if tiers[0].Tier != string(domain.ModelLarge) || tiers[0].Model != "glm-5p2" {
		t.Fatalf("tier/model = %q/%q", tiers[0].Tier, tiers[0].Model)
	}
	if tiers[0].PromptTokens != 100 || tiers[0].CompletionTokens != 50 {
		t.Fatalf("tokens = %+v", tiers[0])
	}
	// Drained — a second flush is empty.
	if len(r.FlushMeter()) != 0 {
		t.Fatal("second flush should be empty")
	}
}

// The json path's usage (previously discarded) now flows through the meter too,
// tagged with the resolved small-tier model.
func TestRouterFlushMeterAfterJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35}}`)
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "glm-5p2", MediumModel: "glm-5p2", SmallModel: "deepseek-v4-flash"}, newTestClient(srv.URL), nil)
	if _, err := r.JSON(context.Background(), domain.ModelSmall,
		ChatOptions{Messages: []ChatMessage{TextMessage("user", "give json")}}); err != nil {
		t.Fatal(err)
	}
	tiers := r.FlushMeter()
	if len(tiers) != 1 {
		t.Fatalf("tiers = %d want 1", len(tiers))
	}
	if tiers[0].Tier != string(domain.ModelSmall) || tiers[0].Model != "deepseek-v4-flash" {
		t.Fatalf("tier/model = %q/%q", tiers[0].Tier, tiers[0].Model)
	}
	if tiers[0].PromptTokens != 30 || tiers[0].CompletionTokens != 5 {
		t.Fatalf("tokens = %+v", tiers[0])
	}
}

// A failed call is never metered — even when the (rejected) response body
// carried a usage block.
func TestRouterDoesNotMeterFailedCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 200 with empty choices fails fast (no retry) AND carries usage, proving
		// the meter keys off success, not off the presence of a usage block.
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50}}`)
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "glm-5p2", MediumModel: "glm-5p2", SmallModel: "deepseek-v4-flash"}, newTestClient(srv.URL), nil)
	if _, err := r.JSON(context.Background(), domain.ModelSmall,
		ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}}); err == nil {
		t.Fatal("expected error on empty choices")
	}
	if len(r.FlushMeter()) != 0 {
		t.Fatal("failed call must not be metered")
	}
}

// A non-streaming Chat call is metered too (guards against the recordUsage call
// silently dropping off the Chat path).
func TestRouterFlushMeterAfterChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":40,"completion_tokens":8,"total_tokens":48}}`)
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "glm-5p2", MediumModel: "glm-5p2", SmallModel: "deepseek-v4-flash"}, newTestClient(srv.URL), nil)
	if _, err := r.Chat(context.Background(), domain.ModelLarge,
		ChatOptions{Messages: []ChatMessage{TextMessage("user", "hi")}}); err != nil {
		t.Fatal(err)
	}
	tiers := r.FlushMeter()
	if len(tiers) != 1 {
		t.Fatalf("tiers = %d want 1", len(tiers))
	}
	if tiers[0].Tier != string(domain.ModelLarge) || tiers[0].Model != "glm-5p2" {
		t.Fatalf("tier/model = %q/%q", tiers[0].Tier, tiers[0].Model)
	}
	if tiers[0].PromptTokens != 40 || tiers[0].CompletionTokens != 8 {
		t.Fatalf("tokens = %+v", tiers[0])
	}
}

// When TotalTokens is absent the bucket falls back to prompt+completion; a bucket
// mixing absent and explicit totals across calls sums each call's own total.
func TestUsageAccumulatorMixedTotalTokensNilAndExplicit(t *testing.T) {
	a := newUsageAccumulator()
	a.Add(domain.ModelSmall, "deepseek-v4-flash", &Usage{PromptTokens: ip(10), CompletionTokens: ip(2)}) // total nil → 12
	a.Add(domain.ModelSmall, "deepseek-v4-flash", &Usage{PromptTokens: ip(50), CompletionTokens: ip(8), TotalTokens: ip(120)})
	tu := a.FlushAndReset()[0]
	if tu.PromptTokens != 60 || tu.CompletionTokens != 10 {
		t.Fatalf("prompt/completion = %d/%d", tu.PromptTokens, tu.CompletionTokens)
	}
	if tu.TotalTokens != 132 { // (10+2) + 120
		t.Fatalf("totalTokens = %d want 132", tu.TotalTokens)
	}
}

// Add and FlushAndReset racing (the documented background-call-mid-drain case)
// must be race-free AND lossless: every Add lands in exactly one flush. Run -race.
func TestUsageAccumulatorConcurrentAddDuringFlush(t *testing.T) {
	a := newUsageAccumulator()
	const n = 200
	var (
		mu        sync.Mutex
		gotPrompt int
	)
	stop := make(chan struct{})
	var flushers sync.WaitGroup
	for i := 0; i < 4; i++ {
		flushers.Add(1)
		go func() {
			defer flushers.Done()
			drain := func() {
				for _, tu := range a.FlushAndReset() {
					mu.Lock()
					gotPrompt += tu.PromptTokens
					mu.Unlock()
				}
			}
			for {
				select {
				case <-stop:
					return
				default:
					drain()
				}
			}
		}()
	}
	var adders sync.WaitGroup
	for i := 0; i < n; i++ {
		adders.Add(1)
		go func() {
			defer adders.Done()
			a.Add(domain.ModelSmall, "deepseek-v4-flash", &Usage{PromptTokens: ip(10), CompletionTokens: ip(2)})
		}()
	}
	adders.Wait()
	close(stop)
	flushers.Wait()
	// Final drain for anything added after the last flusher loop iteration.
	for _, tu := range a.FlushAndReset() {
		gotPrompt += tu.PromptTokens
	}
	if gotPrompt != n*10 {
		t.Fatalf("summed prompt across flushes = %d want %d (lost or double-counted)", gotPrompt, n*10)
	}
}
