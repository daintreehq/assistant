package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func newTestClient(srvURL string) *FireworksClient {
	return NewFireworksClient(FireworksConfig{BaseURL: srvURL, APIKey: "test-key"})
}

func TestChatNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer auth")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<think>plan</think>answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "answer" || res.Reasoning != "plan" {
		t.Fatalf("res = %+v", res)
	}
	if res.Usage == nil || *res.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

// The per-tier reasoning_effort default is model-aware. Every tier on flash (the
// default for all three) runs reasoning_effort:"none" so each call — small-tier
// judges/summaries/extraction AND the large-tier orchestration main thread — stays
// thinking-free and fast (the loaded skills carry the playbooks, so a provider-side
// <think> phase is pure latency). A GLM-family model on large/medium keeps "high"
// because GLM-5.2 on Fireworks rejects "none" (its minimum effort is "high"). An
// explicit caller value wins on any tier.
func TestRouterTierReasoningDefaults(t *testing.T) {
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody = map[string]any{}
		_ = json.Unmarshal(b, &lastBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	// --- flash (non-GLM) tiers all default to "none" ---
	flash := NewRouter(RouterConfig{SmallModel: "small-m", LargeModel: "large-m", MediumModel: "med-m"}, newTestClient(srv.URL), nil)

	// small tier → reasoning_effort "none"
	_, _ = flash.JSON(context.Background(), domain.ModelSmall, ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "none" {
		t.Fatalf("small tier: reasoning_effort = %v, want none", lastBody["reasoning_effort"])
	}

	// large tier on a non-GLM model → defaults to "none" (no <think> on routing turns)
	_, _ = flash.Chat(context.Background(), domain.ModelLarge, ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "none" {
		t.Fatalf("large tier (flash): reasoning_effort = %v, want none", lastBody["reasoning_effort"])
	}

	// medium tier (routes to the large model id) on a non-GLM model → also "none"
	_, _ = flash.Chat(context.Background(), domain.ModelMedium, ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "none" {
		t.Fatalf("medium tier (flash): reasoning_effort = %v, want none", lastBody["reasoning_effort"])
	}

	// --- GLM-family large/medium keep "high" (GLM rejects "none") ---
	glm := NewRouter(RouterConfig{
		SmallModel:  "small-m",
		LargeModel:  "accounts/fireworks/models/glm-5p2",
		MediumModel: "accounts/fireworks/models/glm-5p2",
	}, newTestClient(srv.URL), nil)

	_, _ = glm.Chat(context.Background(), domain.ModelLarge, ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "high" {
		t.Fatalf("large tier (glm): reasoning_effort = %v, want high", lastBody["reasoning_effort"])
	}
	_, _ = glm.Chat(context.Background(), domain.ModelMedium, ChatOptions{Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "high" {
		t.Fatalf("medium tier (glm): reasoning_effort = %v, want high", lastBody["reasoning_effort"])
	}

	// --- explicit caller value wins on any tier ---
	// ...on the small tier
	_, _ = flash.JSON(context.Background(), domain.ModelSmall, ChatOptions{ReasoningEffort: "low", Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "low" {
		t.Fatalf("explicit effort (small): reasoning_effort = %v, want low", lastBody["reasoning_effort"])
	}
	// ...and on the large tier (overriding the flash "none" default)
	_, _ = flash.Chat(context.Background(), domain.ModelLarge, ChatOptions{ReasoningEffort: "max", Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["reasoning_effort"] != "max" {
		t.Fatalf("explicit effort (large): reasoning_effort = %v, want max", lastBody["reasoning_effort"])
	}
}

// guard() rejects offline + missing key before any wire call.
func TestGuard(t *testing.T) {
	off := NewFireworksClient(FireworksConfig{BaseURL: "x", APIKey: "k", Offline: true})
	if _, err := off.Chat(context.Background(), ChatOptions{}); err == nil {
		t.Fatal("offline must fail")
	} else if _, ok := err.(*FireworksUnavailableError); !ok {
		t.Fatalf("want FireworksUnavailableError, got %T", err)
	}
	nokey := NewFireworksClient(FireworksConfig{BaseURL: "x"})
	if _, err := nokey.Chat(context.Background(), ChatOptions{}); err == nil {
		t.Fatal("missing key must fail")
	}
}

// The chat path sends prompt_cache_key only when set; json never sends it.
func TestPromptCacheKeyWireRules(t *testing.T) {
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody = map[string]any{}
		_ = json.Unmarshal(b, &lastBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	// chat WITH cache key → present.
	_, _ = c.Chat(context.Background(), ChatOptions{Model: "m", PromptCacheKey: "daintree-main",
		Messages: []ChatMessage{TextMessage("user", "x")}})
	if lastBody["prompt_cache_key"] != "daintree-main" {
		t.Errorf("chat: prompt_cache_key = %v, want daintree-main", lastBody["prompt_cache_key"])
	}
	if lastBody["temperature"] != 0.3 {
		t.Errorf("chat: temperature = %v, want 0.3", lastBody["temperature"])
	}

	// json must NEVER send prompt_cache_key and defaults temperature to 0.
	_, _, _ = c.JSON(context.Background(), ChatOptions{Model: "m", PromptCacheKey: "daintree-main",
		Messages: []ChatMessage{TextMessage("user", "give JSON")}})
	if _, present := lastBody["prompt_cache_key"]; present {
		t.Errorf("json: prompt_cache_key must be absent, got %v", lastBody["prompt_cache_key"])
	}
	if lastBody["temperature"] != float64(0) {
		t.Errorf("json: temperature = %v, want 0", lastBody["temperature"])
	}
	if rf, ok := lastBody["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("json: response_format = %v", lastBody["response_format"])
	}
}

// stream sends stream:true + stream_options.include_usage and accumulates tokens.
func TestChatStream(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi \"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"there\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	var toks []string
	res, err := c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}, func(s string) { toks = append(toks, s) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hi there" {
		t.Fatalf("content = %q", res.Content)
	}
	if strings.Join(toks, "") != "Hi there" {
		t.Fatalf("tokens = %v", toks)
	}
	if body["stream"] != true {
		t.Errorf("stream flag missing")
	}
	if so, ok := body["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v", body["stream_options"])
	}
}

// A transient 500 before any token is retried; success on the second attempt.
func TestChatStreamPreTokenRetry(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	res, err := c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q", res.Content)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// The router image-tier gate rejects images on non-large tiers before any wire call.
func TestRouterImageTierGate(t *testing.T) {
	r := NewRouter(RouterConfig{LargeModel: "L", MediumModel: "M", SmallModel: "S"}, nil, nil)
	imgMsg := []ChatMessage{{Role: "user", Parts: []ChatContentPart{ImageDataPart("AAAA", "")}}}
	for _, tier := range []domain.ModelTier{domain.ModelSmall, domain.ModelMedium} {
		_, err := r.Chat(context.Background(), tier, ChatOptions{Messages: imgMsg})
		if err == nil {
			t.Fatalf("tier %s must reject image", tier)
		}
		if _, ok := err.(*ImageInputNotSupportedError); !ok {
			t.Fatalf("tier %s: want ImageInputNotSupportedError, got %T", tier, err)
		}
	}
}

func TestRouterModelFor(t *testing.T) {
	r := NewRouter(RouterConfig{LargeModel: "L", MediumModel: "M", SmallModel: "S"}, nil, nil)
	if r.ModelFor(domain.ModelSmall) != "S" || r.ModelFor(domain.ModelMedium) != "M" || r.ModelFor(domain.ModelLarge) != "L" {
		t.Fatal("modelFor mismatch")
	}
	if r.ModelFor(domain.ModelTier("bogus")) != "L" {
		t.Fatal("default must route to large")
	}
}

// A 400 is not retried (the apiError carries the status into the classifier).
func TestChatNoRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "x")}})
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (4xx not retried)", attempts)
	}
}
