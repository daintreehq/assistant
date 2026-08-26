package auth

import (
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// accountsnapshot.go holds what the backend last said about this account — the email,
// the plan, the billing verdict — for THIS PROCESS AND NO LONGER.
//
// Nothing here is persisted, and that is a security boundary rather than an oversight.
// A plan on disk is a plan that can be wrong: a subscription can lapse, an account can
// be closed, and a cached "pro" would keep asserting itself long after it stopped being
// true, on a machine that never asked again. So a new process starts knowing only that a
// credential exists, and learns the rest by asking — which is what `auth status
// --refresh` is for.
//
// Staleness is STRUCTURAL rather than remembered. Clearing the fields at each site that
// ends a session was rejected: there are several such sites, they are spread across a
// 1200-line file, and nothing stops a new one being added without the clear. Instead the
// snapshot records the conditions it was taken under and every read re-checks all three:
//
//	generation — the local identity counter. Moves on a login, a logout, a revocation,
//	             and on another process's revision bump noticed during a refresh.
//	revision   — the marker SHARED between processes. This is the one the generation
//	             cannot cover on its own: another process can log out and back in as a
//	             DIFFERENT account, and a plain Hydrate here then finds a perfectly valid
//	             credential and settles a signed-in state without the local counter ever
//	             moving. Account A's email and plan would render under account B.
//	signed in  — a credential still exists. Catches every path that ends a session
//	             without touching either counter: a Hydrate that finds the store empty, a
//	             refresh whose grant was rejected, a logout whose key would not resolve.
//
// Any one of them failing hides the snapshot, so a path that forgets all three would have
// to also leave a live, same-account, same-revision session — which is not a stale read.

// accountSnapshot is the memory-only account view.
type accountSnapshot struct {
	// set is the ONLY discriminator for "nothing known yet". It cannot be inferred from
	// the other fields: generation 0 is a perfectly ordinary live session (a process
	// that hydrates a stored credential never increments), and `unverified` legitimately
	// carries no plan, no source and no email.
	set bool
	// gen is the identity generation this describes.
	gen uint64
	// revision is the SHARED marker at the time of the answer. See the header: it is
	// what catches another process swapping the account underneath this one.
	revision Marker

	email             string
	subjectHash       string
	planID            string
	entitlementSource string
	entitlementStale  bool
	access            string
	// checkedAt is when the BACKEND established the entitlement — its own `checked_at`,
	// not when this process asked. The two differ whenever the answer came from a cache,
	// and the difference is the whole reason a stale answer is worth flagging.
	checkedAt time.Time
}

// StateForAccess maps a backend access verdict onto the local state it produces.
//
// It sits beside the state enum for the same reason StateForRemedy does: a verdict added
// upstream cannot reach the CLI without a decision here about what it MEANS locally.
// An unrecognised verdict deliberately yields StateSignedInUnverified rather than
// anything more definite — the identity is good (the request was authenticated) and the
// entitlement is a word this build cannot read, which is exactly "signed in, not
// verified".
func StateForAccess(access string) State {
	switch access {
	case backend.AccessGranted:
		return StateSignedInActive
	case backend.AccessSubscriptionRequired:
		return StateSubscriptionRequired
	case backend.AccessSubscriptionInactive:
		return StateSubscriptionInactive
	case backend.AccessUnverified:
		return StateSignedInUnverified
	}
	return StateSignedInUnverified
}

// ApplyAccountStatus folds a decoded account response into local state.
//
// `gen` is the generation captured BEFORE the request, and the check is the same one
// ApplyBackendVerdict and MarkIdentityLive perform, for the same reason: answers arrive
// late.
// A status read can outlive a logout, and applying it unconditionally would report a
// signed-in account with a plan on a machine that has no credential at all.
//
// Unlike ApplyBackendVerdict it takes no token: the account body describes the SESSION,
// not the specific access token that carried the request, so a refresh that replaced the
// token mid-call does not make the answer stale. The generation is the whole guard.
//
// It only ever applies to a session that still exists (State.SignedIn), mirroring
// MarkIdentityLive — a verdict can confirm or qualify a login, never resurrect one.
//
// This is also the ONLY route to StateSignedInActive, and it must stay the only one.
// That state means signed in AND ENTITLED, so nothing short of the backend saying
// access=granted may produce it; a protected 2xx elsewhere proves the credential lives
// and says nothing whatever about billing.
//
// It REPORTS whether it committed, and callers must believe that report rather than the
// absence of an error. Every reason below to decline is a legitimate one — the identity
// moved, the session ended, the body was not a verdict — and none of them is a failure of
// the request. A caller that read "no error" as "this landed" would tell somebody their
// plan had been refreshed by a call that deliberately changed nothing, which is the exact
// misreading the generation guard exists to make impossible.
func (m *Manager) ApplyAccountStatus(gen uint64, st backend.AccountStatus) (applied bool) {
	// An answer that is not one of the four recognised verdicts is not an answer, and
	// this returns WITHOUT MUTATING rather than mapping it to something harmless.
	//
	// The distinction matters more than it looks. Every other effect below is the effect
	// of a SUCCESSFUL check: it stamps a fresh verification time, clears the last error,
	// and replaces whatever plan was previously known. Applying those to a zero value —
	// which is exactly what the client returns beside an error — would record a
	// successful verification that never happened and drop a good snapshot for it.
	// Client.Account already refuses an unrecognised verdict, so nothing valid is lost;
	// this is the guard for a caller that passes on the error path anyway.
	switch st.Access {
	case backend.AccessGranted, backend.AccessSubscriptionRequired,
		backend.AccessSubscriptionInactive, backend.AccessUnverified:
	default:
		return false
	}
	// Read the shared marker BEFORE taking m.mu: it is a file read, and the manager's
	// mutex is deliberately never held across one.
	marker := m.revision.Current()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != gen {
		return false
	}
	if !m.state.SignedIn() {
		return false
	}
	now := m.now()
	m.account = accountSnapshot{
		set:               true,
		gen:               gen,
		revision:          marker,
		email:             st.Email,
		subjectHash:       st.SubjectHash,
		planID:            st.PlanID,
		entitlementSource: st.EntitlementSource,
		entitlementStale:  st.Stale(),
		access:            st.Access,
		checkedAt:         st.CheckedAtTime,
	}
	// The state follows the verdict even when the credential is memory-only. That looks
	// like it discards the "this sign-in will not persist" warning, and it does not: the
	// storage tier is a SEPARATE axis with its own field and its own rendered row, so
	// `signed_in_active` beside `credentials  this process only` says both true things,
	// where StateStorageUnavailable alone said nothing about the plan.
	m.state = StateForAccess(st.Access)
	// A live answer supersedes whatever last went wrong. Without this, a status that has
	// just been confirmed still carries the error code from the outage that preceded it.
	m.lastErr = nil
	m.lastVerifiedAt = &now
	return true
}

// accountSnapshotLocked returns the snapshot if it still describes the current identity.
//
// Callers must hold m.mu and must pass the CURRENT shared marker, read outside the lock
// — the caller has already read it (Status renders it) and reading it here would put a
// file read under the manager's mutex.
//
// The marker comparison is plain INEQUALITY, with no exemption for a zero value on
// either side, and both directions earn it.
//
// A snapshot stamped with the zero marker and a non-zero marker now is the ordinary
// first-logout case: no revision file existed when the answer arrived, and one appearing
// since means something bumped it. Exempting zero there was a real bug — it left the
// previous account's email and plan rendering under the new one's session.
//
// The other direction, a marker that has gone back to zero, is a file this process could
// not read. That is ambiguous rather than clean, and it resolves toward invalidating:
// the cost is a plan that needs `--refresh` to reappear, against showing an account that
// may no longer be the one signed in. Every bump site is a genuine identity change (a
// login, a logout, a revocation, a rejected refresh grant), so nothing routine trips it.
func (m *Manager) accountSnapshotLocked(current Marker) (accountSnapshot, bool) {
	switch {
	case !m.account.set:
	case m.account.gen != m.generation:
	case !m.state.SignedIn():
	case current != m.account.revision:
	default:
		return m.account, true
	}
	return accountSnapshot{}, false
}

// ForgetAccountStatus drops the snapshot without touching the credential or the state.
//
// It is for the one case the generation guard cannot catch: the backend answered that it
// could not CHECK — a dependency outage — and the retained display fields are now of
// unknown age. Callers that want the snapshot kept across an outage (which is the
// documented behaviour for a status read) simply do not call this; it exists so a caller
// that has positive reason to distrust the snapshot can say so.
func (m *Manager) ForgetAccountStatus() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.account = accountSnapshot{}
}
