package models

import (
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
