// Package auth is the CLI's account-credential authority: OAuth discovery, the
// Authorization Code + PKCE login, secure refresh-token storage, refresh under a
// cross-process lock, logout, and the redacted status every surface renders.
//
// It is the ONE implementation for every way this binary runs — a terminal session, a
// one-shot, the embedded host Daintree drives, the MCP server, and the per-project
// supervisor daemon. That is deliberate and it is the whole design. Those modes outlive
// or bypass any visible UI: the daemon can make paid requests hours after the window
// closed. If a graphical shell owned the refresh token instead, every one of them would
// either stop working or need raw credentials copied across environment variables and
// IPC, which is exactly how a rotating secret ends up in a process listing.
//
// Two boundaries hold the package together:
//
//   - The refresh token lives in the OS credential store and nowhere else. The access
//     token lives in process memory and nowhere else. Neither is ever written to the
//     project state directory, passed in argv, put in an environment variable, or sent
//     over the daemon's control socket.
//   - Auth state is PER USER, not per project. It sits at the state ROOT, so one login
//     covers every project on the machine.
package auth

import (
	"errors"
	"fmt"
)

// Local error codes. These describe failures that happen on THIS machine, before or
// instead of a backend request, and they are deliberately disjoint from the backend's
// account codes (internal/backend/account.go) — no string appears in both sets.
//
// The split matters because the two sets are acted on by different code and answered by
// different people. A backend code is a verdict about the account; a code here is a
// fact about this computer. Sharing a namespace would let "the port is busy" reach a
// branch built to handle "your session was revoked".
const (
	// CodeCallbackPortInUse: the fixed loopback callback port is occupied.
	//
	// This is a first-class, named failure rather than a fallback because Supabase
	// matches redirect URIs EXACTLY. Quietly binding a different port would produce a
	// redirect_uri that is not registered, and the user would watch the browser fail
	// with a provider error naming a URL they have never seen.
	CodeCallbackPortInUse = "auth_callback_port_in_use"
	// CodeInteractiveRequired: no system browser or no usable loopback, so the
	// authorization-code flow cannot complete here. Named rather than generic because
	// the answer is a specific one (SSH port forwarding, or a future device login) and
	// "login failed" sends people looking for a broken password.
	CodeInteractiveRequired = "auth_interactive_environment_required"
	// CodeDiscoveryUnavailable: the backend's auth manifest could not be fetched.
	// Transient — the endpoint is down or unreachable.
	CodeDiscoveryUnavailable = "auth_discovery_unavailable"
	// CodeAccountsUnavailable: the backend answered, and says it has no account layer.
	//
	// Distinct from CodeDiscoveryInvalid because nothing is wrong. A deployment can
	// legitimately run with no identity provider — that is what every install does today
	// — and reporting it as a broken configuration would send someone looking for a fault
	// that does not exist.
	CodeAccountsUnavailable = "auth_accounts_unavailable"
	// CodeDiscoveryInvalid: the manifest was fetched and REJECTED. Not transient, and
	// not to be retried: the endpoint described an OAuth configuration this build will
	// not use. See Validate for what that means and why each check exists.
	CodeDiscoveryInvalid = "auth_discovery_invalid"
	// CodeStateMismatch: the callback's state did not match the one this attempt
	// generated. Someone else's authorization response, or a forged one.
	CodeStateMismatch = "auth_state_mismatch"
	// CodeCancelled: the user declined at the consent screen, or cancelled locally.
	// A normal outcome, not an error condition — but a distinct one, so a caller can
	// stay silent rather than reporting a failure.
	CodeCancelled = "auth_cancelled"
	// CodeTimeout: the login attempt expired before a callback arrived.
	CodeTimeout = "auth_timeout"
	// CodeExchangeFailed: the authorization code could not be exchanged for tokens.
	CodeExchangeFailed = "auth_exchange_failed"
	// CodeBrowserFailed: the system browser could not be opened.
	CodeBrowserFailed = "auth_browser_failed"
	// CodeGrantRejected: the identity provider explicitly rejected the grant
	// (invalid_grant) — the refresh token is expired, already used, or revoked.
	//
	// It is its own code, rather than a flavour of CodeRefreshFailed, because it is the
	// ONE provider answer that means the session is genuinely gone and the stored
	// credential should be deleted. Everything else that goes wrong during a refresh is
	// a reason to keep it. Distinguishing them by parsing a message would put that
	// decision at the mercy of wording; a code makes it a type.
	CodeGrantRejected = "auth_grant_rejected"
	// CodeRefreshFailed: a token refresh could not complete. Distinct from
	// CodeExchangeFailed because the remedies differ — a failed refresh may simply be a
	// network blip on an otherwise valid session, where a failed exchange means the
	// login attempt itself did not produce credentials.
	CodeRefreshFailed = "auth_refresh_failed"
	// CodeNotSignedIn: an operation needing a credential was attempted without one.
	//
	// This is the ORDINARY signed-out state, and callers treat it as "send no
	// credential" rather than as a failure — the backend's open door serves anonymous
	// requests, and refusing to make one locally would break every install that has
	// never signed in.
	CodeNotSignedIn = "auth_not_signed_in"
	// CodeSessionRevoked: a session that DID exist is gone — the provider rejected the
	// grant, or a logout/disconnect happened elsewhere.
	//
	// It is separate from CodeNotSignedIn precisely because that one is swallowed. A
	// revocation must not be: "you were signed in and no longer are" is actionable and
	// completely different from "you have never signed in", and folding them together
	// would turn a revoked session into a silent downgrade to anonymous requests.
	CodeSessionRevoked = "auth_session_revoked_locally"
	// CodeStorageUnavailable: no OS credential service, so a session cannot persist.
	CodeStorageUnavailable = "auth_storage_unavailable"
	// CodeLoginInProgress: another login attempt is already running.
	CodeLoginInProgress = "auth_login_in_progress"
)

// Error is a local auth failure carrying a stable Code, a message written for a human,
// and an optional Hint naming the next action.
//
// Message is composed HERE, from our own strings — never from a provider's
// error_description, a callback query parameter, or any other text this process did not
// author. That text is attacker-influenced (it arrives on a URL a browser was pointed
// at) and it lands in terminal scrollback and a debug log. See the callback handler.
type Error struct {
	Code    string
	Message string
	Hint    string
	cause   error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return "auth: " + e.Code
	}
	return "auth: " + e.Code + ": " + e.Message
}

// Unwrap exposes the underlying cause so errors.Is/As still work through the envelope.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// newError builds a local auth error.
func newError(code, message string) *Error { return &Error{Code: code, Message: message} }

// wrapError builds a local auth error carrying an underlying cause.
func wrapError(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// withHint attaches the next action to an error.
func (e *Error) withHint(hint string) *Error {
	if e != nil {
		e.Hint = hint
	}
	return e
}

// CodeOf returns the stable local auth code carried by err, or "" when err is not one
// of this package's errors. Callers branch on this, never on message text.
func CodeOf(err error) string {
	var ae *Error
	if errors.As(err, &ae) && ae != nil {
		return ae.Code
	}
	return ""
}

// HintOf returns the next action carried by err, or "" when there is none. Callers
// render it under the message rather than inside it, so a hint can be omitted in a
// machine-readable context without losing the error.
func HintOf(err error) string {
	var ae *Error
	if errors.As(err, &ae) && ae != nil {
		return ae.Hint
	}
	return ""
}

// IsCancelled reports a login the user declined or cancelled — a normal ending, not a
// failure to report as one.
func IsCancelled(err error) bool { return CodeOf(err) == CodeCancelled }

// errPortInUse builds the port-collision error with its specific remedy.
func errPortInUse(port int, cause error) *Error {
	return wrapError(CodeCallbackPortInUse,
		fmt.Sprintf("the local sign-in callback port %d is already in use", port),
		cause,
	).withHint(fmt.Sprintf("Another process holds 127.0.0.1:%d — close it and try again. This port is registered with the identity provider and cannot be changed here.", port))
}
