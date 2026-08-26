package backend

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Envelope is the stable Daintree error envelope. It is delivered pre-stream as a
// JSON body and mid-stream as a terminal SSE `error` event with the same shape.
type Envelope struct {
	Error      EnvelopeError `json:"error"`
	RetryAfter string        `json:"retry_after,omitempty"`
}

// EnvelopeError is the inner error object.
type EnvelopeError struct {
	// The taxonomy family, "_error"-suffixed — invalid_request_error |
	// authentication_error | rate_limit_error | api_error, plus billing_error for the
	// subscription family. The list is OPEN, and an unrecognised family is decoded and
	// carried rather than rejected. It is a machine identifier but still a
	// backend-controlled string, so it is scrubbed on the way out like the rest — see
	// scrubBackendError.
	//
	// No classifier here consults Type except IsRateLimited, which ORs in
	// `rate_limit_error` and so does reach the retry layer through isRetriable. Every
	// other decision — account, provider, billing, spend — reads Code, the stable half of
	// the contract, plus the documented status fallbacks for a body this build does not
	// recognise. That is what lets the backend add a family or move a code between them
	// without a coordinated release: `billing_error` for the two subscription codes is
	// exactly such a move, and it changes nothing here.
	Type string `json:"type"`

	Code    string `json:"code"`    // stable machine code, e.g. system_messages_not_allowed
	Message string `json:"message"` // human-readable detail
	Param   string `json:"param"`   // offending field path, when applicable
}

// The backend's stable upstream-failure codes. Every one of these names a DIFFERENT
// problem with a different fix, which is the entire reason they exist: they used to
// collapse into one 502 `upstream_error`, so a tester whose balance had run out was told
// their credentials were rejected and dutifully replaced a perfectly good key.
//
// Two properties are load-bearing for this package, and neither is on the wire:
//
//   - Retryability. The backend knows it per code and does NOT serialise it, so
//     isRetriable classifies from the code here. Getting it wrong is expensive in both
//     directions — replaying a deterministic verdict burns the whole retry budget to
//     re-derive the same answer, and failing to replay a transient one turns a blip
//     into a failed turn.
//   - Whose problem it is. The provider-account codes describe the credential the
//     BACKEND spends upstream; the routing code is the caller's policy; the two
//     "rejected/protocol" codes are ours. (Not to be confused with account.go's codes,
//     which are about who is CALLING — a different question with a different remedy.)
//     Only distinct codes can produce distinct advice, which is what
//     internal/agent/session.go renders.
//
// The HTTP statuses are listed for orientation only. NEVER classify on status alone: a
// mid-stream SSE error carries HTTPStatus 0 (the 200 was already committed), so the same
// condition reaches this package with and without its status depending only on how far
// the request got.
const (
	// The DEPLOYMENT's upstream provider account. Deterministic — the backend
	// operator's settings, the backend operator's fix.
	//
	// `provider_invalid_api_key` carries no status annotation on purpose. It has arrived
	// as a 401 for as long as the code has existed and is moving to a 5xx; nothing in
	// this package reads the number for it, so the move lands with nothing to change
	// here. Annotating it again is how someone comes to classify on it again.
	CodeProviderInvalidAPIKey      = "provider_invalid_api_key"
	CodeProviderInsufficientCredit = "provider_insufficient_credits" // 402 upstream
	CodeProviderKeyForbidden       = "provider_key_forbidden"        // 403 upstream

	// Routing. Deterministic: the policy is fixed, so an immediate replay re-derives
	// the same empty endpoint pool. The backend fails closed here rather than quietly
	// relaxing the privacy floor to find a route.
	CodeUpstreamNoCompliantProvider = "upstream_no_compliant_provider" // 503

	// Transient upstream conditions. Worth replaying while no visible content has
	// committed.
	CodeUpstreamRateLimited = "upstream_rate_limited" // 429
	CodeUpstreamTimeout     = "upstream_timeout"      // 504
	CodeUpstreamUnavailable = "upstream_unavailable"  // 503

	// Our bug, not the caller's: we sent something the provider would not accept, or it
	// answered with something we could not parse. Permanent either way.
	CodeUpstreamRequestRejected = "upstream_request_rejected" // 502
	CodeUpstreamProtocolError   = "upstream_protocol_error"   // 502

	// The pre-split catch-all. The backend still emits it, but now ONLY for a stream
	// error it could not map to anything above — a genuine "we don't know", which is
	// why it stays retryable on the stream path.
	CodeUpstreamError = "upstream_error" // 502
)

// CodeInvalidAPIKey is the backend's own ingress rejection of a malformed bearer — the
// one door code that belongs to neither taxonomy above nor the account codes in
// account.go. It is named here because a decision path reads it (see taskMayHaveBilled,
// which must know that no provider was ever reached). IsAuth deliberately still reaches
// it through the 401 fallback instead: unlike the codes it excludes by name, this one IS
// a 401 by nature — the header on THIS request is the thing that is wrong.
const CodeInvalidAPIKey = "invalid_api_key"

// Error is a backend failure surfaced to the agent loop. HTTPStatus is 0 for a
// mid-stream SSE error (the 200 was already committed). RetryAfter is set from the
// Retry-After header on an HTTP error or the top-level retry_after field on an SSE
// error. Stream is true when the failure arrived as a terminal `error` event after
// the meta event (vs. a pre-stream JSON error).
type Error struct {
	HTTPStatus int
	Type       string
	Code       string
	Message    string
	Param      string
	RetryAfter time.Duration
	Stream     bool
	// cause is the underlying local error for a failure this package RAISED rather than
	// received — today only a credential the token source could not produce. It is
	// unexported and surfaced through Unwrap so `errors.Is` can recover a sentinel the
	// auth layer raised (a locked keychain, a refresh outage), which a string-formatted
	// message throws away. Nil for every error decoded off the wire.
	cause error
	// RequestID is the backend's X-Request-Id for the failing call, when it sent one.
	// It is what makes "report this as a bug" actionable: the two codes that mean our
	// bug (CodeUpstreamRequestRejected, CodeUpstreamProtocolError) are undiagnosable
	// without it, since the useful detail is in the server's log, not the client's.
	RequestID string
}

// Unwrap exposes the local cause of a client-raised failure, so a caller can use
// errors.Is against the sentinel the auth layer returned. Nil for wire errors.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("backend: ")
	if e.HTTPStatus != 0 {
		b.WriteString("http ")
		b.WriteString(strconv.Itoa(e.HTTPStatus))
		b.WriteString(" ")
	}
	if e.Stream {
		b.WriteString("(stream) ")
	}
	if e.Code != "" {
		b.WriteString(e.Code)
		b.WriteString(": ")
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	} else {
		b.WriteString("request failed")
	}
	if e.Param != "" {
		b.WriteString(" [")
		b.WriteString(e.Param)
		b.WriteString("]")
	}
	return b.String()
}

// IsAuth reports a credential problem at OUR door — one whose fix is the Authorization
// header: sign in, refresh, or re-authenticate.
//
// Two families are deliberately EXCLUDED even though they share 401/403.
//
// The provider account codes came first, and they are why the check below asks the code
// and not the status: `provider_invalid_api_key` arrives today with the same 401 a real
// auth failure carries — and is moving off it, which changes nothing here — while
// `provider_key_forbidden` shares the 403. Either way the key the provider rejected is
// the one the BACKEND spends, so telling the person at the terminal to "check you pasted
// it in full" sends them round a re-entry loop for a credential they have never held.
//
// The two 403s — `auth_client_not_allowed` and `auth_permission_denied` — are the
// newer and sharper case. Each carries a perfectly valid, perfectly fresh token that
// this deployment refuses anyway: the first because the OAuth client that minted it is
// not accepted, the second because the credential lacks authority for the operation.
// Reading either as "sign again" is worse than unhelpful — the auth layer treats IsAuth
// as licence to refresh, and every refresh mints another token wrong in exactly the same
// way. No credential operation can fix them, which is what `unfixableIdentityCodes`
// names. See AuthRemedy, which maps both to RemedyReconfigure.
//
// A 401 or 403 carrying no code this package recognises still reads as auth, so a CLI
// pointed at an older backend — or through a proxy that rewrote the body — keeps its
// correct message instead of falling through to "model error".
func (e *Error) IsAuth() bool {
	if e == nil {
		return false
	}
	if providerAccountCodes[e.Code] {
		return false
	}
	// A recognised account code decides on its own. Consulting the status for these
	// would let a `subscription_required` that happened to arrive as a 401 send the
	// user to sign in again, only to reach the same 402 — and would let an
	// `auth_token_expired` that arrived mid-stream (status 0) escape classification
	// entirely. The code is the stable contract; the status is not.
	if accountCodes[e.Code] {
		return identityCodes[e.Code] && !unfixableIdentityCodes[e.Code]
	}
	// A local credential failure is not the backend rejecting us — we never asked.
	if e.Code == CodeCredentialUnavailable {
		return false
	}
	return e.HTTPStatus == 401 || e.HTTPStatus == 403
}

// providerAccountCodes are the three conditions that live on the OpenRouter account the
// DEPLOYMENT funds turns from — never the user's, who holds no provider credential at
// all. Grouped because every consumer that asks "is this the upstream account?" wants
// all three, while the advice for each one differs — see IsUpstreamAuth.
var providerAccountCodes = map[string]bool{
	CodeProviderInvalidAPIKey:      true,
	CodeProviderInsufficientCredit: true,
	CodeProviderKeyForbidden:       true,
}

// IsUpstreamAuth reports a well-formed key that the UPSTREAM provider then rejected.
// IsAuth is about the credential this CLI presents — fix your header; this is about the
// credential the backend spends upstream — a revoked key, an empty balance, or a key not
// permitted to use this model, none of which anything on the user's machine can change.
// Without the split, a funding problem on the deployment's account would read to the
// person in front of the terminal as their own broken login.
//
// The legacy 502 `upstream_error` form is still recognised so a CLI pointed at an older
// backend keeps its correct message rather than falling through to "Model error".
func (e *Error) IsUpstreamAuth() bool {
	return providerAccountCodes[e.Code] ||
		(e.HTTPStatus == 502 && e.Code == CodeUpstreamError)
}

// IsProviderAccount reports one of the three post-split account codes specifically —
// i.e. IsUpstreamAuth minus the legacy catch-all. Consumers that branch per code (to
// say "out of credit" rather than "key rejected") gate on this, because the legacy code
// cannot tell them which of the three it was.
func (e *Error) IsProviderAccount() bool { return providerAccountCodes[e.Code] }

// ProviderAccountReason is a short clause naming WHICH account problem this is, for a
// caller composing its own sentence around it ("… but the provider " + reason). Empty
// when the error is not one of the three, including for the legacy `upstream_error`
// blob — which genuinely could not tell them apart, and must not be made to look as if
// it could.
//
// Every clause names the DEPLOYMENT's credential and offers the reader no action of
// their own. The account in question is the one whoever runs this backend pays for, so
// telling a user to rotate a key or top up a balance sends them to change a value they
// have never held — see upstreamFailureAdvice in internal/agent/session.go, which is
// the copy a real turn renders.
func (e *Error) ProviderAccountReason() string {
	switch e.Code {
	case CodeProviderInvalidAPIKey:
		return "does not recognise the credential this backend spends — it belongs to the deployment, so report it to whoever runs the backend"
	case CodeProviderInsufficientCredit:
		return "reports the account this backend spends from has no credit left — it is the deployment's account rather than yours"
	case CodeProviderKeyForbidden:
		return "will not let the credential this backend spends use this model — its model permissions, spend limit or guardrails are blocking it"
	}
	return ""
}

// IsRateLimited reports an upstream/model rate limit (429).
//
// The type check was previously "rate_limit", which the backend never emits — it
// sends the "_error"-suffixed OpenAI taxonomy, so that comparison was dead and the
// 429/code checks were carrying the whole function.
// An exhausted plan quota is deliberately excluded even though it also rides 429 and
// a backend may well tag it `rate_limit_error`. A rate limit clears on its own within
// seconds; a usage cap does not clear until the billing period rolls over, so treating
// it as one produces a backoff loop that cannot succeed and a message ("slow down")
// that is simply untrue.
//
// `account_rate_limited` is recognised by CODE as well as by status, because a
// mid-stream 429 arrives with HTTPStatus 0 and would otherwise depend on the backend
// also setting Type — a field the stable-code contract should not need.
func (e *Error) IsRateLimited() bool {
	if e == nil || e.Code == CodeUsageLimitReached {
		return false
	}
	return e.HTTPStatus == 429 ||
		e.Type == "rate_limit_error" ||
		e.Code == CodeUpstreamRateLimited ||
		e.Code == CodeAccountRateLimited
}

// IsRoutingDeadEnd reports that no upstream endpoint satisfied the active routing
// policy. Not an outage and not a bug: the eligible pool was empty, which a stricter
// privacy mode or a narrow endpoint allowlist can cause on its own. Surfacing it as a
// generic upstream failure would hide the one thing that fixes it.
func (e *Error) IsRoutingDeadEnd() bool { return e.Code == CodeUpstreamNoCompliantProvider }

// IsReportable reports the two codes whose only useful action is a bug report carrying
// RequestID: the exchange between Daintree and the provider was malformed in one
// direction or the other, and nothing the caller can change about their account, their
// key or their routing policy affects it.
//
// The two are grouped for that reason alone, NOT because they share a culprit — and a
// message must not claim they do. `upstream_request_rejected` means the provider judged
// OUR request body malformed, which is almost always our bug.
// `upstream_protocol_error` means the provider answered with something unparseable,
// which is usually a provider or compatibility problem. Same next step, opposite
// direction of fault.
func (e *Error) IsReportable() bool {
	return e.Code == CodeUpstreamRequestRejected || e.Code == CodeUpstreamProtocolError
}

// IsConnect reports that the backend was unreachable — a connection-level failure
// (dial refused/timeout), not an HTTP response. It is the most common local-dev
// failure (the backend isn't running) and deserves a connectivity message, not a
// "model error". Set at the only two construction sites in client.go.
func (e *Error) IsConnect() bool { return e.Code == "connect" }

// IsContract reports a 400 — a CLI contract bug (forbidden role, reserved tool,
// invalid schema/name, unknown field). The message+param tell you what to fix.
func (e *Error) IsContract() bool { return e.HTTPStatus == 400 }

// IsProtocolMismatch reports a 426 — the CLI's protocol_version is unsupported.
func (e *Error) IsProtocolMismatch() bool {
	return e.HTTPStatus == 426 || e.Code == "unsupported_daintree_protocol"
}

// newError builds an *Error from an envelope and HTTP status.
func newError(status int, env Envelope, retryAfter time.Duration, stream bool) *Error {
	return &Error{
		HTTPStatus: status,
		Type:       env.Error.Type,
		Code:       env.Error.Code,
		Message:    env.Error.Message,
		Param:      env.Error.Param,
		RetryAfter: retryAfter,
		Stream:     stream,
	}
}

// httpError builds an *Error for a non-2xx HTTP response when the body could not
// be decoded into an envelope (e.g. an unexpected 5xx with no JSON).
func httpError(status int, rawBody string) *Error {
	msg := strings.TrimSpace(rawBody)
	if msg == "" {
		msg = fmt.Sprintf("unexpected status %d", status)
	}
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	return &Error{HTTPStatus: status, Type: "api_error", Code: "http_error", Message: msg}
}

// parseRetryAfter parses a Retry-After header value (seconds, or an HTTP-date).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
