package backend

import (
	"context"
	"encoding/json"
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

func TestRespondStream_RetriesUpstreamTimeout(t *testing.T) {
	const timeoutStream = "event: meta\ndata: {}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_timeout\",\"message\":\"provider timed out\"}}\n\n"
	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, timeoutStream
		}
		return http.StatusOK, streamOK
	})
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("expected success after upstream timeout retry, got %v", err)
	}
	if res.Message.Content != "hi" {
		t.Fatalf("content = %q, want hi", res.Message.Content)
	}
	if got := hits(); got != 2 {
		t.Fatalf("server hit %d times, want 2", got)
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
		return http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"bad","message":"nope"}}`
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

// The CLI covers only the CLI↔backend hop — the backend owns provider retries — so
// the default budget is exactly one retry, and a server Retry-After can stall a turn
// at most 10s. These are alignment invariants, not tunables; lock them.
func TestDefaultRetryAlignment(t *testing.T) {
	if got := DefaultRetryPolicy().MaxAttempts; got != 2 {
		t.Fatalf("default MaxAttempts = %d, want 2 (initial attempt + 1 retry)", got)
	}
	if maxRetryAfterWait != 10*time.Second {
		t.Fatalf("maxRetryAfterWait = %v, want 10s", maxRetryAfterWait)
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
		{"idle stream abort", &Error{Code: "stream_idle_timeout", Stream: true}, true},
		{"oversized line", &Error{Code: "stream_line_too_large", Stream: true}, false},
		{"oversized event", &Error{Code: "stream_event_too_large", Stream: true}, false},
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
	if d := p.backoff(0, 8*time.Second); d != 8*time.Second {
		t.Fatalf("retry-after honoured: got %v, want %v", d, 8*time.Second)
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

	var metaCount, rawMetaCount int
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(6)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnRawMeta: func(StreamMeta) { rawMetaCount++ },
		OnMeta:    func(StreamMeta) { metaCount++ },
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if metaCount != 1 {
		t.Fatalf("OnMeta fired %d times, want 1 (deferred until the committed attempt)", metaCount)
	}
	if rawMetaCount != failFirst+1 {
		t.Fatalf("OnRawMeta fired %d times, want %d (once per HTTP attempt)", rawMetaCount, failFirst+1)
	}
}

func TestRespondStream_SkillLoadDeduplicatedAcrossRetries(t *testing.T) {
	const firstMeta = "event: meta\n" +
		"data: {\"skills\":{\"newly_loaded\":[{\"id\":\"multi_agent\",\"title\":\"Multi-agent orchestration\"}]}}\n\n"
	const changedMeta = "event: meta\n" +
		"data: {\"skills\":{\"newly_loaded\":[{\"id\":\" multi_agent \",\"title\":\"Changed title\"}]}}\n\n"
	const failWithSkill = firstMeta +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"
	const changedFailWithSkill = changedMeta +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"
	const okWithSkill = changedMeta +
		"event: delta\ndata: {\"content\":\"hi\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	const failFirst = 2
	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, failWithSkill
		}
		if n < failFirst {
			return http.StatusOK, changedFailWithSkill
		}
		return http.StatusOK, okWithSkill
	})
	defer srv.Close()

	var callbacks [][]SkillRef
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(6)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnSkillLoaded: func(refs []SkillRef) {
			callbacks = append(callbacks, append([]SkillRef(nil), refs...))
		},
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := hits(); got != failFirst+1 {
		t.Fatalf("server hit %d times, want %d", got, failFirst+1)
	}
	if len(callbacks) != 1 {
		t.Fatalf("OnSkillLoaded fired %d times, want 1: %+v", len(callbacks), callbacks)
	}
	if got := callbacks[0]; len(got) != 1 || got[0].ID != "multi_agent" || got[0].Title != "Multi-agent orchestration" {
		t.Fatalf("skill callback refs = %+v", got)
	}
}

func TestRespondStream_RetryAdoptsMetaStateAndKeepsSkillSelection(t *testing.T) {
	const selectedState = "dst1.selected"
	const firstAttempt = "event: meta\n" +
		"data: {\"state\":\"dst1.selected\",\"skills\":{\"newly_loaded\":[{\"id\":\"skill_a\",\"title\":\"Skill A\"}]}}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"
	const stableRetry = "event: meta\n" +
		"data: {\"state\":\"dst1.selected\",\"skills\":{\"newly_loaded\":[]}}\n\n" +
		"event: delta\ndata: {\"content\":\"hi\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	// This branch models what would happen if the second POST omitted state and made
	// the backend run selection again: a different skill would be reported.
	const reselectedRetry = "event: meta\n" +
		"data: {\"state\":\"dst1.different\",\"skills\":{\"newly_loaded\":[{\"id\":\"skill_b\",\"title\":\"Skill B\"}]}}\n\n" +
		"event: delta\ndata: {\"content\":\"wrong selection\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	var (
		mu         sync.Mutex
		requests   []RespondRequest
		reselected bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RespondRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		n := len(requests)
		requests = append(requests, req)
		mu.Unlock()

		body := firstAttempt
		if n > 0 {
			if req.State != nil && *req.State == selectedState {
				body = stableRetry
			} else {
				mu.Lock()
				reselected = true
				mu.Unlock()
				body = reselectedRetry
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	var loaded []string
	var committedMeta StreamMeta
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnSkillLoaded: func(refs []SkillRef) {
			for _, ref := range refs {
				loaded = append(loaded, ref.ID)
			}
		},
		OnMeta: func(m StreamMeta) { committedMeta = m },
	})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if res.Message.Content != "hi" {
		t.Fatalf("content = %q, want hi", res.Message.Content)
	}

	mu.Lock()
	gotRequests := append([]RespondRequest(nil), requests...)
	gotReselected := reselected
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("request count = %d, want 2", len(gotRequests))
	}
	if gotRequests[0].State != nil {
		t.Fatalf("first request state = %q, want omitted", *gotRequests[0].State)
	}
	if gotRequests[1].State == nil || *gotRequests[1].State != selectedState {
		t.Fatalf("retry state = %v, want %q", gotRequests[1].State, selectedState)
	}
	if gotReselected {
		t.Fatal("retry omitted the selected state and reran the selector")
	}
	if len(loaded) != 1 || loaded[0] != "skill_a" {
		t.Fatalf("surfaced skill loads = %v, want only skill_a", loaded)
	}
	if committedMeta.State != selectedState || len(committedMeta.Skills.NewlyLoaded) != 0 {
		t.Fatalf("committed meta = %+v, want stable retry meta with no new skill", committedMeta)
	}
}

func TestRespondStream_TerminalPreMetaRetryFlushesLastReceivedMeta(t *testing.T) {
	const firstAttempt = "event: meta\n" +
		"data: {\"state\":\"dst1.selected\",\"skills\":{\"newly_loaded\":[{\"id\":\"skill_a\",\"title\":\"Skill A\"}]}}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"boom\"}}\n\n"

	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, firstAttempt
		}
		// The retry budget ends on an HTTP failure before this attempt can emit meta.
		return http.StatusBadGateway, `{"error":{"code":"gateway_unavailable","message":"down"}}`
	})
	defer srv.Close()

	var metas []StreamMeta
	var loaded []string
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(2)})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnMeta: func(m StreamMeta) { metas = append(metas, m) },
		OnSkillLoaded: func(refs []SkillRef) {
			for _, ref := range refs {
				loaded = append(loaded, ref.ID)
			}
		},
	})
	if err == nil {
		t.Fatal("expected terminal gateway failure")
	}
	if got := hits(); got != 2 {
		t.Fatalf("server hit %d times, want 2", got)
	}
	if len(metas) != 1 || metas[0].State != "dst1.selected" {
		t.Fatalf("OnMeta calls = %+v, want last received state once", metas)
	}
	if len(loaded) != 1 || loaded[0] != "skill_a" {
		t.Fatalf("OnSkillLoaded calls = %v, want skill_a once", loaded)
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
