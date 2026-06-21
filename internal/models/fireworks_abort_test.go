package models

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// In Go the AbortSignal equivalent is a cancelled context.Context. A cancelled
// turn must surface as *CancelledError on chat(), json(), and chatStream() — the
// raw transport/context error never leaks upward.

func TestChatNormalisesAbortToCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already aborted before the wire call
	_, err := c.Chat(ctx, ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}})
	if _, ok := err.(*CancelledError); !ok {
		t.Fatalf("chat abort: want *CancelledError, got %T (%v)", err, err)
	}
}

func TestJSONNormalisesAbortToCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.JSON(ctx, ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}})
	if _, ok := err.(*CancelledError); !ok {
		t.Fatalf("json abort: want *CancelledError, got %T (%v)", err, err)
	}
}

func TestChatStreamNormalisesAbortToCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ChatStream(ctx, ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}}, nil)
	if _, ok := err.(*CancelledError); !ok {
		t.Fatalf("stream abort: want *CancelledError, got %T (%v)", err, err)
	}
}

// A non-abort error (a fatal 4xx) is NOT wrapped as CancelledError — it propagates
// as the underlying apiError so the classifier can distinguish failure from cancel.
func TestChatLeavesNonAbortErrorUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "hi")}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := err.(*CancelledError); ok {
		t.Fatalf("non-abort error must not normalise to CancelledError: %v", err)
	}
}

// The model-call retry budget is 1 initial attempt + MaxRetries (3) = 4 total tries
// before a persistent 5xx propagates.
func TestChatExhaustsRetryBudget(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "still broken")
	}))
	defer srv.Close()
	// Shrink the backoff window so the four attempts don't sleep for real.
	c := newTestClient(srv.URL)
	_, err := c.Chat(context.Background(), ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "x")}})
	if err == nil {
		t.Fatal("expected error after exhausting the budget")
	}
	if got := atomic.LoadInt32(&attempts); got != int32(1+ModelRetryPolicy.MaxRetries) {
		t.Fatalf("attempts = %d, want %d (1 + %d retries)", got, 1+ModelRetryPolicy.MaxRetries, ModelRetryPolicy.MaxRetries)
	}
}

// Streaming retry is PRE-TOKEN ONLY: once a visible token has reached the caller, a
// later mid-stream failure must propagate unchanged rather than restart (which would
// duplicate "hel" into the immutable transcript).
func TestChatStreamNoRetryOnceTokenEmitted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		fl, _ := w.(http.Flusher)
		// Emit one visible token, flush, then slam the connection shut mid-stream.
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Abruptly close by hijacking — but simplest is to panic the handler so the
		// body is truncated without a [DONE]; net/http closes the conn, the client's
		// SSE scanner reaches EOF cleanly. To force an error mid-stream we instead
		// write a partial then close via hijack.
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	var toks []string
	_, _ = c.ChatStream(context.Background(), ChatOptions{Model: "m",
		Messages: []ChatMessage{TextMessage("user", "x")}}, func(s string) { toks = append(toks, s) })
	// Whether the truncated body is an error or a clean EOF, the contract under test
	// is: NEVER more than one attempt once a token was emitted.
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry after a token was emitted)", got)
	}
}

// A cancel that lands while the retry backoff is sleeping ends the loop as a clean
// cancellation — no second attempt is made.
func TestChatStreamCancelMidBackoffNoSecondAttempt(t *testing.T) {
	var attempts int32
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		// Fail with a retriable 5xx, and cancel the turn so the abort lands while the
		// backoff sleep is waiting before a (would-be) second attempt.
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)
	_, err := c.ChatStream(ctx, ChatOptions{Model: "m", Messages: []ChatMessage{TextMessage("user", "x")}}, nil)
	if _, ok := err.(*CancelledError); !ok {
		t.Fatalf("want *CancelledError, got %T (%v)", err, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (cancel mid-backoff ends the loop)", got)
	}
}

// The router gates image content on stream() and json() the same way it gates
// chat(): small and medium reject before any wire call; large is allowed.
func TestRouterImageTierGateStreamAndJSON(t *testing.T) {
	r := NewRouter(RouterConfig{LargeModel: "L", MediumModel: "M", SmallModel: "S"}, nil, nil)
	imgMsg := []ChatMessage{{Role: "user", Parts: []ChatContentPart{TextPart("describe"), ImageDataPart("AAAA", "")}}}
	for _, tier := range []domain.ModelTier{domain.ModelSmall, domain.ModelMedium} {
		if _, err := r.Stream(context.Background(), tier, ChatOptions{Messages: imgMsg}, nil); err == nil {
			t.Fatalf("stream tier %s must reject image", tier)
		} else if _, ok := err.(*ImageInputNotSupportedError); !ok {
			t.Fatalf("stream tier %s: want *ImageInputNotSupportedError, got %T", tier, err)
		}
		if _, err := r.JSON(context.Background(), tier, ChatOptions{Messages: imgMsg}); err == nil {
			t.Fatalf("json tier %s must reject image", tier)
		} else if _, ok := err.(*ImageInputNotSupportedError); !ok {
			t.Fatalf("json tier %s: want *ImageInputNotSupportedError, got %T", tier, err)
		}
	}
}

// The image-gate error carries the stable downstream code and a clear name.
func TestImageInputNotSupportedErrorCode(t *testing.T) {
	r := NewRouter(RouterConfig{LargeModel: "L", MediumModel: "M", SmallModel: "S"}, nil, nil)
	imgMsg := []ChatMessage{{Role: "user", Parts: []ChatContentPart{ImageDataPart("x", "")}}}
	_, err := r.Chat(context.Background(), domain.ModelSmall, ChatOptions{Messages: imgMsg})
	ie, ok := err.(*ImageInputNotSupportedError)
	if !ok {
		t.Fatalf("want *ImageInputNotSupportedError, got %T", err)
	}
	if ie.Code() != "IMAGE_INPUT_NOT_SUPPORTED" {
		t.Fatalf("code = %q", ie.Code())
	}
}
