package backend

import (
	"context"
	"math/rand"
	"net/http"
	"time"
)

// Retry defaults, shared by the streamed respond turn and the JSON endpoints
// (tasks / capabilities / health / ready).
//
// The budget is sized to RIDE OUT a backend that is down rather than to paper over
// one dropped packet. The motivating failure (observed 2026-07-30) is the backend
// being restarted or briefly wedged: the CLI got `connect: connection refused`,
// burned its single 286ms retry against a socket that was still closed, and failed
// the turn — when simply waiting ~30s would have found the backend back up. So the
// schedule starts fast (a genuine blip is fixed in under a second) and settles into
// a patient 10–15s poll:
//
//	0.5s · 1s · 2s · 4s · 8s · 15s · 15s · 15s · 15s   (9 retries, ~50–75s with jitter)
//
// This is still only the SECOND line of defence — the backend owns retrying its own
// upstream provider — but it now covers the whole CLI↔backend hop for as long as a
// restart plausibly takes. Replays are near-free: the conversation prefix is
// unchanged across attempts and the backend prefix-caches it; after an eager meta,
// the retry also carries that attempt's signed state so skill selection is reused.
const (
	defaultMaxAttempts = 10 // initial attempt + 9 retries
	defaultBaseDelay   = 500 * time.Millisecond
	defaultMaxDelay    = 15 * time.Second
	// defaultMaxElapsed bounds the WHOLE retried call, because an attempt budget
	// alone does not: the attempts themselves are not free. A JSON attempt may run
	// its 60s per-attempt timeout and a stream attempt may burn its idle window, so
	// ten of those plus the backoff sleeps could stall a deadline-less caller
	// (compaction, an extraction task) for ten minutes or more. The budget is a
	// restart-recovery window, not an endurance test: once it is spent, the honest
	// answer is a failure the user can see. It never truncates the case it exists
	// for — a refused socket fails in microseconds, so all 10 attempts fit inside
	// the ~50–75s of backoff with room to spare.
	defaultMaxElapsed = 2 * time.Minute
	// maxRetryAfterWait bounds how long a server-provided Retry-After can stall a
	// turn. We HONOUR Retry-After (from an HTTP response or SSE error) rather than
	// clamping it down to the jittered-backoff cap — retrying earlier than the server
	// asked just burns the budget — but a pathological value can't freeze the attached session.
	// It is pinned to the steady-state ceiling: a server asking for longer than the
	// backoff's own maximum wait gets that maximum, and the budget absorbs the rest.
	maxRetryAfterWait = defaultMaxDelay
	// retryJitterFloorNum/Den set the jitter window as a fraction of the computed
	// delay: [d·2/3, d]. Classic FULL jitter ([d/2, d]) decorrelates a thundering
	// herd, which a single local CLI does not have; the narrower window keeps the
	// steady-state poll inside the intended 10–15s band instead of dipping to 7.5s.
	retryJitterFloorNum = 2
	retryJitterFloorDen = 3
)

// RetryPolicy tunes transient-failure retries for RespondStream. The zero value is
// not usable directly — NewClient substitutes DefaultRetryPolicy when MaxAttempts is
// 0. Set MaxAttempts to 1 to disable retries entirely (a single attempt).
type RetryPolicy struct {
	MaxAttempts int           // total attempts INCLUDING the first; 1 = no retries
	BaseDelay   time.Duration // backoff for the first retry (doubles thereafter)
	MaxDelay    time.Duration // cap on a single backoff (0 = uncapped)
	// MaxElapsed caps the wall clock of the whole retried call — attempts plus
	// backoff sleeps — so slow failures can't multiply the attempt budget into
	// minutes. 0 = unbounded (the attempt count is then the only limit).
	MaxElapsed time.Duration
}

// exhausted reports whether scheduling another attempt after `delay` would run past
// the elapsed budget. Checked BEFORE sleeping, so the budget bounds when the last
// attempt STARTS; a single in-flight attempt may still overrun it (its own timeout,
// or the caller's context, bounds that).
func (p RetryPolicy) exhausted(elapsed, delay time.Duration) bool {
	return p.MaxElapsed > 0 && elapsed+delay >= p.MaxElapsed
}

// DefaultRetryPolicy is the production policy: 10 attempts total, exponential from
// 500ms and settling into a 10–15s poll — enough to ride out a backend restart on
// the CLI↔backend hop (the backend still owns its own provider retries).
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: defaultMaxAttempts,
		BaseDelay:   defaultBaseDelay,
		MaxDelay:    defaultMaxDelay,
		MaxElapsed:  defaultMaxElapsed,
	}
}

// RetryInfo is handed to ClientConfig.OnRetry (and StreamCallbacks.OnRetry) just
// before each backoff sleep, for observability and for the attached session's "retrying…"
// cue. Attempt is 0-based: 0 is the first failure (about to make the first retry).
// MaxAttempts is the policy's total budget, so a renderer can say "2 of 10" without
// reaching into the client. Op names the failing call ("respond", or the JSON
// method+path) — with every endpoint retried now, a log line without it can't tell a
// stalled turn from a stalled utility task.
type RetryInfo struct {
	Attempt     int
	MaxAttempts int
	Delay       time.Duration
	Op          string
	Err         error
}

// noRetryCtxKey marks a context as a one-shot probe.
type noRetryCtxKey struct{}

// WithoutRetry returns a context whose backend calls make exactly ONE attempt.
//
// It exists for DIAGNOSTICS. `/doctor` and the doctor subcommand ask "is the hop up
// right now?", and the answer must be immediate: with the patient default budget, a
// probe against a refused socket would silently spend its whole timeout retrying and
// report the same failure seconds later. Retrying there would also mask the exact
// condition the probe exists to surface. Everything else — turns, tasks, the boot
// handshake — wants the retries.
func WithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryCtxKey{}, true)
}

// retriesDisabled reports whether ctx was marked one-shot by WithoutRetry.
func retriesDisabled(ctx context.Context) bool {
	v, _ := ctx.Value(noRetryCtxKey{}).(bool)
	return v
}

// nonRetriableAppCodes are the backend's stable machine codes that mean "the work
// already ran and this is its verdict" — a replay would re-run the model to reach the
// same answer. Keyed on `error.code` rather than the HTTP status because the backend
// reuses 502 for both an application verdict and a genuine gateway failure. Applies
// to PRE-stream errors only (see isRetriable).
var nonRetriableAppCodes = map[string]bool{
	"task_output_invalid": true,
	CodeUpstreamError:     true,
	"internal_error":      true,
}

// deterministicUpstreamCodes are final on BOTH transports. Unlike the set above, these
// are not verdicts about work that ran — they are conditions that will hold identically
// on every replay: a key the provider does not recognise, an account with no credit, a
// key without permission for this model, a routing policy that matches no endpoint, and
// the two malformed-exchange codes that mean our own bug.
//
// The transport-independence is the point. The backend emits `meta` before it opens the
// upstream stream, so a deterministic rejection usually arrives as an SSE error with
// HTTPStatus 0 rather than as an HTTP status at all. Classifying those by status would
// see a 503-shaped routing dead end as "transient gateway", and the retry loop would sit
// through the full ~50-75s backoff to re-derive the same empty pool — the user waiting
// out a budget for an answer that was final the first time.
//
// `upstream_unavailable` and `upstream_timeout` are deliberately NOT here: those are the
// genuinely transient half of the old `upstream_error` blob, and giving up on them would
// trade one wrong behaviour for another.
var deterministicUpstreamCodes = map[string]bool{
	CodeProviderInvalidAPIKey:       true,
	CodeProviderInsufficientCredit:  true,
	CodeProviderKeyForbidden:        true,
	CodeUpstreamNoCompliantProvider: true,
	CodeUpstreamRequestRejected:     true,
	CodeUpstreamProtocolError:       true,
}

// isRetriable reports whether a backend failure is a transient class worth retrying.
// Two families are never retried: deterministic failures (auth, contract bugs,
// protocol mismatch), where a replay fails identically; and application VERDICTS,
// where the work already ran and a replay would re-run the model to reach the same
// answer. The caller separately guarantees no visible content has streamed yet, so
// replaying the turn cannot duplicate output.
func isRetriable(e *Error) bool {
	if e == nil {
		return false
	}
	// Deterministic — retrying cannot help.
	if e.IsAuth() || e.IsContract() || e.IsProtocolMismatch() {
		return false
	}
	// Deterministic upstream conditions, on either transport. Checked BEFORE everything
	// below it, because three of those would otherwise misread one of these: the status
	// switch sees a routing dead end as a transient 503, the stream allowlist sees an
	// unrecognised key arriving with no status at all, and IsRateLimited() ORs in a
	// `rate_limit_error` type that a future backend could plausibly attach to one of
	// these. A code in this set is final; nothing after here should get a say.
	if deterministicUpstreamCodes[e.Code] {
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
	// An application VERDICT that happens to ride a transient-looking status. The
	// status alone is NOT enough to decide here, because the Daintree backend reuses
	// 502 for "the work ran and its output was unusable" — see `_task_output_invalid`
	// in ../assistant-backend (services/task_runner.py), whose own comment says to
	// re-read the terminal a different way "rather than retrying the same structured
	// extract". Replaying one of these burns a FULL model call per attempt to arrive
	// at the identical verdict, so the pre-stream classes below are terminal:
	//
	//   task_output_invalid — output parsed neither first time nor after the repair pass
	//   upstream_error      — provider REJECTED the request (backend default retryable=False)
	//   internal_error      — a 500; may already have run a side effect
	//
	// Deliberately scoped to `!e.Stream`: the mid-stream contract below is different
	// and load-bearing — an `upstream_error` arriving after the 200 but before any
	// visible token is exactly the transient blip this retry layer was built for.
	if !e.Stream && nonRetriableAppCodes[e.Code] {
		return false
	}
	// Transient gateway statuses before the backend opens the SSE response. With the
	// application verdicts above already excluded, what remains is a request that did
	// NOT complete at the app: a genuine bad gateway from an intermediary, a warming
	// backend (503 `service_unavailable`, which the backend sends with Retry-After: 5),
	// or an upstream timeout (504, backend `retryable=True`). Replaying a
	// non-idempotent POST is safe in those cases. Upstream connection/generation
	// failures after the eager meta event use the SSE error path below instead.
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
	// `upstream_unavailable` is in this list because the taxonomy split moved it here:
	// a provider outage used to arrive as `upstream_error` and be replayed, and would
	// otherwise have silently stopped being replayed the day the backend started naming
	// it precisely. `upstream_error` itself survives as the backend's last-resort
	// mapping for a stream error it could not classify — a real "we don't know", which
	// is the one case where a replay is worth the attempt.
	if e.Stream {
		switch e.Code {
		case CodeUpstreamError, CodeUpstreamTimeout, CodeUpstreamRateLimited,
			CodeUpstreamUnavailable, "stream_read", "stream_interrupted",
			"stream_no_meta", "stream_error", "stream_idle_timeout":
			return true
		}
	}
	return false
}

// backoff computes the wait before the retry for a 0-based attempt. A server-provided
// Retry-After is HONOURED (from an HTTP response or terminal SSE error), bounded only
// by maxRetryAfterWait so a hostile value can't freeze the turn. Otherwise it is
// exponential (BaseDelay·2^attempt, capped at MaxDelay) with jitter in [d·2/3, d] —
// enough to decorrelate concurrent sessions without letting the steady-state poll
// drift below the intended band.
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
	floor := d * retryJitterFloorNum / retryJitterFloorDen
	span := d - floor
	if floor <= 0 || span <= 0 {
		return d
	}
	return floor + time.Duration(rand.Int63n(int64(span)+1))
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
