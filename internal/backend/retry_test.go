package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The CLI covers only the CLI↔backend hop — the backend owns provider retries — but
// the budget must be long enough to RIDE OUT a backend restart, settling into a
// patient 10–15s poll rather than giving up after one fast replay against a socket
// that is still closed. These are alignment invariants, not tunables; lock them.
func TestDefaultRetryAlignment(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.MaxAttempts != 10 {
		t.Fatalf("default MaxAttempts = %d, want 10 (initial attempt + 9 retries)", p.MaxAttempts)
	}
	// Assert the fields directly: the band checks below pass for a whole family of
	// policies (a 1s base reaches the same ceiling), so they alone would not catch a
	// changed ramp.
	if p.BaseDelay != 500*time.Millisecond {
		t.Fatalf("default BaseDelay = %v, want 500ms", p.BaseDelay)
	}
	if p.MaxDelay != 15*time.Second {
		t.Fatalf("default MaxDelay = %v, want 15s", p.MaxDelay)
	}
	if p.MaxElapsed != 2*time.Minute {
		t.Fatalf("default MaxElapsed = %v, want 2m (the restart-recovery window)", p.MaxElapsed)
	}
	if maxRetryAfterWait != p.MaxDelay {
		t.Fatalf("maxRetryAfterWait = %v, want it pinned to MaxDelay (%v)", maxRetryAfterWait, p.MaxDelay)
	}
	// The steady-state poll: once the exponential ramp saturates, every wait must
	// land in the intended 10–15s band (jitter floor 2/3 of the cap).
	for attempt := 5; attempt < 20; attempt++ {
		d := p.backoff(attempt, 0)
		if d < 10*time.Second || d > 15*time.Second {
			t.Fatalf("saturated attempt %d: backoff %v outside the 10–15s band", attempt, d)
		}
	}
	// ...and the whole budget spans roughly a minute of patience, not three.
	var total time.Duration
	for attempt := 0; attempt < p.MaxAttempts-1; attempt++ {
		total += p.backoff(attempt, 0)
	}
	if total < 40*time.Second || total > 90*time.Second {
		t.Fatalf("total backoff budget %v, want roughly 40–90s", total)
	}
	// The elapsed window must never truncate the case the budget exists FOR: against a
	// refused socket every attempt fails in microseconds, so the full retry chain has
	// to fit inside MaxElapsed with room to spare. If this ever fails, a backend
	// restart would be cut short by the very guard meant to bound the pathological case.
	if p.exhausted(total, 0) {
		t.Fatalf("MaxElapsed %v cannot fit the full backoff chain (%v)", p.MaxElapsed, total)
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

		// The split upstream taxonomy. Each deterministic code is asserted on BOTH
		// transports, because that is precisely where the old classification went wrong:
		// the same condition arrives with its HTTP status pre-stream and with
		// HTTPStatus 0 mid-stream (the backend emits `meta` before it opens the upstream
		// stream), and a status-based rule silently reverses its answer between the two.
		{"invalid provider key", &Error{HTTPStatus: 401, Code: CodeProviderInvalidAPIKey}, false},
		{"invalid provider key mid-stream", &Error{Code: CodeProviderInvalidAPIKey, Stream: true}, false},
		{"no credit", &Error{HTTPStatus: 402, Code: CodeProviderInsufficientCredit}, false},
		{"no credit mid-stream", &Error{Code: CodeProviderInsufficientCredit, Stream: true}, false},
		{"key forbidden", &Error{HTTPStatus: 403, Code: CodeProviderKeyForbidden}, false},
		{"key forbidden mid-stream", &Error{Code: CodeProviderKeyForbidden, Stream: true}, false},

		// A routing dead end is a 503, so the status switch would call it transient and
		// replay it through the whole budget to re-derive the same empty endpoint pool.
		{"routing dead end", &Error{HTTPStatus: http.StatusServiceUnavailable, Code: CodeUpstreamNoCompliantProvider}, false},
		{"routing dead end mid-stream", &Error{Code: CodeUpstreamNoCompliantProvider, Stream: true}, false},

		// Our bug: 502, which the status switch would also replay.
		{"request rejected", &Error{HTTPStatus: http.StatusBadGateway, Code: CodeUpstreamRequestRejected}, false},
		{"protocol error", &Error{HTTPStatus: http.StatusBadGateway, Code: CodeUpstreamProtocolError}, false},
		{"protocol error mid-stream", &Error{Code: CodeUpstreamProtocolError, Stream: true}, false},

		// The transient half of the old `upstream_error` blob must STILL be replayed.
		// A provider outage arriving mid-stream under its precise new name would
		// otherwise have quietly stopped being retried the day the backend split the
		// taxonomy — a regression with no symptom other than more failed turns.
		{"upstream unavailable", &Error{HTTPStatus: http.StatusServiceUnavailable, Code: CodeUpstreamUnavailable}, true},
		{"upstream unavailable mid-stream", &Error{Code: CodeUpstreamUnavailable, Stream: true}, true},
		{"upstream timeout mid-stream", &Error{Code: CodeUpstreamTimeout, Stream: true}, true},
		{"upstream rate limit mid-stream", &Error{Code: CodeUpstreamRateLimited, Stream: true}, true},

		// The deterministic gate must sit ABOVE IsRateLimited(), which ORs in a bare
		// `rate_limit_error` type. Without this case, moving the gate below it would
		// still pass the whole table while quietly making an unfundable account
		// retryable — the single most expensive misclassification in the set.
		{"deterministic code wearing a rate-limit type", &Error{Type: "rate_limit_error", Code: CodeProviderInsufficientCredit}, false},
		{"request rejected mid-stream", &Error{Code: CodeUpstreamRequestRejected, Stream: true}, false},
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
	// Exponential backoff with jitter stays within (0, MaxDelay], and lands in
	// [d·2/3, d] of THAT attempt's own pre-jitter delay — a flat floor of
	// BaseDelay·2/3 would pass even if the ramp collapsed to a constant.
	for attempt := 0; attempt < 40; attempt++ {
		want := p.BaseDelay << min(attempt, 40) // pre-jitter, before the cap
		if want <= 0 || want > p.MaxDelay {
			want = p.MaxDelay
		}
		d := p.backoff(attempt, 0)
		if d <= 0 || d > p.MaxDelay {
			t.Fatalf("attempt %d: backoff %v out of (0, %v]", attempt, d, p.MaxDelay)
		}
		if d < want*2/3 || d > want {
			t.Fatalf("attempt %d: backoff %v outside the jitter window [%v, %v]", attempt, d, want*2/3, want)
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

// jsonServer counts hits on a JSON endpoint and lets the test script per-attempt
// responses. Unlike countingServer it speaks application/json, so it exercises the
// doJSON path rather than the SSE parser.
func jsonServer(t *testing.T, handler func(n int) (status int, body string)) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := count
		count++
		mu.Unlock()
		status, body := handler(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
}

const taskOK = `{"id":"task_1","object":"daintree.task.result","task":"checkpoint","model":"m","output":{"goal":"g"},"finish_reason":"stop","usage":{"total_tokens":5},"prompt_version":"checkpoint"}`

// The JSON endpoints (tasks / capabilities / health / ready) went entirely un-retried
// before: a backend restart mid-turn failed a /compact checkpoint or a watcher judge
// outright, even though the call is replay-safe. Pin that they now ride it out — and
// that the OnTask observability hook still reports ONE round trip for the whole
// retried call, not one per attempt.
func TestDoJSONRetriesTransientFailure(t *testing.T) {
	const failFirst = 3
	srv, hits := jsonServer(t, func(n int) (int, string) {
		if n < failFirst {
			return http.StatusServiceUnavailable, `{"error":{"type":"api_error","code":"unavailable","message":"restarting"}}`
		}
		return http.StatusOK, taskOK
	})

	var tasks []TaskTraceInfo
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   fastRetry(5),
		OnTask:  func(info TaskTraceInfo) { tasks = append(tasks, info) },
	})

	out, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "t"})
	if err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if out.Goal != "g" {
		t.Fatalf("output goal = %q, want %q", out.Goal, "g")
	}
	if got := hits(); got != failFirst+1 {
		t.Fatalf("server hit %d times, want %d", got, failFirst+1)
	}
	if len(tasks) != 1 || tasks[0].Err != nil {
		t.Fatalf("OnTask = %+v, want exactly one successful round trip for the whole call", tasks)
	}
}

// A deterministic rejection must fail on the first attempt: replaying a contract bug
// burns the budget to arrive at the identical 400.
func TestDoJSONDoesNotRetryContractError(t *testing.T) {
	srv, hits := jsonServer(t, func(int) (int, string) {
		return http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"bad_field","message":"nope"}}`
	})
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(5)})
	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("expected the 400 to surface, got nil")
	}
	if got := hits(); got != 1 {
		t.Fatalf("server hit %d times, want 1 (contract errors are not retried)", got)
	}
}

// The retry loop is bounded by the CALLER's deadline: a boot handshake or a /doctor
// probe with a 3s budget must not be extended into a minute of patient polling.
func TestDoJSONRetriesStopAtCallerDeadline(t *testing.T) {
	srv, hits := jsonServer(t, func(int) (int, string) {
		return http.StatusBadGateway, `{"error":{"type":"api_error","code":"bad_gateway","message":"down"}}`
	})
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 50, BaseDelay: 20 * time.Millisecond, MaxDelay: 20 * time.Millisecond}})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := c.Health(ctx); err == nil {
		t.Fatal("expected failure, got nil")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("retry loop ran %v past a 120ms caller deadline", elapsed)
	}
	// The budget allowed ~50 attempts; the deadline must have cut it far short.
	if got := hits(); got == 0 || got > 20 {
		t.Fatalf("server hit %d times, want a handful before the deadline cut in", got)
	}
}

// The per-call cue that keeps a minute-long retry chain from reading as a hang.
func TestRespondStream_OnRetryCallbackFires(t *testing.T) {
	srv, _ := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, streamFail
		}
		return http.StatusOK, streamOK
	})
	defer srv.Close()

	var infos []RetryInfo
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
		OnRetry: func(info RetryInfo) { infos = append(infos, info) },
	}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("OnRetry fired %d times, want 1", len(infos))
	}
	if infos[0].Attempt != 0 || infos[0].MaxAttempts != 3 || infos[0].Op != "respond" || infos[0].Err == nil {
		t.Fatalf("RetryInfo = %+v, want attempt 0 of 3 on \"respond\" with the cause set", infos[0])
	}
}

// Diagnostics opt out: /doctor probes must report the hop's current state instantly
// rather than spend their whole timeout budget replaying a dead socket.
func TestWithoutRetryMakesCallsOneShot(t *testing.T) {
	jsonSrv, jsonHits := jsonServer(t, func(int) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"type":"api_error","code":"unavailable","message":"down"}}`
	})
	c := NewClient(ClientConfig{BaseURL: jsonSrv.URL, Retry: fastRetry(5)})
	if err := c.Health(backendWithoutRetryCtx()); err == nil {
		t.Fatal("expected the 503 to surface, got nil")
	}
	if got := jsonHits(); got != 1 {
		t.Fatalf("json endpoint hit %d times, want 1 (probe is one-shot)", got)
	}

	streamSrv, streamHits := countingServer(t, func(int) (int, string) { return http.StatusOK, streamFail })
	defer streamSrv.Close()
	sc := NewClient(ClientConfig{BaseURL: streamSrv.URL, Retry: fastRetry(5)})
	if _, err := sc.RespondStream(backendWithoutRetryCtx(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("expected the stream failure to surface, got nil")
	}
	if got := streamHits(); got != 1 {
		t.Fatalf("respond hit %d times, want 1 (probe is one-shot)", got)
	}

	// The marker is opt-in: an unmarked context keeps the retries.
	if retriesDisabled(context.Background()) {
		t.Fatal("a plain context must not disable retries")
	}
}

func backendWithoutRetryCtx() context.Context { return WithoutRetry(context.Background()) }

// A replayed POST must carry the FULL body every time. The body is marshaled once and
// each attempt wraps it in a fresh reader; reusing one consumed reader would send an
// empty document on every retry — and the server would answer a structurally valid
// but wrong request, so nothing downstream would notice.
func TestDoJSONReplaysFullBodyEveryAttempt(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"api_error","code":"service_unavailable","message":"warming"}}`)
			return
		}
		_, _ = io.WriteString(w, taskOK)
	}))
	t.Cleanup(srv.Close)

	var infos []RetryInfo
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   fastRetry(5),
		OnRetry: func(info RetryInfo) { infos = append(infos, info) },
	})
	if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "a distinctive transcript"}); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, "a distinctive transcript") {
			t.Fatalf("attempt %d body did not carry the payload: %q", i, b)
		}
		if b != bodies[0] {
			t.Fatalf("attempt %d body differs from the first attempt", i)
		}
	}
	// The JSON path must report which call is being replayed — with every endpoint
	// retried, an `op`-less log line can't tell a stalled turn from a stalled task.
	if len(infos) != 2 {
		t.Fatalf("OnRetry fired %d times, want 2", len(infos))
	}
	if infos[0].Op != "POST /v1/daintree/tasks" || infos[0].MaxAttempts != 5 {
		t.Fatalf("RetryInfo = %+v, want op=POST /v1/daintree/tasks maxAttempts=5", infos[0])
	}
}

// The backend reuses 502 for an application VERDICT: `task_output_invalid` means the
// model already ran and its output could not be parsed, and the backend's own comment
// says to read the terminal a different way rather than retry. Replaying it would burn
// a full model call per attempt to reach the identical answer.
func TestDoJSONDoesNotRetryApplicationVerdicts(t *testing.T) {
	for _, code := range []string{"task_output_invalid", "upstream_error", "internal_error"} {
		t.Run(code, func(t *testing.T) {
			srv, hits := jsonServer(t, func(int) (int, string) {
				return http.StatusBadGateway, `{"error":{"type":"api_error","code":"` + code + `","message":"verdict"}}`
			})
			c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(5)})
			if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "t"}); err == nil {
				t.Fatal("expected the verdict to surface, got nil")
			}
			if got := hits(); got != 1 {
				t.Fatalf("server hit %d times, want 1 (an application verdict is terminal)", got)
			}
		})
	}

	// ...but a bare 502 with no application code is a genuine gateway failure and is
	// still replayed, as is the warming-backend 503.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"bare gateway 502", `{"error":{"type":"api_error","message":"bad gateway"}}`},
		{"warming backend", `{"error":{"type":"api_error","code":"service_unavailable","message":"not ready"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := http.StatusBadGateway
			if strings.Contains(tc.body, "service_unavailable") {
				status = http.StatusServiceUnavailable
			}
			srv, hits := jsonServer(t, func(n int) (int, string) {
				if n == 0 {
					return status, tc.body
				}
				return http.StatusOK, taskOK
			})
			c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(5)})
			if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "t"}); err != nil {
				t.Fatalf("RunCheckpoint: %v", err)
			}
			if got := hits(); got != 2 {
				t.Fatalf("server hit %d times, want 2 (transient → replayed)", got)
			}
		})
	}
}

// The mid-stream contract is deliberately DIFFERENT from the pre-stream one: an
// `upstream_error` arriving after the 200 but before any visible token is the exact
// transient blip this retry layer was built for, so excluding it pre-stream must not
// leak into the stream path.
func TestStreamUpstreamErrorStillRetriedAfterVerdictExclusion(t *testing.T) {
	if !isRetriable(&Error{Code: "upstream_error", Stream: true}) {
		t.Fatal("mid-stream upstream_error must stay retriable")
	}
	if isRetriable(&Error{Code: "upstream_error", HTTPStatus: http.StatusBadGateway}) {
		t.Fatal("pre-stream upstream_error is an application verdict, not a gateway blip")
	}
}

// The doctor paths mark the context and THEN wrap it in a timeout. Pin the real
// ordering: context values must survive WithTimeout, or the opt-out silently stops
// working and every probe goes back to burning its budget.
func TestWithoutRetrySurvivesWithTimeoutWrapping(t *testing.T) {
	srv, hits := jsonServer(t, func(int) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"type":"api_error","code":"service_unavailable","message":"down"}}`
	})
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(5)})

	ctx, cancel := context.WithTimeout(WithoutRetry(context.Background()), 5*time.Second)
	defer cancel()
	if err := c.Health(ctx); err == nil {
		t.Fatal("expected failure, got nil")
	}
	if got := hits(); got != 1 {
		t.Fatalf("server hit %d times, want 1 (WithoutRetry must survive WithTimeout)", got)
	}
}

// The elapsed window bounds the WHOLE call. Without it, ten slow-failing attempts plus
// their backoff could stall a deadline-less caller (compaction, extraction) for many
// minutes even though the attempt budget looks like "about a minute".
func TestRetryStopsAtElapsedBudget(t *testing.T) {
	srv, hits := jsonServer(t, func(int) (int, string) {
		return http.StatusServiceUnavailable, `{"error":{"type":"api_error","code":"service_unavailable","message":"down"}}`
	})
	// A huge attempt budget, but an elapsed window that only affords a couple of waits.
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{
		MaxAttempts: 50, BaseDelay: 30 * time.Millisecond, MaxDelay: 30 * time.Millisecond, MaxElapsed: 100 * time.Millisecond,
	}})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected failure, got nil")
	}
	if got := hits(); got == 0 || got > 6 {
		t.Fatalf("server hit %d times, want a handful before the elapsed budget closed", got)
	}

	// Zero means unbounded — the attempt count is then the only limit.
	if (RetryPolicy{MaxElapsed: 0}).exhausted(time.Hour, time.Hour) {
		t.Fatal("MaxElapsed 0 must mean unbounded")
	}
	if !(RetryPolicy{MaxElapsed: time.Second}).exhausted(900*time.Millisecond, 200*time.Millisecond) {
		t.Fatal("a wait that would overrun the window must stop the loop")
	}
}
