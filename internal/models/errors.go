package models

// Sentinel error types for the model layer. The `code` strings are a downstream
// contract (callers switch on them), so they must stay stable.

// DeepSeekUnavailableError is raised by guard() before any wire call when the
// client can't talk to DeepSeek (offline mode, or no API key).
type DeepSeekUnavailableError struct{ Message string }

func (e *DeepSeekUnavailableError) Error() string { return e.Message }

// Code returns the stable error code ("DEEPSEEK_UNAVAILABLE").
func (e *DeepSeekUnavailableError) Code() string { return "DEEPSEEK_UNAVAILABLE" }

// ImageInputNotSupportedError is raised when a request carries image content but
// is routed to a tier whose model can't see images. Only the large tier is
// vision-capable; small is text-only and medium routes through, so both reject.
// The router enforces this before any wire call so the failure is a clear local
// error, not an opaque provider 400. The gate is on tier semantics, not the
// resolved model id (medium rejects even though it currently routes to large).
type ImageInputNotSupportedError struct{ Message string }

func (e *ImageInputNotSupportedError) Error() string { return e.Message }

// Code returns the stable error code ("IMAGE_INPUT_NOT_SUPPORTED").
func (e *ImageInputNotSupportedError) Code() string { return "IMAGE_INPUT_NOT_SUPPORTED" }

// RateLimitedError is raised when the model retry budget is exhausted on a 429
// (provider rate-limit / quota). It is the EXPORTED boundary type: the unexported
// apiError can't cross into the agent layer, so the final exhausted 429 is wrapped
// here (see wrapExhaustedRateLimit). classifyStreamError matches it to produce a
// friendly "Model rate-limited" reply instead of a raw provider blob, and the
// cockpit raises a model-health badge. RetryAfterMs copies the provider's honoured
// Retry-After (ms, 0 when no header was sent) — preserved on the struct for future
// surfacing, deliberately NOT shown to the user today.
type RateLimitedError struct {
	Message      string
	RetryAfterMs int
}

func (e *RateLimitedError) Error() string {
	if e.Message == "" {
		return "provider quota/throughput exceeded"
	}
	return e.Message
}

// Code returns the stable error code ("MODEL_RATE_LIMITED").
func (e *RateLimitedError) Code() string { return "MODEL_RATE_LIMITED" }

// CancelledError is a streaming/chat turn the caller aborted (the UI's
// Escape-to-cancel). Distinct from a model failure: the agent loop treats it as a
// clean stop, not a broken turn. Raised when the context is cancelled, so the raw
// transport error never leaks upward.
type CancelledError struct{ Message string }

func (e *CancelledError) Error() string {
	if e.Message == "" {
		return "Turn cancelled"
	}
	return e.Message
}

// Code returns the stable error code ("CANCELLED").
func (e *CancelledError) Code() string { return "CANCELLED" }

// newCancelled builds a CancelledError with the default message.
func newCancelled() *CancelledError { return &CancelledError{Message: "Turn cancelled"} }
