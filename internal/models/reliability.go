// Transient-failure resilience primitives for the model client (port of the
// model-relevant slice of src/reliability.ts).
//
// A single 5xx, a dropped socket, or a momentarily hung request should not kill a
// turn. These are the small, dependency-free primitives that make calls resilient:
//   - bounded exponential backoff with FULL jitter (avoids retry thundering herds);
//   - a retryable-error predicate (a 429/5xx/connection error retries, a user
//     cancel or a 4xx never does);
//   - an abortable sleep so a backoff wait still tears down on the turn's cancel.
//
// Per-attempt timeouts are NOT modeled here: in Go each attempt derives a
// context.WithTimeout (60s chat/json, 300s stream) at the call site, so the
// "don't combine AbortSignals per attempt" Node #54614 caveat is moot.
//
// The MCP-read retry policy, the rate-limit OUTPUT-signature detector, and the
// MCP error predicate from reliability.ts belong to the MCP/watcher subsystems and
// are deliberately NOT ported here (see models.md §11).
package models

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// RetryPolicy is a bounded exponential-backoff retry budget. MaxRetries is the
// number of ADDITIONAL attempts after the first (so 3 ⇒ up to 4 total tries).
type RetryPolicy struct {
	MaxRetries  int
	BaseDelayMs int
	MaxDelayMs  int
}

// ModelRetryPolicy is the retry budget for Fireworks model calls: three retries,
// ~0.5s→10s with jitter — enough to ride out a brief 5xx blip or a connection
// reset without making a failing provider feel hung.
var ModelRetryPolicy = RetryPolicy{MaxRetries: 3, BaseDelayMs: 500, MaxDelayMs: 10_000}

// ModelRequestTimeoutMs is the per-attempt timeout for one-shot model calls
// (chat/json). A response that hasn't arrived in this window is a hung attempt.
const ModelRequestTimeoutMs = 60_000

// ModelStreamTimeoutMs is the per-attempt timeout for streaming model calls.
// Generous on purpose: a backstop against a truly hung stream, NOT a cap on a
// legitimately long one — killing a slow-but-progressing turn would be worse.
const ModelStreamTimeoutMs = 300_000

// maxRetryAfterMs caps an honoured Retry-After so a pathological header value
// can't park a turn for minutes; beyond this we fall back to jittered backoff.
const maxRetryAfterMs = 30_000

// fullJitterDelay is full-jitter exponential backoff: a uniform random delay in
// [0, min(maxMs, baseMs * 2^attempt)]. Full (not equal/decorrelated) jitter is
// the simplest choice that still spreads concurrent retriers across the window.
// attempt is 0-based (the first retry is attempt 0).
func fullJitterDelay(attempt, baseMs, maxMs int) int {
	exp := attempt
	if exp < 0 {
		exp = 0
	}
	// baseMs * 2^exp, but guard against overflow on large exponents by capping at
	// maxMs the instant the running ceiling exceeds it.
	ceiling := baseMs
	for i := 0; i < exp; i++ {
		ceiling *= 2
		if ceiling >= maxMs {
			ceiling = maxMs
			break
		}
	}
	if ceiling > maxMs {
		ceiling = maxMs
	}
	// floor(random()*(ceiling+1)) → a uniform integer in [0, ceiling].
	return rand.Intn(ceiling + 1)
}

// apiError carries an HTTP status from a failed model request so the retry
// classifier can distinguish a retriable 429/5xx from a fatal 4xx, and so a 429
// can honour a Retry-After header. A status of 0 means a connection-level error
// (dial/timeout) with no HTTP response — treated as retriable.
type apiError struct {
	status  int
	headers http.Header
	body    string
}

func (e *apiError) Error() string {
	if e.body != "" {
		return "fireworks api error " + strconv.Itoa(e.status) + ": " + e.body
	}
	return "fireworks api error " + strconv.Itoa(e.status)
}

// isModelAbort reports whether an error is a user-initiated cancellation. In Go
// this is a context cancellation (the AbortSignal equivalent) — never retried.
func isModelAbort(err error) bool {
	return errors.Is(err, context.Canceled)
}

// isRetriableModelError reports whether a model-call error is worth retrying: a
// 429 (rate limit), a 5xx, or a connection-level failure (status 0). A user abort
// and any other 4xx (bad request, auth) are NOT retriable.
func isRetriableModelError(err error) bool {
	if err == nil {
		return false
	}
	if isModelAbort(err) {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.status == 0 {
			return true // connection error / timeout, no HTTP response
		}
		return ae.status == 429 || ae.status >= 500
	}
	// A bare transport / mid-body read error carries no apiError but is exactly the
	// transient class the budget should ride out — the same way an SDK
	// APIConnectionError (status undefined) was retriable in TS. We classify:
	//   - a per-attempt timeout (context.DeadlineExceeded);
	//   - any net.Error / url.Error (dial failure, connection reset, etc.);
	//   - a truncated read (io.ErrUnexpectedEOF, or a plain io.EOF before the
	//     stream's [DONE] terminator) — including our own parseSSE truncation/
	//     malformed-payload errors, which wrap these.
	// context.Canceled is EXCLUDED above (isModelAbort): a user cancel must
	// propagate as CancelledError, never trigger a retry.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// A url.Error wrapping a cancellation is still a cancel — don't retry.
		return !errors.Is(urlErr, context.Canceled)
	}
	return false
}

// isRateLimitModelError reports a 429 specifically — distinguished from a plain
// 5xx so the backoff can honour a Retry-After header when the provider sends one.
func isRateLimitModelError(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status == 429
	}
	return false
}

// parseRetryAfterMs parses a Retry-After value (ms or HTTP-date) from response
// headers. Prefers the non-standard `retry-after-ms`. Returns (0,false) when
// nothing parseable is found.
func parseRetryAfterMs(h http.Header) (int, bool) {
	if h == nil {
		return 0, false
	}
	if ms := h.Get("retry-after-ms"); ms != "" {
		if n, err := strconv.ParseFloat(ms, 64); err == nil && n >= 0 {
			return int(n), true
		}
	}
	if ra := h.Get("retry-after"); ra != "" {
		// Numeric seconds → *1000.
		if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs >= 0 {
			return int(secs * 1000), true
		}
		// Otherwise an HTTP-date → max(0, when-now).
		if when, err := http.ParseTime(ra); err == nil {
			d := time.Until(when)
			if d < 0 {
				d = 0
			}
			return int(d / time.Millisecond), true
		}
	}
	return 0, false
}

// modelRetryDelayMs is the delay before retrying a failed model attempt: an
// honoured (capped) Retry-After on a 429, otherwise full-jitter backoff.
func modelRetryDelayMs(attempt int, err error, policy RetryPolicy) int {
	if isRateLimitModelError(err) {
		var ae *apiError
		if errors.As(err, &ae) {
			if ra, ok := parseRetryAfterMs(ae.headers); ok {
				if ra > maxRetryAfterMs {
					return maxRetryAfterMs
				}
				return ra
			}
		}
	}
	return fullJitterDelay(attempt, policy.BaseDelayMs, policy.MaxDelayMs)
}

// abortableSleep waits ms, or returns ctx.Err() the instant the context is
// cancelled — so a backoff wait between attempts is itself cancellable.
func abortableSleep(ctx context.Context, ms int) error {
	if ms <= 0 {
		// Still honour an already-cancelled context.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryModelCall retries a one-shot model call (chat/json) under policy, sleeping
// with backoff between attempts. Stops immediately on a non-retriable error or a
// cancelled context; the final attempt's error propagates so the caller can
// normalise an abort to CancelledError.
//
// Deliberately NOT used for streaming: once a stream has emitted a token,
// re-running it would duplicate output into the immutable transcript, so
// chatStream owns a bespoke pre-token-only loop.
func retryModelCall[T any](ctx context.Context, policy RetryPolicy, attempt func() (T, error)) (T, error) {
	var zero T
	for i := 0; ; i++ {
		res, err := attempt()
		if err == nil {
			return res, nil
		}
		if i >= policy.MaxRetries {
			return zero, err
		}
		if ctx.Err() != nil {
			return zero, err
		}
		if !isRetriableModelError(err) {
			return zero, err
		}
		if serr := abortableSleep(ctx, modelRetryDelayMs(i, err, policy)); serr != nil {
			return zero, serr
		}
	}
}
