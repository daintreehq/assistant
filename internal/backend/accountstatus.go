package backend

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// accountstatus.go is the account STATUS RESPONSE: what the backend says about the
// caller who is signed in right now.
//
// It is deliberately a different file from account.go, which holds the account ERROR
// TAXONOMY. The two answer opposite questions and one of them is not an error at all:
// account.go says what a REFUSAL means and what the credential layer must do about it,
// while this says what the backend reports when the request SUCCEEDS. Collapsing them
// into one module is how "account error" and "account status" become the same word in
// a reader's head, at which point a 200 carrying `subscription_required` starts looking
// like a failure — which is the single most expensive misreading available here, since
// it is a perfectly good login and deleting its credential fixes nothing.
//
// The endpoint returns 200 for a known missing or inactive subscription BECAUSE it is a
// status read. Paid work still returns the typed 402s in account.go.

// AccountStatusPath is the protected status endpoint.
const AccountStatusPath = "/v1/daintree/account"

// AccountStatusVersion is the only contract version this build understands.
//
// An unknown version is REFUSED rather than best-effort decoded. A version bump means
// a field changed meaning, and a client that guesses at the parts it recognises will
// confidently render a plan or an access verdict that no longer says what it used to.
// Refusing preserves the credential and reports "could not verify", which is true.
const AccountStatusVersion = 1

// The access verdicts. Exactly one of these arrives, and each maps to a different local
// state — see auth.StateForAccess. `unverified` is the one that is easy to leave out:
// it means identity is good and entitlement lookup is simply not configured for this
// rollout, which is neither a grant nor a refusal.
const (
	AccessGranted              = "granted"
	AccessSubscriptionRequired = "subscription_required"
	AccessSubscriptionInactive = "subscription_inactive"
	AccessUnverified           = "unverified"
)

// The plan identifiers. `standard` and `pro` grant the same binary access today; the
// difference is a product decision that does not exist yet, and this package must not
// invent one.
const (
	PlanStandard = "standard"
	PlanPro      = "pro"
)

// Where the entitlement answer came from. `cache` is the only source a stale answer may
// carry — a live billing lookup cannot be stale by definition, so the combination is a
// contradiction rather than a curiosity.
const (
	EntitlementSourcePolar = "polar"
	EntitlementSourceCache = "cache"
)

// CodeAccountContractInvalid is LOCAL, not a backend verdict: the status endpoint
// answered 200 and its body did not satisfy the contract.
//
// It is deliberately absent from account.go's `accountCodes`, and that absence is
// load-bearing. Every member of that set is a statement ABOUT the account, and the
// state machine acts on them — clearing credentials, refusing to spend, sending someone
// to a billing page. Malformed data is a statement about the BACKEND, and the one thing
// it must never be allowed to do is masquerade as "you are signed out" or "you are not
// subscribed". Staying outside the set means AuthRemedy answers RemedyNone, isRetriable
// answers false, and the credential is untouched — which is exactly right.
const CodeAccountContractInvalid = "account_contract_invalid"

// Field bounds. Generous against any legitimate value; their job is to stop a
// compromised or misconfigured backend from putting an arbitrary blob into a status
// line, a support bundle and a JSON event stream.
const (
	maxAccountEmailBytes = 320 // RFC 5321 maximum path length
	// The billing provider's own word for the subscription's condition. Deliberately
	// LOOSE: the contract does not enumerate these values, so a bound tight enough to
	// be a format check would turn a wordier provider status into a protocol failure —
	// and this field is prose nothing branches on. It is an abuse cap, not a rule.
	maxAccountSubscriptionStatusBytes = 256
	maxAccountPlanBytes               = 64
)

// subjectHashPattern pins the support correlation id to exactly what auth.SubjectHash
// produces: 16 lowercase hexadecimal characters. Anything else is not a hash this
// system generated, and rendering it as one would put an unvetted backend string into
// the field whose entire purpose is to be safe to paste into a bug report.
var subjectHashPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// AccountStatus is the decoded /v1/daintree/account body.
//
// UNKNOWN FIELDS ARE IGNORED, on purpose: encoding/json's default. A backend must be
// able to add optional metadata without an older CLI refusing the whole response — the
// strictness here is about the fields we DO read, not about the shape of the document.
//
// Nothing in this struct is persisted. It describes a session as of one instant, and a
// plan on disk is a plan that can be wrong.
type AccountStatus struct {
	Version int `json:"version"`
	// Email is display-only, optional and bounded. It is never a join key and never
	// reaches disk.
	Email string `json:"email"`
	// SubjectHash is the support correlation id. The RAW subject is deliberately not
	// carried on this contract at all — there is no field for it, so no caller can
	// render one by accident.
	SubjectHash string `json:"subject_hash"`
	// Access is the verdict. See the constants above.
	Access string `json:"access"`
	// PlanID is the active plan, or empty. `null` on the wire decodes to empty, which
	// is the intended reading of "no plan".
	PlanID string `json:"plan_id"`
	// SubscriptionStatus is the billing provider's own word for the subscription's
	// condition ("active", "past_due", …). Free-form and bounded: it is prose for a
	// human, and Access is the field to branch on.
	SubscriptionStatus string `json:"subscription_status"`
	// EntitlementSource says which authority answered. Empty is legitimate for an
	// unverified rollout, where nothing was asked.
	EntitlementSource string `json:"entitlement_source"`
	// EntitlementStale reports that the answer came from an aged cache rather than a
	// live lookup. It is shown to the user because "we believe you are subscribed, as
	// of some minutes ago" is a materially different claim from a fresh one.
	EntitlementStale bool `json:"entitlement_stale"`
	// CheckedAt is when the entitlement answer was established, as sent.
	CheckedAt string `json:"checked_at"`

	// CheckedAtTime is CheckedAt parsed, filled in by validate. It has no JSON tag
	// because it is not on the wire: a caller that needs the instant should not have to
	// re-parse a string the contract has already validated, and two parse sites is two
	// chances to disagree about whether a naive timestamp is acceptable.
	CheckedAtTime time.Time `json:"-"`
}

// Granted reports the one verdict that permits paid work.
func (a AccountStatus) Granted() bool { return a.Access == AccessGranted }

// accountContractError builds the local protocol error for a body that does not satisfy
// the contract.
//
// The reason is always OUR words describing which rule failed, never the offending
// value: a backend that echoed a bearer into `email` would otherwise have it quoted
// back through the error, into the debug log and into a support bundle.
func accountContractError(reason string) *Error {
	return &Error{
		Type:    "api_error",
		Code:    CodeAccountContractInvalid,
		Message: "backend account status did not satisfy the contract: " + reason,
	}
}

// validate enforces the version-1 rules and fills in CheckedAtTime.
//
// Everything it rejects is reported as a PROTOCOL problem, never as an account verdict.
// The distinction decides what happens to the user's login: a contract failure leaves
// the credential in place and the status merely unverified, whereas letting a malformed
// body fall through as "no plan" would send a paying customer to a checkout page.
func (a *AccountStatus) validate() error {
	if a.Version != AccountStatusVersion {
		return accountContractError("unsupported contract version")
	}

	switch a.Access {
	case AccessGranted, AccessSubscriptionRequired, AccessSubscriptionInactive, AccessUnverified:
	default:
		// Includes the empty string. An absent verdict is not a permissive default.
		return accountContractError("unrecognised access verdict")
	}

	if len(a.Email) > maxAccountEmailBytes {
		return accountContractError("email is too long")
	}
	if len(a.SubscriptionStatus) > maxAccountSubscriptionStatusBytes {
		return accountContractError("subscription status is too long")
	}
	if len(a.PlanID) > maxAccountPlanBytes {
		return accountContractError("plan id is too long")
	}

	if a.SubjectHash != "" && !subjectHashPattern.MatchString(a.SubjectHash) {
		return accountContractError("subject hash is not 16 lowercase hex characters")
	}

	switch a.PlanID {
	case "", PlanStandard, PlanPro:
	default:
		return accountContractError("unrecognised plan id")
	}
	// A grant with no plan behind it is the contradiction that matters most: it is the
	// combination that would let a backend bug hand out paid access, and it is cheap to
	// refuse here.
	if a.Access == AccessGranted && a.PlanID == "" {
		return accountContractError("access is granted with no plan")
	}

	switch a.EntitlementSource {
	case "", EntitlementSourcePolar, EntitlementSourceCache:
	default:
		return accountContractError("unrecognised entitlement source")
	}
	// A GRANT with no source is the contradiction worth refusing: paid access was
	// authorised and the response cannot say which authority said so. The other three
	// verdicts may legitimately omit it — `unverified` because nothing was asked, and
	// the two subscription verdicts because a deployment can know the answer without
	// naming its authority, and rejecting those would replace a correct "you need a
	// plan" with "could not verify" over a field nothing branches on.
	if a.Access == AccessGranted && a.EntitlementSource == "" {
		return accountContractError("access is granted with no entitlement source")
	}
	// Stale describes an aged CACHE. A live answer that calls itself stale is either a
	// bug or a mislabelled cache, and neither should be shown to a user as a fact about
	// their billing.
	if a.EntitlementStale && a.EntitlementSource != EntitlementSourceCache {
		return accountContractError("entitlement marked stale without a cache source")
	}

	if strings.TrimSpace(a.CheckedAt) == "" {
		return accountContractError("checked_at is missing")
	}
	t, err := time.Parse(time.RFC3339, a.CheckedAt)
	if err != nil {
		// Timezone-aware only. time.RFC3339 rejects a naive timestamp, which is the
		// point: an instant with no offset is an instant this process would have to
		// guess at, and a guessed "last verified" time is worse than none.
		return accountContractError("checked_at is not an RFC 3339 timestamp with an offset")
	}
	a.CheckedAtTime = t
	return nil
}

// Account asks the backend who is signed in and what their plan currently permits.
//
// It is a protected, non-streaming GET, so it inherits everything doJSON already owns:
// the bearer from the token source, the one-refresh/one-replay ladder for an expired
// token, redirect refusal, the request id, `X-Daintree-Protocol`, and the 1 MiB body
// cap. Nothing about it is special-cased at the transport except one thing — see
// accountAttempt.deferSuccess — because the DECODED BODY, not the 200, is the verdict.
//
// This is the call that lets a fresh process learn the current plan. Without it a CLI
// that has a stored credential can prove someone is signed in and never learn anything
// else about them, which is how `signed_in_unverified` became a permanent resting state
// no successful checkout could move.
func (c *Client) Account(ctx context.Context) (AccountStatus, error) {
	var out AccountStatus
	if err := c.doJSON(ctx, http.MethodGet, AccountStatusPath, nil, &out); err != nil {
		return AccountStatus{}, err
	}
	// VALIDATE FIRST, then scrub. The order is load-bearing in both directions.
	//
	// Scrubbing first would mean validating altered data: a replacement is a different
	// length, so an oversized field could be shortened under its bound and a response
	// that violates the contract would pass. It also mutates values the rules are
	// about — a short credential that happened to appear inside `plan_id` would corrupt
	// a legitimate plan into a rejection.
	//
	// Validating first is safe because the rules already close off every field that
	// could carry a credential OUT of here. Access, PlanID and EntitlementSource are
	// closed enums; SubjectHash is 16 hex characters; CheckedAt must parse as RFC 3339.
	// A bearer cannot survive in any of them — it either fails the rule, taking the
	// whole response with it, or it was never a bearer. That leaves exactly two
	// unconstrained display strings, and those are the two scrubbed below.
	if err := out.validate(); err != nil {
		return AccountStatus{}, err
	}
	// A no-op on a normal install, where there is no caller key for a backend to echo
	// back in the first place. It earns its place on the install that sets one: these
	// ride a 200 — the success path, which no error-scrubbing wrapper covers — and they
	// reach the status block, the JSON event stream and the debug log.
	out.Email = c.scrubSecrets(out.Email)
	out.SubscriptionStatus = c.scrubSecrets(out.SubscriptionStatus)
	return out, nil
}
