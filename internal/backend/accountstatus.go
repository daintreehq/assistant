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

// Where the entitlement answer came from. Source and freshness are ONE fact wearing two
// field names: `polar` is a live billing lookup, which cannot be stale by definition, and
// `cache` is the fallback served when that lookup could not be made, which cannot be
// fresh. Either mismatch is a contradiction rather than a curiosity, and validateChecked
// refuses both.
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
	// The server bounds `email` at 320 CHARACTERS (Pydantic max_length, which counts
	// code points); this counts BYTES. They are the same number only for ASCII, so the
	// byte ceiling is set at the widest a 320-code-point string can be — four bytes per
	// code point — rather than at 320. At 320 bytes a perfectly server-valid address of
	// 320 accented characters would be refused here, and refused as a CONTRACT failure,
	// which is the reading that turns somebody's own email address into "we could not
	// verify your account".
	//
	// The bound is still worth having: it stops a compromised or misconfigured backend
	// putting an arbitrary blob into a status line, a support bundle and a JSON event
	// stream. It is an abuse ceiling measured in the unit this language counts in, not
	// a restatement of the server's rule.
	maxAccountEmailBytes = 320 * 4
	// The billing provider's own word for the subscription's condition. Deliberately
	// LOOSE: the contract does not enumerate these values, so a bound tight enough to
	// be a format check would turn a wordier provider status into a protocol failure —
	// and this field is prose nothing branches on. It is an abuse cap, not a rule.
	//
	// It is deliberately WIDER than the backend's own cap on the same field, which is
	// 64 and — unlike its email rule — is measured in BYTES there too
	// (len(status.encode("utf-8")) in the entitlement client), so the two units agree
	// and no server-valid value can exceed 64 here. The asymmetry is the safe direction
	// and the only safe direction: a reader that accepts more than the writer can emit
	// never refuses a legitimate response, whereas one that matched exactly would turn
	// any future widening on the server into "could not verify your plan" for everybody
	// on an older build. Tightening this to 64 buys nothing — a value between 65 and 256
	// cannot arrive — and costs that.
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
	//
	// A POINTER, because absent and false are different statements and the contract
	// distinguishes them: the backend omits this field entirely when no entitlement
	// lookup happened, and sends it as a real boolean when one did. A bare `false`
	// would decode identically from both, which would make the `unverified` rule below
	// unenforceable — "we checked, and the answer is fresh" is precisely the claim an
	// identity-only response must not be able to make by accident. Read it through
	// Stale(); nothing outside this file should dereference it.
	EntitlementStale *bool `json:"entitlement_stale"`
	// CheckedAt is when the entitlement answer was established, as sent. Absent for
	// `unverified`, where nothing was checked — see validate.
	CheckedAt string `json:"checked_at"`

	// CheckedAtTime is CheckedAt parsed, filled in by validate. It has no JSON tag
	// because it is not on the wire: a caller that needs the instant should not have to
	// re-parse a string the contract has already validated, and two parse sites is two
	// chances to disagree about whether a naive timestamp is acceptable.
	CheckedAtTime time.Time `json:"-"`
}

// Granted reports the one verdict that permits paid work.
func (a AccountStatus) Granted() bool { return a.Access == AccessGranted }

// Stale reports whether the entitlement answer came from an aged cache.
//
// An ABSENT field reads as false, which is the only sound reading: a response that did
// not mention staleness has not claimed any, and validate has already established that
// absence is legitimate only where no lookup happened. Every caller outside this file
// goes through here rather than the pointer, so "not stale" and "never asked" cannot
// end up rendered differently by two readers that both meant the same thing.
func (a AccountStatus) Stale() bool { return a.EntitlementStale != nil && *a.EntitlementStale }

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
//
// The shape is a fork, because version 1 describes TWO documents rather than one with
// optional parts. An `unverified` response reports that identity is established and
// entitlement was never looked up; the other three report the result of a lookup that
// did happen. Fields that are required in one are forbidden in the other, and folding
// them into a single pass of "optional unless" rules is how this file previously came
// to demand `checked_at` from a response that had nothing to timestamp.
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

	// REQUIRED, on every verdict, not merely well-formed when it happens to be there.
	//
	// This is a PROTECTED endpoint: reaching it at all means a bearer was accepted, so
	// there is always a subject to hash and no legitimate response can omit it. Treating
	// an empty one as acceptable made the field's absence indistinguishable from an
	// anonymous principal the route cannot produce — and the support id is the one thing
	// on this contract whose entire job is to correlate a user's report with a server
	// log, which it cannot do when it is quietly blank.
	if !subjectHashPattern.MatchString(a.SubjectHash) {
		return accountContractError("subject hash is not 16 lowercase hex characters")
	}

	switch a.PlanID {
	case "", PlanStandard, PlanPro:
	default:
		return accountContractError("unrecognised plan id")
	}
	switch a.EntitlementSource {
	case "", EntitlementSourcePolar, EntitlementSourceCache:
	default:
		return accountContractError("unrecognised entitlement source")
	}

	if a.Access == AccessUnverified {
		return a.validateIdentityOnly()
	}
	return a.validateChecked()
}

// validateIdentityOnly enforces the rule that gives `unverified` its meaning: a verdict
// that reports NO LOOKUP cannot carry a lookup's results.
//
// Every field it forbids is one a reader would otherwise render as a finding. A
// `plan_id` beside `unverified` is a plan nobody confirmed; a `checked_at` is a time at
// which nothing was checked; an `entitlement_stale: false` is the sentence "we asked,
// and the answer is current" written by a response that asked nothing. The backend
// enforces the same rule on the way out, and it is repeated here because this side is
// what decides whether somebody is told to go and buy a subscription.
//
// CheckedAtTime is left at its zero value on this path, and callers already read that
// as "no entitlement time to show" (auth/status.go guards on IsZero) rather than as a
// timestamp at the epoch.
func (a *AccountStatus) validateIdentityOnly() error {
	switch {
	case a.PlanID != "":
		return accountContractError("access is unverified but a plan was reported")
	case a.SubscriptionStatus != "":
		return accountContractError("access is unverified but a subscription status was reported")
	case a.EntitlementSource != "":
		return accountContractError("access is unverified but an entitlement source was reported")
	case a.EntitlementStale != nil:
		return accountContractError("access is unverified but entitlement staleness was reported")
	case strings.TrimSpace(a.CheckedAt) != "":
		return accountContractError("access is unverified but a check time was reported")
	}
	return nil
}

// validateChecked enforces the rules for the three verdicts an entitlement lookup
// produced, and fills in CheckedAtTime.
func (a *AccountStatus) validateChecked() error {
	// A grant with no plan behind it is the contradiction that matters most: it is the
	// combination that would let a backend bug hand out paid access, and it is cheap to
	// refuse here.
	if a.Access == AccessGranted && a.PlanID == "" {
		return accountContractError("access is granted with no plan")
	}
	// SOURCE AND FRESHNESS ARE REQUIRED ON ALL THREE, not only on `granted`.
	//
	// The backend's own entitlement value object types both as non-optional and refuses
	// a website answer that omits either, so a checked verdict that arrives without them
	// did not come from a healthy deployment. Accepting it anyway is not leniency: with
	// `entitlement_stale` absent, Stale() answers false, and the response is rendered as
	// "we checked, and this is current" — the exact claim the pointer above exists to
	// stop a body making by accident. A missing source is the same shape of problem one
	// step down: paid access, or a refusal of it, that cannot say which authority
	// decided.
	//
	// This is stricter than an earlier reading of the contract, which let the two
	// subscription verdicts omit their source on the theory that a deployment might know
	// the answer without naming its authority. No deployment does — the field is
	// mandatory on the way out — and the cost of the lenient reading is a fabricated
	// freshness claim, which is worse than the "could not verify" it was avoiding.
	if a.EntitlementSource == "" {
		return accountContractError("a checked verdict named no entitlement source")
	}
	if a.EntitlementStale == nil {
		return accountContractError("a checked verdict did not say whether its answer was stale")
	}
	// Stale describes an aged CACHE. A live answer that calls itself stale is either a
	// bug or a mislabelled cache, and neither should be shown to a user as a fact about
	// their billing. The backend enforces the same pairing on the way out.
	if a.Stale() && a.EntitlementSource != EntitlementSourceCache {
		return accountContractError("entitlement marked stale without a cache source")
	}
	// And the same pairing read the other way, which is the half that actually hurts. The
	// cache is the fallback served when the live provider could not be reached, so a
	// cached answer is stale by construction and `cache` beside `entitlement_stale: false`
	// is one body making both claims at once. The two do not cancel out: Stale() answers
	// false, and the response is rendered as "we checked, and this is current" — a
	// freshness claim about somebody's billing that the cached answer never made, and the
	// precise fabrication the pointer decoding above exists to keep a body from committing
	// by omission. Leaving this direction open let it be committed outright instead. The
	// backend enforces the same pairing on the way out, in both directions, so nothing a
	// healthy deployment sends is refused by either guard.
	if a.EntitlementSource == EntitlementSourceCache && !a.Stale() {
		return accountContractError("entitlement came from a cache but was not marked stale")
	}

	// REQUIRED here, and only here. A lookup happened, so it happened at a time, and
	// that time is what separates "you are subscribed" from "you were subscribed when
	// we last managed to ask" — the single fact a user needs to judge how much to trust
	// the line above it.
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
	// Year one parses cleanly and IS Go's zero time, which every consumer reads as "no
	// entitlement check happened" (auth/status.go guards on IsZero before rendering a
	// checked-at row). Left through, it would be a completed lookup that silently loses
	// its timestamp — the response says one thing and the screen shows another. The Unix
	// epoch is deliberately NOT rejected: it is an absurd clock reading, but it is a
	// distinguishable one, and rendering "plan checked 1 Jan 1970" tells the truth about
	// a broken backend where dropping the row would hide it.
	if t.IsZero() {
		return accountContractError("checked_at is the zero time, which cannot be told apart from no check at all")
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
