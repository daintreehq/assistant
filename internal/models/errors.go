package models

// Sentinel error types for the model layer. The `code` strings are a downstream
// contract (callers switch on them), so they must stay stable.

// FireworksUnavailableError is raised by guard() before any wire call when the
// client can't talk to Fireworks (offline mode, or no API key).
type FireworksUnavailableError struct{ Message string }

func (e *FireworksUnavailableError) Error() string { return e.Message }

// Code returns the stable error code ("FIREWORKS_UNAVAILABLE").
func (e *FireworksUnavailableError) Code() string { return "FIREWORKS_UNAVAILABLE" }

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
