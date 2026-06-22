package models

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// parseRetryAfterMs prefers the non-standard retry-after-ms, then numeric
// delta-seconds, and returns ok=false when nothing parseable is present.
func TestParseRetryAfterMsPreference(t *testing.T) {
	hMs := http.Header{}
	hMs.Set("retry-after-ms", "1500")
	if got, ok := parseRetryAfterMs(hMs); !ok || got != 1500 {
		t.Fatalf("retry-after-ms: got %d ok=%v, want 1500", got, ok)
	}

	hSec := http.Header{}
	hSec.Set("retry-after", "2")
	if got, ok := parseRetryAfterMs(hSec); !ok || got != 2000 {
		t.Fatalf("retry-after seconds: got %d ok=%v, want 2000", got, ok)
	}

	// retry-after-ms wins when both are present.
	hBoth := http.Header{}
	hBoth.Set("retry-after-ms", "750")
	hBoth.Set("retry-after", "9")
	if got, ok := parseRetryAfterMs(hBoth); !ok || got != 750 {
		t.Fatalf("both headers: got %d ok=%v, want 750 (ms preferred)", got, ok)
	}

	// Nothing parseable → ok=false.
	if _, ok := parseRetryAfterMs(nil); ok {
		t.Error("nil headers must be ok=false")
	}
	if _, ok := parseRetryAfterMs(http.Header{}); ok {
		t.Error("empty headers must be ok=false")
	}
	hBad := http.Header{}
	hBad.Set("retry-after", "not-a-date")
	if _, ok := parseRetryAfterMs(hBad); ok {
		t.Error("unparseable retry-after must be ok=false")
	}
}

// isRateLimitModelError flags a 429 specifically, distinct from a plain 5xx, so the
// backoff can honour a Retry-After header only when the provider rate-limited us.
func TestIsRateLimitModelError(t *testing.T) {
	if !isRateLimitModelError(&apiError{status: 429}) {
		t.Error("429 must be flagged as rate-limit")
	}
	if isRateLimitModelError(&apiError{status: 500}) {
		t.Error("500 must NOT be flagged as rate-limit")
	}
	if isRateLimitModelError(&apiError{status: 0}) {
		t.Error("connection error must NOT be flagged as rate-limit")
	}
}

// wrapExhaustedRateLimit converts a budget-exhausted 429 into the exported
// *RateLimitedError (so it can cross into the agent layer) and passes every other
// error through untouched, including nil.
func TestWrapExhaustedRateLimit(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after-ms", "1200")

	// A 429 with a Retry-After header → RateLimitedError carrying RetryAfterMs.
	var rl *RateLimitedError
	got := wrapExhaustedRateLimit(&apiError{status: 429, headers: h})
	if !errors.As(got, &rl) {
		t.Fatalf("429 must wrap to *RateLimitedError, got %T", got)
	}
	if rl.RetryAfterMs != 1200 {
		t.Errorf("RetryAfterMs = %d, want 1200", rl.RetryAfterMs)
	}
	if rl.Code() != "MODEL_RATE_LIMITED" {
		t.Errorf("Code = %q, want MODEL_RATE_LIMITED", rl.Code())
	}
	if rl.Error() != "provider quota/throughput exceeded" {
		t.Errorf("Error = %q", rl.Error())
	}

	// A 429 with no header → RetryAfterMs 0.
	rl = nil
	if got := wrapExhaustedRateLimit(&apiError{status: 429}); !errors.As(got, &rl) || rl.RetryAfterMs != 0 {
		t.Fatalf("429 no-header: got %v (rl=%+v)", got, rl)
	}

	// Non-429 errors pass through unchanged (same interface value).
	for _, e := range []error{
		&apiError{status: 500},
		&apiError{status: 0},
		context.DeadlineExceeded,
		errors.New("misc"),
	} {
		if out := wrapExhaustedRateLimit(e); out != e {
			t.Errorf("wrapExhaustedRateLimit(%v) = %v, want unchanged", e, out)
		}
	}
	// nil stays nil.
	if out := wrapExhaustedRateLimit(nil); out != nil {
		t.Errorf("nil must pass through, got %v", out)
	}
}

// retryModelCall surfaces a final exhausted 429 as the exported *RateLimitedError so
// the agent layer can classify it; the budget is honoured (MaxRetries+1 attempts).
func TestRetryModelCallExhausted429Wraps(t *testing.T) {
	fast := RetryPolicy{MaxRetries: 2, BaseDelayMs: 0, MaxDelayMs: 0}
	calls := 0
	_, err := retryModelCall(context.Background(), fast, func() (int, error) {
		calls++
		return 0, &apiError{status: 429}
	})
	if calls != fast.MaxRetries+1 {
		t.Fatalf("attempts = %d, want %d", calls, fast.MaxRetries+1)
	}
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("exhausted 429 must surface *RateLimitedError, got %T (%v)", err, err)
	}
}

// modelRetryDelayMs honours a Retry-After only on a 429; a 5xx falls back to
// full-jitter backoff (which never exceeds the policy ceiling).
func TestModelRetryDelayMsOnlyHonoursRetryAfterFor429(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after-ms", "2000")

	// 429 honours the header.
	if got := modelRetryDelayMs(0, &apiError{status: 429, headers: h}, ModelRetryPolicy); got != 2000 {
		t.Fatalf("429 delay = %d, want 2000", got)
	}
	// A 5xx ignores the header and uses jittered backoff bounded by the policy max.
	for i := 0; i < 50; i++ {
		if got := modelRetryDelayMs(0, &apiError{status: 503, headers: h}, ModelRetryPolicy); got < 0 || got > ModelRetryPolicy.MaxDelayMs {
			t.Fatalf("5xx delay = %d out of [0,%d]", got, ModelRetryPolicy.MaxDelayMs)
		}
	}
}
