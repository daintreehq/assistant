package mcp

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// urlishRe matches an absolute URL embedded in error text. Go transport errors
// (*url.Error and the SDK errors wrapping them) format the FULL request URL into
// their message — including the query string, which for an MCP endpoint can carry
// credentials (?session=<token>). Every error string this package logs or stores
// is run through sanitizeErrText before it can reach lastError/Status/debug logs.
var urlishRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']+`)

// sanitizeErrText strips credential-bearing URL parts (userinfo, query string,
// fragment) from every URL embedded in an error message, leaving
// scheme://host/path so the message stays diagnosable. Deliberately NOT
// URL.Redacted(), which masks only the userinfo password and leaves query
// parameters verbatim.
func sanitizeErrText(msg string) string {
	if msg == "" {
		return ""
	}
	return urlishRe.ReplaceAllStringFunc(msg, redactURLText)
}

// redactURLText rebuilds one matched URL without userinfo/query/fragment. An
// unparseable match falls back to cutting at the first '?'/'#' so a token can
// never survive on the parse-failure path.
func redactURLText(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i]
		}
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// IsCredentialTerminalStatus reports whether a connection status/error string
// marks this session's MCP credentials as PERMANENTLY dead: the per-session
// bearer was revoked (401/unauthorized — Daintree rotates it on every
// re-provision and revokes on window close/eviction/displacement) or the
// server declared the binding gone (SESSION_BINDING_GONE / BINDING_STALE).
// docs/DAINTREE_HOST.md: repeated auth failures trip Daintree's abuse policy,
// so a caller seeing this must STOP reconnecting with the same token and wait
// for fresh credentials (the next assistant launch pushes them). String-level
// because connect failures surface only through Status().Error text.
func IsCredentialTerminalStatus(errText string) bool {
	if errText == "" {
		return false
	}
	if isBindingTerminal(errText) {
		return true
	}
	low := strings.ToLower(errText)
	return strings.Contains(low, "401") || strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "unauthenticated") || strings.Contains(low, "403")
}

// isUnavailable reports whether err is (or wraps) an UnavailableError. Callers use
// it to decide NOT to degrade the connection (it already means disconnected).
func isUnavailable(err error) bool {
	var ue *UnavailableError
	return errors.As(err, &ue)
}

// UnavailableError is the sentinel returned when a Daintree-requiring call is made
// while the client is not connected. Callers distinguish it from transport errors
// (errors.As) so they DO NOT degrade the connection on it (it already means
// disconnected) and surface a clean MCP_UNAVAILABLE tool result. Port of
// McpUnavailableError; Code is fixed to "MCP_UNAVAILABLE".
type UnavailableError struct {
	Message string
}

// UnavailableCode is the stable error code surfaced to tool callers.
const UnavailableCode = "MCP_UNAVAILABLE"

func (e *UnavailableError) Error() string {
	if e == nil || e.Message == "" {
		return "Daintree MCP is not connected"
	}
	return e.Message
}

// Code returns the stable machine code ("MCP_UNAVAILABLE").
func (e *UnavailableError) Code() string { return UnavailableCode }

// ErrUnavailable is a zero-value sentinel for errors.As matching:
//
//	var ue *mcp.UnavailableError
//	if errors.As(err, &ue) { ... }
var ErrUnavailable = &UnavailableError{}

// newUnavailable builds an UnavailableError from the current lastError, falling
// back to the canonical message when empty.
func newUnavailable(lastError string) *UnavailableError {
	if lastError == "" {
		lastError = "Daintree MCP is not connected"
	}
	return &UnavailableError{Message: lastError}
}
