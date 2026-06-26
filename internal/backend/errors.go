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
	Error EnvelopeError `json:"error"`
}

// EnvelopeError is the inner error object.
type EnvelopeError struct {
	Type    string `json:"type"`    // invalid_request|authentication|rate_limit|api_error
	Code    string `json:"code"`    // stable machine code, e.g. system_messages_not_allowed
	Message string `json:"message"` // human-readable detail
	Param   string `json:"param"`   // offending field path, when applicable
}

// Error is a backend failure surfaced to the agent loop. HTTPStatus is 0 for a
// mid-stream SSE error (the 200 was already committed). RetryAfter is set from the
// Retry-After header on a 429. Stream is true when the failure arrived as a
// terminal `error` event after the meta event (vs. a pre-stream JSON error).
type Error struct {
	HTTPStatus int
	Type       string
	Code       string
	Message    string
	Param      string
	RetryAfter time.Duration
	Stream     bool
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

// IsAuth reports a 401/403 (dev token mismatch in dev; account error later).
func (e *Error) IsAuth() bool { return e.HTTPStatus == 401 || e.HTTPStatus == 403 }

// IsRateLimited reports an upstream/model rate limit (429).
func (e *Error) IsRateLimited() bool {
	return e.HTTPStatus == 429 || e.Type == "rate_limit" || e.Code == "upstream_rate_limited"
}

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
