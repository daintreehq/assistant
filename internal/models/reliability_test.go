package models

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestIsRetriableModelError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&apiError{status: 429}, true},
		{&apiError{status: 500}, true},
		{&apiError{status: 503}, true},
		{&apiError{status: 0}, true},    // connection error, no HTTP response
		{&apiError{status: 400}, false}, // bad request
		{&apiError{status: 401}, false}, // auth
		{context.Canceled, false},       // user abort never retried
		{context.DeadlineExceeded, true},
		{errors.New("misc"), false},
	}
	for _, c := range cases {
		if got := isRetriableModelError(c.err); got != c.want {
			t.Errorf("isRetriableModelError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestModelRetryDelayHonoursRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", "2")
	err := &apiError{status: 429, headers: h}
	if got := modelRetryDelayMs(0, err, ModelRetryPolicy); got != 2000 {
		t.Fatalf("retry-after seconds = %d ms, want 2000", got)
	}

	h2 := http.Header{}
	h2.Set("retry-after-ms", "750")
	err2 := &apiError{status: 429, headers: h2}
	if got := modelRetryDelayMs(0, err2, ModelRetryPolicy); got != 750 {
		t.Fatalf("retry-after-ms = %d, want 750", got)
	}

	// A pathological Retry-After is capped.
	h3 := http.Header{}
	h3.Set("retry-after-ms", "999999")
	err3 := &apiError{status: 429, headers: h3}
	if got := modelRetryDelayMs(0, err3, ModelRetryPolicy); got != maxRetryAfterMs {
		t.Fatalf("capped retry-after = %d, want %d", got, maxRetryAfterMs)
	}
}

func TestFullJitterDelayBounds(t *testing.T) {
	// Ceiling never exceeds maxMs and result stays in [0, ceiling].
	for attempt := 0; attempt < 20; attempt++ {
		for i := 0; i < 50; i++ {
			d := fullJitterDelay(attempt, 500, 10_000)
			if d < 0 || d > 10_000 {
				t.Fatalf("delay %d out of [0,10000] at attempt %d", d, attempt)
			}
		}
	}
	// attempt 0 ceiling is baseMs.
	for i := 0; i < 100; i++ {
		if d := fullJitterDelay(0, 500, 10_000); d > 500 {
			t.Fatalf("attempt0 delay %d > base 500", d)
		}
	}
}

func TestAbortableSleepCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := abortableSleep(ctx, 5_000); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestRetryModelCallStopsOnNonRetriable(t *testing.T) {
	calls := 0
	_, err := retryModelCall(context.Background(), ModelRetryPolicy, func() (int, error) {
		calls++
		return 0, &apiError{status: 400}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v — non-retriable must not retry", calls, err)
	}
}

func TestEstimateCostUsd(t *testing.T) {
	// glm-5p2: input 1.40, cached 0.26, output 4.40 per M. 1000 prompt (200 cached), 500 output.
	cost, ok := EstimateCostUsd("accounts/fireworks/models/glm-5p2", 1000, 500, 200)
	if !ok {
		t.Fatal("expected a known rate")
	}
	// fresh=800 → 800*1.40 + 200*0.26 + 500*4.40 = 1120 + 52 + 2200 = 3372
	// total = 3372 / 1e6 = 0.003372
	want := (800*1.40 + 200*0.26 + 500*4.40) / 1_000_000
	if diff := cost - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
	if _, ok := EstimateCostUsd("accounts/x/models/unknown-model", 10, 10, 0); ok {
		t.Fatal("unknown model must return ok=false")
	}
}

func TestBareModelID(t *testing.T) {
	if got := BareModelID("accounts/fireworks/models/glm-5p2"); got != "glm-5p2" {
		t.Fatalf("bare = %q", got)
	}
	if got := BareModelID("deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("bare = %q", got)
	}
}
