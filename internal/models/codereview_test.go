package models

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Fix 1: parseSSE must reject a truncated stream (EOF without [DONE]) and a
// malformed data payload, instead of fabricating a clean "stop" out of a partial
// response. The normal [DONE] terminator and comment/empty lines stay successful.
// ---------------------------------------------------------------------------

// EOF before [DONE] is a truncated stream → error, not a fake-complete success.
func TestParseSSEErrorsOnEOFWithoutDone(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"par"}}]}`,
		`data: {"choices":[{"delta":{"content":"tial"}}]}`,
		``, // truncated here — no [DONE]
	}, "\n")
	err := parseSSE(strings.NewReader(stream), func(*streamChunk) {})
	if err == nil {
		t.Fatal("truncated stream (no [DONE]) must error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want a truncation error classifiable as unexpected EOF, got %v", err)
	}
	if !isRetriableModelError(err) {
		t.Fatal("a truncation error must be retriable (pre-token)")
	}
}

// A malformed data: JSON payload is a protocol failure → error (not silently skipped).
func TestParseSSEErrorsOnMalformedPayload(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: {not valid json`,
		`data: [DONE]`,
		``,
	}, "\n")
	err := parseSSE(strings.NewReader(stream), func(*streamChunk) {})
	if err == nil {
		t.Fatal("malformed data payload must error")
	}
	if !isRetriableModelError(err) {
		t.Fatalf("a malformed-payload error must be retriable, got %v", err)
	}
}

// The normal [DONE] terminator + comment/empty separator lines succeed.
func TestParseSSESucceedsOnDoneWithComments(t *testing.T) {
	stream := strings.Join([]string{
		`: this is a comment`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var got string
	err := parseSSE(strings.NewReader(stream), func(c *streamChunk) {
		if len(c.Choices) > 0 && c.Choices[0].Delta.Content != nil {
			got += *c.Choices[0].Delta.Content
		}
	})
	if err != nil {
		t.Fatalf("clean stream must succeed: %v", err)
	}
	if got != "hi" {
		t.Fatalf("content = %q", got)
	}
}

// ChatStream surfaces a truncated stream as an error (pre-token retry applies).
// Here the truncation happens before any token, so the retriable budget retries
// and a clean second attempt succeeds.
func TestChatStreamRetriesTruncatedStreamPreToken(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// Truncated: a usage/empty chunk but NO [DONE] and NO visible token.
			_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
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
		t.Fatalf("pre-token truncation should retry to success: %v", err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q", res.Content)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Fix 2: transport / mid-body read failures are classified retriable, but a
// context.Canceled (cancellation) is NOT — it must propagate as a cancel.
// ---------------------------------------------------------------------------

func TestIsRetriableTransportAndReadErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unexpected EOF mid-body", io.ErrUnexpectedEOF, true},
		{"plain EOF (empty body)", io.EOF, true},
		{"net timeout", &net.DNSError{IsTimeout: true}, true},
		{"url.Error transport", &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, true},
		{"url.Error wrapping cancel", &url.Error{Op: "Post", Err: context.Canceled}, false},
		{"context canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		if got := isRetriableModelError(tc.err); got != tc.want {
			t.Errorf("%s: isRetriableModelError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A raw transport-level Do() failure (server closes before any response) is
// retriable and rides out the budget; cancellation never does.
func TestChatRetriesTransportFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			// Hijack and close without writing → the client's Do/read sees a broken
			// connection (an io.EOF / unexpected-EOF style transport error).
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	res, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "x")}})
	if err != nil {
		t.Fatalf("transport failure should retry to success: %v", err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q", res.Content)
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("attempts = %d, want >= 2 (transport error retried)", got)
	}
}

// ---------------------------------------------------------------------------
// Fix 3: JSON() errors on empty choices / empty content (no silent "{}").
// ---------------------------------------------------------------------------

func TestJSONErrorsOnEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	if _, err := c.JSON(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}); err == nil {
		t.Fatal("empty choices must error, not return {}")
	}
}

func TestJSONErrorsOnEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	if _, err := c.JSON(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}); err == nil {
		t.Fatal("empty content must error in json_object mode, not return {}")
	}
}

// A real json_object response still flows through (think-stripped + extracted).
func TestJSONHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<think>x</think>{\"ok\":true} trailing"}}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	got, err := c.JSON(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"ok":true}` {
		t.Fatalf("json = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Fix 4: a tool with nil/empty Parameters marshals to "parameters":{} on the
// wire, never the null Fireworks rejects.
// ---------------------------------------------------------------------------

func TestNilToolParametersMarshalsEmptyObject(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	_, err := c.Chat(context.Background(), ChatOptions{
		Model:    "m",
		Messages: []ChatMessage{TextMessage("user", "x")},
		Tools: []ChatTool{
			{Type: "function", Function: ChatToolFunc{Name: "ping" /* Parameters nil */}},
			{Type: "function", Function: ChatToolFunc{Name: "blank", Parameters: json.RawMessage("   ")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools wire = %v", body["tools"])
	}
	for i, raw := range tools {
		fn := raw.(map[string]any)["function"].(map[string]any)
		params, present := fn["parameters"]
		if !present {
			t.Fatalf("tool %d: parameters key missing", i)
		}
		if params == nil {
			t.Fatalf("tool %d: parameters is null (Fireworks rejects this)", i)
		}
		obj, ok := params.(map[string]any)
		if !ok || len(obj) != 0 {
			t.Fatalf("tool %d: parameters = %v, want empty object {}", i, params)
		}
	}
}

// normalizeTools leaves a real schema untouched (and shares the slice when nothing
// needs fixing).
func TestNormalizeToolsPreservesSchema(t *testing.T) {
	in := []ChatTool{{Type: "function", Function: ChatToolFunc{
		Name: "x", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}}}
	out := normalizeTools(in)
	if string(out[0].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("schema mutated: %s", out[0].Function.Parameters)
	}
}

// ---------------------------------------------------------------------------
// Fix 5: a stream that blows the total-byte ceiling errors cleanly.
// ---------------------------------------------------------------------------

// parseSSE enforces a cumulative total-byte ceiling across all data payloads.
func TestParseSSETotalByteCeiling(t *testing.T) {
	// Build many chunks whose combined payload bytes exceed maxStreamTotalBytes.
	// Use a payload just under the 8MB per-line cap, repeated until total > ceiling.
	big := strings.Repeat("a", 4*1024*1024)
	chunk := `data: {"choices":[{"delta":{"content":"` + big + `"}}]}`
	var sb strings.Builder
	for sb.Len() < maxStreamTotalBytes+len(chunk) {
		sb.WriteString(chunk)
		sb.WriteString("\n")
	}
	sb.WriteString("data: [DONE]\n")
	err := parseSSE(strings.NewReader(sb.String()), func(*streamChunk) {})
	if err == nil {
		t.Fatal("a stream over the total-byte ceiling must error")
	}
	if !strings.Contains(err.Error(), "total byte ceiling") {
		t.Fatalf("err = %v, want a total-byte-ceiling error", err)
	}
}

// ChatStream caps the accumulated arguments of a single streamed tool call. A
// runaway argument-fragment stream errors cleanly (before any visible token, so
// the failure is observable). The per-tool cap is below the total ceiling, so the
// breach is detected here and not the stream-total path.
func TestChatStreamPerToolArgCeiling(t *testing.T) {
	// Each fragment is ~4MB of arguments; two of them exceed the 8MB per-tool cap.
	frag := strings.Repeat("x", 5*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"n","arguments":"` + frag + `"}}]}}]}` + "\n\n")
		write(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` + frag + `"}}]}}]}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}, nil)
	if err == nil {
		t.Fatal("oversized tool arguments must error")
	}
	if !strings.Contains(err.Error(), "tool-call arguments exceeded") {
		t.Fatalf("err = %v, want a per-tool-argument ceiling error", err)
	}
}
