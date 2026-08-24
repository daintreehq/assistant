package auth

import "github.com/daintreehq/assistant/internal/backend"

// state.go is the local account state machine.
//
// One typed model, driven by typed outcomes, never by string matching. The reason it is
// a closed enum rather than a pair of booleans is that several of these states LOOK the
// same to a naive "am I signed in?" check and demand opposite handling:
//
//   - StateSubscriptionRequired is a fully valid login. Clearing credentials there would
//     make the user sign in again to reach the same 402.
//   - StateTemporarilyUnavailable means we could not CHECK. Rendering it as signed-out
//     deletes a working session because a dependency blipped.
//   - StateRevoked means the session is genuinely gone and the stored credential is
//     dead weight that must be removed.
//   - StateStorageUnavailable is signed-in-but-not-persisted: everything works until the
//     process exits, and the user has to be told that.
//
// Every consumer — the CLI status command, the JSON event stream, Daintree's settings
// panel — branches on this and nothing else.

// State is the local account state.
type State string

const (
	// StateUnknown: nothing has been determined yet this process.
	StateUnknown State = "unknown"
	// StateSignedOut: no stored credential.
	StateSignedOut State = "signed_out"
	// StateAuthorizing: a browser login is in progress.
	StateAuthorizing State = "authorizing"
	// StateSignedInUnverified: a credential exists but the backend has not confirmed it
	// this process. Offline startup lands here.
	StateSignedInUnverified State = "signed_in_unverified"
	// StateSignedInActive: signed in and entitled.
	StateSignedInActive State = "signed_in_active"
	// StateSubscriptionRequired: signed in, no plan grants access. The LOGIN IS GOOD.
	StateSubscriptionRequired State = "signed_in_subscription_required"
	// StateSubscriptionInactive: signed in, a known plan does not currently grant
	// access. Distinct from the above because the fix is the billing portal, not a
	// second checkout.
	StateSubscriptionInactive State = "signed_in_subscription_inactive"
	// StateRefreshing: a token refresh is in flight.
	StateRefreshing State = "refreshing"
	// StateTemporarilyUnavailable: a dependency was down, so nothing could be verified.
	// Credentials are RETAINED.
	StateTemporarilyUnavailable State = "temporarily_unavailable"
	// StateRevoked: the session no longer exists upstream. Credentials are DELETED.
	StateRevoked State = "revoked"
	// StateStorageUnavailable: signed in, but nothing was persisted and the session dies
	// with this process.
	StateStorageUnavailable State = "storage_unavailable"
)

// SignedIn reports whether a credential exists, whatever the plan says.
//
// The subscription states are deliberately included: they are authenticated sessions.
// A caller that treated them as signed-out would send someone through a browser flow to
// arrive at the identical 402.
func (s State) SignedIn() bool {
	switch s {
	case StateSignedInUnverified, StateSignedInActive,
		StateSubscriptionRequired, StateSubscriptionInactive,
		StateRefreshing, StateTemporarilyUnavailable, StateStorageUnavailable:
		return true
	}
	return false
}

// NeedsLogin reports whether the only way forward is a fresh sign-in.
//
// StateTemporarilyUnavailable is deliberately excluded even though nothing can be
// verified in it: "we could not check" is not "you are signed out", and prompting there
// discards a working credential over an outage.
func (s State) NeedsLogin() bool {
	return s == StateSignedOut || s == StateRevoked
}

// NeedsPlan reports whether the account needs billing attention rather than a login.
func (s State) NeedsPlan() bool {
	return s == StateSubscriptionRequired || s == StateSubscriptionInactive
}

// CanSpend reports whether a paid request should be attempted at all.
//
// This is what the supervisor daemon consults before a wake turn. It is deliberately
// strict: only a confirmed-active session qualifies, and an unverified one does not,
// because an unattended process should not discover its login is dead by spending money
// to find out. An interactive session takes the opposite view and simply tries — a
// human is there to read the error.
func (s State) CanSpend() bool { return s == StateSignedInActive }

// Terminal reports a state that will not change without user action.
func (s State) Terminal() bool {
	return s == StateSignedOut || s == StateRevoked || s == StateSubscriptionRequired
}

// StateForRemedy maps a backend identity verdict onto the local state it produces.
//
// It lives beside the enum so a new remedy cannot be added upstream without a decision
// here about what it means locally. internal/backend imports nothing from this module,
// so depending on it directly is safe and is better than mirroring the constants — a
// mirrored enum drifts silently, and the whole point of a typed remedy is that it
// cannot.
func StateForRemedy(r backend.AuthRemedy) State {
	switch r {
	case backend.RemedyClear:
		// The session is gone upstream; the stored refresh token is dead weight.
		return StateRevoked
	case backend.RemedySignIn:
		return StateSignedOut
	case backend.RemedyRefresh, backend.RemedyRefreshOrSignIn:
		// A refresh is worth attempting, so the credential is still assumed good.
		return StateRefreshing
	case backend.RemedyReconfigure:
		// The token is valid and this deployment will not take it. Nothing about the
		// credential is wrong, so it is kept — but nothing can be verified either.
		return StateTemporarilyUnavailable
	}
	return StateUnknown
}
