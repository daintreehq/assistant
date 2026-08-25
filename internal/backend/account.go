package backend

// account.go holds the ACCOUNT half of the error taxonomy: the stable codes the
// backend returns about who the caller is and what their plan currently permits.
//
// It is deliberately a separate file from errors.go, which owns the UPSTREAM half —
// conditions at the provider behind the backend. The two are answered by different
// systems and fixed by different people. An upstream code means "the model call went
// wrong"; an account code means "we never got as far as a model call, and here is the
// door you are standing at". Collapsing them is exactly the mistake the upstream split
// already corrected once: a caller told to "check your key" when the real answer was
// "buy a plan" goes round a loop that cannot terminate.
//
// The IDENTITY codes are matched by a REMEDY (see AuthRemedy) rather than by prose,
// because the auth layer has to branch on them without reading messages. That is the
// whole reason the codes are stable: the message is for a human, the code is for the
// state machine. The plan and dependency codes carry no remedy — nothing the credential
// layer can do affects them — and AuthRemedy deliberately returns RemedyNone for them.

// The account codes. Statuses are listed for orientation only — never classify on
// status alone. Two of these share 401 with a third that must be handled in the exact
// opposite way, and both 429s mean different things:
//
//   - auth_token_expired is a 401 that a silent refresh fixes.
//   - auth_session_revoked is a 401 where refreshing is guaranteed to fail and the
//     stored credential must be DELETED instead.
//   - account_rate_limited is a 429 worth replaying after Retry-After.
//   - usage_limit_reached is a 429 where replaying just burns the backoff budget to
//     re-derive the same exhausted quota.
//   - the two 403s share a remedy and differ only in what they tell a human, which is
//     the whole reason they are not one code.
const (
	// No bearer credential arrived at all. Offer sign-in; never retry automatically.
	CodeAuthRequired = "auth_required" // 401
	// Malformed, wrong issuer/audience/client, or a bad signature. Refresh only when a
	// stored session exists to refresh FROM; otherwise the credential is unusable and
	// the answer is a fresh sign-in.
	CodeAuthTokenInvalid = "auth_token_invalid" // 401
	// The access token is past its expiry. One refresh, then one replay — and only
	// while replaying is still safe (see the streaming rule on AuthRemedy).
	CodeAuthTokenExpired = "auth_token_expired" // 401
	// The JWT verified, but its session no longer exists: a logout, a grant
	// revocation, or a security action ended it. The refresh token is dead too, so
	// clearing the local credential is the only correct response.
	CodeAuthSessionRevoked = "auth_session_revoked" // 401
	// A valid token from an OAuth client this deployment does not accept. This is
	// CONFIGURATION, not a login problem, and it is the specific reason IsAuth no
	// longer answers on status alone: a refresh loop here re-mints a token that is
	// wrong in exactly the same way, forever.
	CodeAuthClientNotAllowed = "auth_client_not_allowed" // 403
	// A credential this deployment DOES accept, presented for an operation it may not
	// perform. The second 403, and it differs from the first in its MESSAGE rather than
	// its remedy — both are settled, and no credential operation fixes either.
	//
	// Worth its own code precisely because the remedy is shared: told
	// `auth_client_not_allowed` when their client IS accepted, whoever reads that log
	// goes hunting a registration problem that does not exist. It is unreachable while
	// every allowlisted client holds the whole permission set, and is carried now so a
	// narrower client cannot make the wrong code correct-by-accident on arrival.
	CodeAuthPermissionDenied = "auth_permission_denied" // 403

	// Authenticated, but no product grants assistant access. Send them to the plans
	// page; the login is fine and must be kept.
	CodeSubscriptionRequired = "subscription_required" // 402
	// A subscription exists but does not currently grant access (payment failed,
	// paused, ended). Distinct from the above because the fix is the billing portal,
	// not a second checkout — suggesting a duplicate purchase here is how someone ends
	// up paying twice.
	CodeSubscriptionInactive = "subscription_inactive" // 402

	// The plan's hard usage cap is exhausted for the period. Deterministic until the
	// period rolls over or the plan changes.
	CodeUsageLimitReached = "usage_limit_reached" // 429
	// Too many requests from this account. Genuinely transient; honour Retry-After.
	CodeAccountRateLimited = "account_rate_limited" // 429

	// Session validity could not be established safely. The user is NOT logged out by
	// this — the backend simply could not check, and treating "we don't know" as "you
	// are signed out" would delete a perfectly good credential during an outage.
	CodeAuthDependencyUnavailable = "auth_dependency_unavailable" // 503
	// The billing authority is unreachable and no acceptable positive cache exists.
	// Must never be presented as "you are unsubscribed".
	CodeEntitlementUnavailable = "entitlement_unavailable" // 503
	// A durable usage write failed before any paid work began, so the backend refused
	// to invoke the provider rather than run work it could not account for.
	CodeUsageAccountingUnavailable = "usage_accounting_unavailable" // 503

	// CodeCredentialUnavailable is LOCAL, not a backend verdict: this process could not
	// produce a credential to send, so no request was made at all.
	//
	// It is deliberately not auth_required. That code means the backend saw an
	// unauthenticated request and wants a sign-in; this one means a locked keychain, a
	// refresh that could not complete, or a credential store we could not read. Telling
	// someone to sign in when the real fault is a locked keychain sends them through a
	// browser flow that will fail at the same write. Keeping them apart also keeps
	// doctor honest: "the backend rejected us" and "we never asked" are different rows.
	CodeCredentialUnavailable = "auth_credential_unavailable" // no HTTP status — never sent
)

// AuthRemedy is what the auth layer must DO about a failure — the typed outcome that
// replaces string-matching on messages. Guide §11.1 and §18 both turn on this being a
// closed set: every branch below has a different, mutually exclusive next step, and
// two of them (RemedyReconfigure, RemedyClear) exist specifically to stop the retry
// loops that a naive "401 means log in" reading produces.
type AuthRemedy int

const (
	// RemedyNone: not an account failure. Nothing for the auth layer to do.
	RemedyNone AuthRemedy = iota
	// RemedySignIn: there is no usable credential and none can be derived. Prompt.
	RemedySignIn
	// RemedyRefresh: refresh once and replay, but only when the operation is
	// replay-safe and nothing has been shown to the user yet. A refresh-driven replay
	// happens at most once per operation.
	RemedyRefresh
	// RemedyRefreshOrSignIn: refresh if a stored session exists; otherwise sign in.
	RemedyRefreshOrSignIn
	// RemedyClear: delete the local credential and require a fresh sign-in. Refreshing
	// cannot succeed — the session that would authorize it is gone.
	RemedyClear
	// RemedyReconfigure: this deployment refuses a credential that is valid and current
	// — either the OAuth client that minted it is not accepted, or the credential lacks
	// the authority for this operation. RETAIN it, never refresh, never replay, report
	// the refusal. The action is shared by both 403s precisely because no credential
	// operation changes the answer; what differs is what a human must be told, which is
	// why they stayed two codes. Callers rendering for a person branch on the CODE.
	RemedyReconfigure
)

// String renders the remedy for logs and tests.
func (r AuthRemedy) String() string {
	switch r {
	case RemedySignIn:
		return "sign_in"
	case RemedyRefresh:
		return "refresh"
	case RemedyRefreshOrSignIn:
		return "refresh_or_sign_in"
	case RemedyClear:
		return "clear"
	case RemedyReconfigure:
		return "reconfigure"
	}
	return "none"
}

// authCodeRemedies maps each identity code to its single correct response.
var authCodeRemedies = map[string]AuthRemedy{
	CodeAuthRequired:         RemedySignIn,
	CodeAuthTokenExpired:     RemedyRefresh,
	CodeAuthTokenInvalid:     RemedyRefreshOrSignIn,
	CodeAuthSessionRevoked:   RemedyClear,
	CodeAuthClientNotAllowed: RemedyReconfigure,
	CodeAuthPermissionDenied: RemedyReconfigure,
}

// unfixableIdentityCodes are the identity codes that no credential operation can
// resolve: the token is valid, current, and refused anyway.
//
// A named set rather than an inline exception because it is now the ONE declaration two
// separate rules read. IsAuth means "a credential operation can fix this", and answering
// true for either of these licenses a refresh loop that re-mints a token wrong in exactly
// the same way; the retry layer needs the same membership to keep a settled 403 out of
// the transient rules (see deterministicAccountCodes, which derives from this). Written
// as `!= CodeAuthClientNotAllowed` at each site instead, the second code would have had
// to be remembered at both — and the one that got forgotten would fail silently.
var unfixableIdentityCodes = map[string]bool{
	CodeAuthClientNotAllowed: true,
	CodeAuthPermissionDenied: true,
}

// subscriptionCodes are the two 402s: authenticated, but not currently entitled.
var subscriptionCodes = map[string]bool{
	CodeSubscriptionRequired: true,
	CodeSubscriptionInactive: true,
}

// accountDependencyCodes are the three 503s that mean "we could not check", never
// "the answer is no". Grouped because every consumer that asks "should I preserve the
// user's login and retry?" wants all three, and because the failure mode they guard
// against is identical: presenting an outage as a verdict.
var accountDependencyCodes = map[string]bool{
	CodeAuthDependencyUnavailable:  true,
	CodeEntitlementUnavailable:     true,
	CodeUsageAccountingUnavailable: true,
}

// identityCodes is the closed set of codes about WHO the caller is. Used by IsAuth and
// by the retry classifier, which must never replay one of these blindly — the auth
// ladder owns the single refresh-and-replay, and a transport-level retry underneath it
// would multiply that into a loop.
var identityCodes = map[string]bool{
	CodeAuthRequired:         true,
	CodeAuthTokenInvalid:     true,
	CodeAuthTokenExpired:     true,
	CodeAuthSessionRevoked:   true,
	CodeAuthClientNotAllowed: true,
	CodeAuthPermissionDenied: true,
}

// accountCodes is the union of every code this package recognises as an ACCOUNT
// verdict. It exists to make the taxonomy code-first: a recognised code decides on its
// own, and the status is consulted only for codes we do not know.
//
// Without it the legacy status fallbacks leak into the new taxonomy in both directions.
// A `subscription_required` that arrives with a 401 — which a backend bug, a proxy, or
// a future status change could easily produce — would read as IsAuth and send the user
// to sign in again to reach the same 402. An `auth_dependency_unavailable` arriving
// with a 500 would become non-retriable. The code is the stable contract; the status is
// not, and must never override it.
var accountCodes = func() map[string]bool {
	m := make(map[string]bool, 13)
	for _, set := range []map[string]bool{identityCodes, subscriptionCodes, accountDependencyCodes} {
		for k := range set {
			m[k] = true
		}
	}
	m[CodeUsageLimitReached] = true
	m[CodeAccountRateLimited] = true
	return m
}()

// IsAccountCode reports whether this error carries a recognised account verdict, and
// therefore that its classification must come from the code rather than the status.
func (e *Error) IsAccountCode() bool { return e != nil && accountCodes[e.Code] }

// AuthRemedy reports what the auth layer should do about this error, or RemedyNone
// when it is not an identity failure.
//
// A bare 401 with no recognised code still yields RemedySignIn: an older backend, or
// one behind a proxy that rewrote the body, can answer 401 without the taxonomy, and
// the safe reading of "unauthenticated, reason unknown" is to ask for a sign-in rather
// than to refresh a token nothing said was expired.
func (e *Error) AuthRemedy() AuthRemedy {
	if e == nil {
		return RemedyNone
	}
	if r, ok := authCodeRemedies[e.Code]; ok {
		return r
	}
	// A recognised NON-identity account code (a plan or dependency verdict) is settled:
	// no credential operation helps, whatever status it happened to arrive with.
	if accountCodes[e.Code] {
		return RemedyNone
	}
	// A 403 without a recognised code is NOT a login problem by default. 401 is.
	if e.HTTPStatus == 401 && !providerAccountCodes[e.Code] {
		return RemedySignIn
	}
	return RemedyNone
}

// IsAccountIdentity reports one of the six identity codes specifically — as opposed
// to IsAuth, which also admits an untyped 401 from an older backend. The two are NOT
// the same question: IsAuth answers "can a credential operation fix this", which is
// false for both 403s even though each is squarely a verdict about who is calling.
func (e *Error) IsAccountIdentity() bool { return e != nil && identityCodes[e.Code] }

// IsSubscription reports that the caller is authenticated but not currently entitled.
// The login is good and must be preserved; only the plan is the problem.
func (e *Error) IsSubscription() bool { return e != nil && subscriptionCodes[e.Code] }

// IsUsageLimited reports an exhausted plan usage cap. Deliberately NOT folded into
// IsRateLimited even though both are 429: a rate limit clears on its own within
// seconds, and a usage cap does not clear until the period rolls over. Retrying the
// second one spends the whole backoff budget to re-derive the same refusal.
func (e *Error) IsUsageLimited() bool { return e != nil && e.Code == CodeUsageLimitReached }

// IsAccountRateLimited reports a per-account request-rate limit, honouring RetryAfter.
func (e *Error) IsAccountRateLimited() bool { return e != nil && e.Code == CodeAccountRateLimited }

// IsAccountDependency reports that an auth/billing dependency was unavailable, so the
// backend could not reach a verdict. Local credentials must be PRESERVED and the call
// retried with backoff — this is the code that must never be rendered as "you are
// signed out" or "you are unsubscribed".
func (e *Error) IsAccountDependency() bool { return e != nil && accountDependencyCodes[e.Code] }
