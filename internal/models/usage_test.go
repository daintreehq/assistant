package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
