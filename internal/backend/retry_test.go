package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// streamFail is one mid-stream failure: a committed 200 with a meta event followed by
// a terminal `error` event (the shape the backend emits for an upstream 502). No
// content fragment is sent, so a retry is safe.
const streamFail = "event: meta\ndata: {}\n\n" +
	"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"

// streamOK is a successful stream: meta, one content delta, then done.
const streamOK = "event: meta\ndata: {}\n\n" +
	"event: delta\ndata: {\"content\":\"hi\"}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

// countingServer returns an httptest server that records how many times it was hit and
// writes whatever body the per-request handler returns.
func countingServer(t *testing.T, handler func(n int) (status int, body string)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := count
		count++
		mu.Unlock()
		status, body := handler(n)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

// fastRetry is a policy that exercises the retry loop without real waiting.
func fastRetry(maxAttempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: maxAttempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestRespondStream_RetriesTransientThenSucceeds(t *testing.T) {
	const failFirst = 2
	srv, hits := countingServer(t, func(n int) (int, string) {
		if n < failFirst {
			return http.StatusOK, streamFail
		}
		return http.StatusOK, streamOK
	})
	defer srv.Close()

	var retries int
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   fastRetry(6),
		OnRetry: func(RetryInfo) { retries++ },
	})

	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if res.Message.Content != "hi" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "hi")
	}
	if retries != failFirst {
		t.Fatalf("OnRetry fired %d times, want %d", retries, failFirst)
	}
	if got := hits(); got != failFirst+1 {
		t.Fatalf("server hit %d times, want %d", got, failFirst+1)
	}
}

func TestRespondStream_NoRetryAfterContentStreamed(t *testing.T) {
	// meta + a content delta, THEN a transient error: the user has already seen the
	// token, so the turn must NOT be replayed.
	const partialThenFail = "event: meta\ndata: {}\n\n" +
		"event: delta\ndata: {\"content\":\"partial\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\"}}\n\n"
	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, partialThenFail })
	defer srv.Close()

	var got string
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(6)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnContent: func(s string) { got += s },
	})
	if err == nil {
		t.Fatal("expected the post-content error to surface, got nil")
	}
	if got != "partial" {
		t.Fatalf("streamed content = %q, want %q", got, "partial")
	}
	if n := hits(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (no replay after content)", n)
	}
}

func TestRespondStream_NoRetryOnContractError(t *testing.T) {
	// A 400 is a deterministic contract bug — retrying would fail identically.
	srv, hits := countingServer(t, func(int) (int, string) {
		return http.StatusBadRequest, `{"error":{"type":"invalid_request","code":"bad","message":"nope"}}`
	})
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(6)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected a contract error, got nil")
	}
	if n := hits(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (400 is not retriable)", n)
	}
}

func TestRespondStream_RetryBudgetExhausted(t *testing.T) {
	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, streamFail })
	defer srv.Close()

	var retries int
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   fastRetry(3), // initial + 2 retries
		OnRetry: func(RetryInfo) { retries++ },
	})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected failure after exhausting the budget, got nil")
	}
	if retries != 2 {
		t.Fatalf("OnRetry fired %d times, want 2", retries)
	}
	if n := hits(); n != 3 {
		t.Fatalf("server hit %d times, want 3", n)
	}
}

func TestRespondStream_CancelDuringBackoff(t *testing.T) {
	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, streamFail })
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// A long base delay guarantees we are sitting in the backoff sleep when cancel
	// fires, so the loop must abandon promptly rather than burn the whole budget.
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{MaxAttempts: 6, BaseDelay: time.Hour, MaxDelay: time.Hour},
		OnRetry: func(RetryInfo) { cancel() },
	})
	start := time.Now()
	_, err := c.RespondStream(ctx, RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected an error after cancel, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel did not interrupt backoff promptly: %v", elapsed)
	}
	if n := hits(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (cancelled during first backoff)", n)
	}
}

func TestRetryDisabledWithSingleAttempt(t *testing.T) {
	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, streamFail })
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err == nil {
		t.Fatal("expected the failure to surface, got nil")
	}
	if n := hits(); n != 1 {
		t.Fatalf("server hit %d times, want 1 (retries disabled)", n)
	}
}

func TestDefaultRetryPolicyApplied(t *testing.T) {
	c := NewClient(ClientConfig{BaseURL: "http://example.invalid"})
	if c.retry != DefaultRetryPolicy() {
		t.Fatalf("default policy = %+v, want %+v", c.retry, DefaultRetryPolicy())
	}
}

func TestIsRetriableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{"connect", &Error{Code: "connect"}, true},
		{"upstream mid-stream", &Error{Code: "upstream_error", Stream: true}, true},
		{"truncated stream", &Error{Code: "stream_interrupted", Stream: true}, true},
		{"502", &Error{HTTPStatus: http.StatusBadGateway}, true},
		{"503", &Error{HTTPStatus: http.StatusServiceUnavailable}, true},
		{"504", &Error{HTTPStatus: http.StatusGatewayTimeout}, true},
		{"500 app error not retried", &Error{HTTPStatus: http.StatusInternalServerError}, false},
		{"429 rate limit", &Error{HTTPStatus: http.StatusTooManyRequests}, true},
		{"auth 401", &Error{HTTPStatus: http.StatusUnauthorized}, false},
		{"contract 400", &Error{HTTPStatus: http.StatusBadRequest}, false},
		{"protocol 426", &Error{HTTPStatus: http.StatusUpgradeRequired}, false},
		{"stream decode bug", &Error{Code: "stream_decode", Stream: true}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isRetriable(tc.err); got != tc.want {
			t.Errorf("%s: isRetriable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackoffHonorsRetryAfterAndCap(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 6, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	// Retry-After is HONOURED (not clamped down to the small backoff cap), so a server
	// rate-limit directive is respected rather than retried early.
	if d := p.backoff(0, 10*time.Second); d != 10*time.Second {
		t.Fatalf("retry-after honoured: got %v, want %v", d, 10*time.Second)
	}
	if d := p.backoff(0, 250*time.Millisecond); d != 250*time.Millisecond {
		t.Fatalf("retry-after passthrough: got %v, want %v", d, 250*time.Millisecond)
	}
	// ...but a pathological Retry-After is bounded so it can't freeze the turn.
	if d := p.backoff(0, time.Hour); d != maxRetryAfterWait {
		t.Fatalf("retry-after ceiling: got %v, want %v", d, maxRetryAfterWait)
	}
	// Exponential backoff with full jitter stays within (0, MaxDelay].
	for attempt := 0; attempt < 40; attempt++ {
		d := p.backoff(attempt, 0)
		if d <= 0 || d > p.MaxDelay {
			t.Fatalf("attempt %d: backoff %v out of (0, %v]", attempt, d, p.MaxDelay)
		}
	}
}

func TestRespondStream_MetaForwardedOnceAcrossRetries(t *testing.T) {
	// Two transient failures (each emits its own meta) then success: OnMeta must reach
	// the caller exactly once — from the committed attempt — not once per attempt.
	const failFirst = 2
	srv, _ := countingServer(t, func(n int) (int, string) {
		if n < failFirst {
			return http.StatusOK, streamFail
		}
		return http.StatusOK, streamOK
	})
	defer srv.Close()

	var metaCount int
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(6)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnMeta: func(StreamMeta) { metaCount++ },
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if metaCount != 1 {
		t.Fatalf("OnMeta fired %d times, want 1 (deferred until the committed attempt)", metaCount)
	}
}

func TestRespondStream_MetaForwardedOnceOnTerminalFailure(t *testing.T) {
	// Every attempt fails after its meta: the caller still gets meta exactly once (parity
	// with the pre-retry behaviour), never one per exhausted attempt.
	srv, _ := countingServer(t, func(int) (int, string) { return http.StatusOK, streamFail })
	defer srv.Close()

	var metaCount int
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnMeta: func(StreamMeta) { metaCount++ },
	})
	if err == nil {
		t.Fatal("expected terminal failure, got nil")
	}
	if metaCount != 1 {
		t.Fatalf("OnMeta fired %d times, want 1 (forwarded once on terminal failure)", metaCount)
	}
}
