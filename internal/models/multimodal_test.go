package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// ImageDataPart wraps base64 as a data URI defaulting to PNG, with no detail field,
// and honours a custom mime type.
func TestImageDataPartShape(t *testing.T) {
	def := ImageDataPart("AAAA", "")
	if def.Type != "image_url" || def.ImageURL != "data:image/png;base64,AAAA" {
		t.Fatalf("default part = %+v", def)
	}
	custom := ImageDataPart("ZZ", "image/jpeg")
	if custom.ImageURL != "data:image/jpeg;base64,ZZ" {
		t.Fatalf("custom mime = %q", custom.ImageURL)
	}
	// Round-trip through the wire marshaller: the image_url object must carry only a
	// url (no detail field).
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}` {
		t.Fatalf("wire = %s", b)
	}
}

func TestTextPartShape(t *testing.T) {
	p := TextPart("hi")
	if p.Type != "text" || p.Text != "hi" {
		t.Fatalf("text part = %+v", p)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"type":"text","text":"hi"}` {
		t.Fatalf("wire = %s", b)
	}
}

// HasImageContent detects image parts only — plain string content and a text-only
// part array are both not images.
func TestHasImageContentMatrix(t *testing.T) {
	cases := []struct {
		name string
		msgs []ChatMessage
		want bool
	}{
		{"plain string", []ChatMessage{TextMessage("user", "plain")}, false},
		{"text-only parts", []ChatMessage{{Role: "user", Parts: []ChatContentPart{TextPart("just text")}}}, false},
		{"text+image", []ChatMessage{{Role: "user", Parts: []ChatContentPart{TextPart("look"), ImageDataPart("x", "")}}}, true},
	}
	for _, c := range cases {
		if got := HasImageContent(c.msgs); got != c.want {
			t.Errorf("%s: HasImageContent = %v, want %v", c.name, got, c.want)
		}
	}
}

// ContentToText flattens, collapsing images to a marker; nil/string pass through.
func TestContentToTextFlatten(t *testing.T) {
	if got := (ChatMessage{Role: "user", StringContent: ""}).ContentToText(); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := TextMessage("user", "hello").ContentToText(); got != "hello" {
		t.Fatalf("string = %q", got)
	}
	m := ChatMessage{Role: "user", Parts: []ChatContentPart{TextPart("Describe this"), ImageDataPart("bigbase64", "")}}
	if got := m.ContentToText(); got != "Describe this\n[image omitted]" {
		t.Fatalf("flatten = %q", got)
	}
}

// json() forwards a multimodal content-part array unchanged AND still requests a
// json_object response_format (the image gate is the router's job, not the client's).
func TestJSONForwardsMultimodalAndRequestsJSONObject(t *testing.T) {
	var raw string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, _, err := c.JSON(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{
		{Role: "user", Parts: []ChatContentPart{TextPart("read this"), ImageDataPart("p", "")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := body["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	// Assert on the raw request bytes so we verify the wire shape (key order preserved):
	// the content-part array is forwarded verbatim, image part carries only a url.
	want := `"content":[{"type":"text","text":"read this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,p"}}]`
	if !strings.Contains(raw, want) {
		t.Fatalf("multimodal wire not forwarded verbatim: %s", raw)
	}
}

// A plain string-content message forwards untouched (no array wrapping).
func TestChatLeavesPlainStringContentUntouched(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}})
	if err != nil {
		t.Fatal(err)
	}
	// Plain string content stays a string on the wire (never wrapped in an array).
	if !strings.Contains(raw, `"messages":[{"role":"user","content":"hi"}]`) {
		t.Fatalf("plain string content not preserved: %s", raw)
	}
}

// The router forwards image content undistorted on the large tier (no gate, parts
// preserved) on both chat() and stream().
func TestRouterForwardsImageOnLargeTier(t *testing.T) {
	var raw string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "minimax-m3", MediumModel: "minimax-m3", SmallModel: "deepseek-v4-flash"},
		newTestClient(srv.URL), nil)
	imgMsg := []ChatMessage{{Role: "user", Parts: []ChatContentPart{TextPart("describe"), ImageDataPart("x", "")}}}
	if _, err := r.Stream(context.Background(), domain.ModelLarge, ChatOptions{Messages: imgMsg}, nil); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "minimax-m3" {
		t.Fatalf("model = %v, want minimax-m3 (tier resolved)", body["model"])
	}
	// The router must not mutate the content parts on the way through.
	want := `"content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]`
	if !strings.Contains(raw, want) {
		t.Fatalf("router distorted image parts: %s", raw)
	}
}

// The router does NOT gate plain-text messages on the small tier — they pass through
// to the wire.
func TestRouterPlainTextNotGatedOnSmall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	r := NewRouter(RouterConfig{LargeModel: "L", MediumModel: "M", SmallModel: "S"}, newTestClient(srv.URL), nil)
	res, err := r.Chat(context.Background(), domain.ModelSmall, ChatOptions{Messages: []ChatMessage{TextMessage("user", "hi")}})
	if err != nil {
		t.Fatalf("plain text on small must not be gated: %v", err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q", res.Content)
	}
}
