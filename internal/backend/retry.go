package backend

import (
	"context"
	"math/rand"
	"net/http"
	"time"
)

// Retry defaults. The streamed respond turn is the only retried path. This is a
// SECOND line of defence — the backend owns retrying its upstream provider (the
// classic DeepSeek 502 blips); the CLI retry covers ONLY the CLI↔backend hop (a
// dropped connection, a backend restart, a stream truncated before any content), so
// ONE retry is enough: anything a second local replay would fix, the first replay
// already fixed. The conversation prefix is unchanged across attempts and the backend
// prefix-caches it; after an eager meta, the retry also carries that attempt's signed
// state so skill selection is reused.
const (
	defaultMaxAttempts = 2 // initial attempt + 1 retry (backend owns provider retries)
	defaultBaseDelay   = 400 * time.Millisecond
	defaultMaxDelay    = 5 * time.Second
	// maxRetryAfterWait bounds how long a server-provided Retry-After can stall a
	// turn. We HONOUR Retry-After (from an HTTP response or SSE error) rather than
	// clamping it down to the small jittered-backoff cap — retrying earlier than the
	// server asked just burns the budget — but a pathological value can't freeze the
	// cockpit: with a single-retry budget, anything past 10s is better surfaced to
	// the user as a failure than served as a silent stall.
	maxRetryAfterWait = 10 * time.Second
)

// RetryPolicy tunes transient-failure retries for RespondStream. The zero value is
// not usable directly — NewClient substitutes DefaultRetryPolicy when MaxAttempts is
// 0. Set MaxAttempts to 1 to disable retries entirely (a single attempt).
type RetryPolicy struct {
	MaxAttempts int           // total attempts INCLUDING the first; 1 = no retries
	BaseDelay   time.Duration // backoff for the first retry (doubles thereafter)
	MaxDelay    time.Duration // cap on a single backoff (0 = uncapped)
}

// DefaultRetryPolicy is the production policy: initial attempt + 1 backoff retry
// (the CLI↔backend hop only — the backend owns provider retries), capped at 5s per
// wait.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: defaultMaxAttempts, BaseDelay: defaultBaseDelay, MaxDelay: defaultMaxDelay}
}

// RetryInfo is handed to ClientConfig.OnRetry just before each backoff sleep, purely
// for observability (debug log / metrics). Attempt is 0-based: 0 is the first failure
// (about to make the first retry).
type RetryInfo struct {
	Attempt int
	Delay   time.Duration
	Err     error
}

// isRetriable reports whether a backend failure is a transient class worth retrying.
// Deterministic failures (auth, contract bugs, protocol mismatch) are never retried —
// a replay would fail identically. The caller separately guarantees no visible content
// has streamed yet, so replaying the turn cannot duplicate output.
func isRetriable(e *Error) bool {
	if e == nil {
		return false
	}
	// Deterministic — retrying cannot help.
	if e.IsAuth() || e.IsContract() || e.IsProtocolMismatch() {
		return false
	}
	// Never reached the backend (DNS failure, connection refused/reset): safe to retry.
	if e.Code == "connect" {
		return true
	}
	// Provider/gateway rate limit — retry honouring any Retry-After.
	if e.IsRateLimited() {
		return true
	}
	// Transient gateway statuses before the backend opens the SSE response. 502/503/504
	// mean the request did NOT complete at the app (bad gateway / unavailable / gateway
	// timeout) — replaying a non-idempotent POST is safe. 500 is DELIBERATELY excluded:
	// an application error may fire after the backend has already done side effects, so
	// a blind replay could duplicate them; the backend owns retrying its own transient
	// internals. Upstream connection/generation failures after the eager meta event use
	// the SSE error path below instead.
	switch e.HTTPStatus {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	// Mid-stream failures the backend surfaces AFTER committing the 200: an upstream
	// provider hiccup (the observed 502 → `upstream_error`), a truncated stream, a
	// transient read error, an idle-timeout abort (a silent half-dead connection —
	// replaying before any content is exactly the stream_interrupted argument), or a
	// stream that died before its meta event. The size-bound protocol errors
	// (stream_line_too_large / stream_event_too_large) are deliberately NOT here: an
	// oversized payload would replay identically.
	if e.Stream {
		switch e.Code {
		case "upstream_error", "upstream_timeout", "upstream_rate_limited", "stream_read",
			"stream_interrupted", "stream_no_meta", "stream_error", "stream_idle_timeout":
			return true
		}
	}
	return false
}

// backoff computes the wait before the retry for a 0-based attempt. A server-provided
// Retry-After is HONOURED (from an HTTP response or terminal SSE error), bounded only
// by maxRetryAfterWait so a hostile value can't freeze the turn. Otherwise it is
// exponential (BaseDelay·2^attempt, capped at MaxDelay) with full jitter in [d/2, d]
// to decorrelate retries across concurrent sessions.
func (p RetryPolicy) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryAfterWait {
			return maxRetryAfterWait
		}
		return retryAfter
	}
	base := p.BaseDelay
	if base <= 0 {
		base = defaultBaseDelay
	}
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		// Guard against both the configured cap and int64 overflow (d wraps negative
		// only with pathological custom BaseDelay/attempt counts; defaults never get
		// near it).
		if d <= 0 || (p.MaxDelay > 0 && d >= p.MaxDelay) {
			d = p.MaxDelay
			break
		}
	}
	if d <= 0 || (p.MaxDelay > 0 && d > p.MaxDelay) {
		if p.MaxDelay > 0 {
			d = p.MaxDelay
		} else {
			d = base
		}
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// sleepCtx waits for d, returning false if the context is cancelled first (so the
// caller stops retrying immediately on Escape / shutdown).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
