package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// doctorcredential_test.go covers the `upstream credential` row — the one row that
// reports a VERDICT about whether a turn can be funded at all, rather than a fact about
// this machine.
//
// It tests credentialVerdictRow rather than its caller on purpose. The caller performs
// the network round trip, and the arm below that used to dereference a nil pointer was
// unreachable from any test precisely because reaching it meant standing up an App, a
// store and a lease. Splitting the rendering out is what makes these cases cheap enough
// to enumerate.

// boolPtr already exists in overrides_test.go; only the float one is new here.
func floatPtr(f float64) *float64 { return &f }

// The regression. A backend may answer `valid: true, usable: false` and report no limit
// at all — an account refused for a reason that is not arithmetic. The row dereferenced
// the limit unconditionally on that path, so the ONE answer that says "this install
// cannot run" crashed the command whose job is to say so.
func TestAnUnusableCredentialWithNoLimitDoesNotPanic(t *testing.T) {
	ver := backend.KeyVerification{
		Valid:  true,
		Usable: boolPtr(false),
		Reason: "account_suspended",
		Detail: "This account is suspended.",
		// LimitRemaining deliberately absent — this is the whole case.
	}

	c := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")

	if c.Status != StatusFail {
		t.Errorf("status = %s, want fail — an unusable credential fails every turn", c.Status)
	}
	if c.Hint == "" {
		t.Error("no hint — a failing row a reader cannot act on is a support ticket")
	}
	// The stable reason is what a newer backend uses to name a condition this build has
	// no copy for. Repeating it is the honest answer; substituting our nearest known
	// wording would send someone to top up an account that is not short of money.
	if !strings.Contains(c.Detail, "account_suspended") {
		t.Errorf("detail = %q, want it to carry the backend's reason", c.Detail)
	}
	if strings.Contains(c.Detail, "NO CREDIT") {
		t.Errorf("detail = %q — asserts an exhausted balance the backend never reported", c.Detail)
	}
	if _, ok := c.Data["limitRemaining"]; ok {
		t.Error("limitRemaining is present for a verification that reported none")
	}
	if c.Data["reason"] != "account_suspended" {
		t.Errorf("Data[reason] = %v, want the stable reason on the machine-readable half", c.Data["reason"])
	}
}

// The reason decides the copy, and `credits_exhausted` is the one this build has real
// advice for. Keeping it distinct from the generic case is the point of branching on the
// reason at all: "top up" is right here and wrong for every other refusal.
func TestAnExhaustedCredentialKeepsItsTopUpAdvice(t *testing.T) {
	ver := backend.KeyVerification{
		Valid:          true,
		Usable:         boolPtr(false),
		Reason:         backend.ReasonCreditsExhausted,
		LimitRemaining: floatPtr(0),
	}

	c := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")

	if c.Status != StatusFail {
		t.Errorf("status = %s, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "NO CREDIT") {
		t.Errorf("detail = %q, want the exhausted-balance wording", c.Detail)
	}
	// It must still be the EXHAUSTED copy — distinct from every other refusal — but it
	// may not tell the reader to top up. The spent account is the deployment's upstream
	// one; the CLI holds no provider credential, so "top up the account" named an action
	// the reader could not take.
	if !strings.Contains(c.Hint, "backend's own upstream account") {
		t.Errorf("hint = %q, want it to say whose account is spent", c.Hint)
	}
	if !strings.Contains(c.Hint, "report it") {
		t.Errorf("hint = %q, want the action actually available here", c.Hint)
	}
	if c.Data["limitRemaining"] != float64(0) {
		t.Errorf("Data[limitRemaining] = %v, want 0 — a reported zero is not the same as absent", c.Data["limitRemaining"])
	}
}

// An older backend predates `usable` and `reason` both, so IsUsable reaches the
// unusable arm from the balance alone. Branching on the reason must not lose the top-up
// advice there: the exhausted balance IS in the response, and answering "the backend
// gave no reason" for a response that plainly reported one is a downgrade.
func TestAnOlderBackendsSpentKeyKeepsItsTopUpAdvice(t *testing.T) {
	for _, remaining := range []float64{0, -4.5} {
		ver := backend.KeyVerification{Valid: true, LimitRemaining: floatPtr(remaining)}

		c := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")

		if c.Status != StatusFail {
			t.Errorf("remaining=%v: status = %s, want fail", remaining, c.Status)
		}
		if !strings.Contains(c.Detail, "NO CREDIT") {
			t.Errorf("remaining=%v: detail = %q, want the exhausted-balance wording", remaining, c.Detail)
		}
		if !strings.Contains(c.Hint, "backend's own upstream account") {
			t.Errorf("remaining=%v: hint = %q — the exhausted-balance advice was lost", remaining, c.Hint)
		}
		if _, ok := c.Data["reason"]; ok {
			t.Errorf("remaining=%v: Data carries a reason for a backend that reported none", remaining)
		}
	}
}

// "Not reported" must stay distinct from a genuine zero. An unlimited or pay-as-you-go
// account reports no limit, and treating that as exhausted would warn every one of them.
func TestAnUnlimitedAccountIsUsable(t *testing.T) {
	ver := backend.KeyVerification{Valid: true, Usable: boolPtr(true), Reason: backend.ReasonOK}

	c := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")

	if c.Status != StatusOK {
		t.Errorf("status = %s, want ok", c.Status)
	}
	if _, ok := c.Data["limitRemaining"]; ok {
		t.Error("limitRemaining present for an account that reported none")
	}
}

// The two 403s share a remedy and must not share a diagnosis. Told their OAuth client is
// not registered when the backend has just confirmed it IS accepted, a reader goes and
// checks an endpoint and a build that are both fine — which is the entire reason
// auth_permission_denied is a separate code.
func TestTheTwo403sGetDifferentAdvice(t *testing.T) {
	notAllowed := credentialVerdictRow(
		backend.KeyVerification{}, &backend.Error{HTTPStatus: 403, Code: backend.CodeAuthClientNotAllowed, Message: "no"},
		"https://assistant.daintree.org", "")
	denied := credentialVerdictRow(
		backend.KeyVerification{}, &backend.Error{HTTPStatus: 403, Code: backend.CodeAuthPermissionDenied, Message: "no"},
		"https://assistant.daintree.org", "")

	for name, c := range map[string]DoctorCheck{"client_not_allowed": notAllowed, "permission_denied": denied} {
		if c.Status != StatusFail {
			t.Errorf("%s: status = %s, want fail — no turn survives it", name, c.Status)
		}
		if c.Hint == "" {
			t.Errorf("%s: no hint", name)
		}
	}
	if notAllowed.Detail == denied.Detail || notAllowed.Hint == denied.Hint {
		t.Error("the two 403s render identically — the code split bought nothing")
	}
	if strings.Contains(denied.Hint, "DAINTREE_BACKEND_URL") || strings.Contains(denied.Hint, "OAuth client") {
		t.Errorf("permission_denied hint = %q — sends the reader to check a client the backend accepts", denied.Hint)
	}
	if !strings.Contains(notAllowed.Hint, "DAINTREE_BACKEND_URL") {
		t.Errorf("client_not_allowed hint = %q, want the endpoint/build advice", notAllowed.Hint)
	}
}

// "Could not check" must never be reported as a verdict about the credential. A
// transport failure leaves the question open, and answering it would either fail an
// install that works or pass one that does not.
func TestATransportFailureStaysUnknown(t *testing.T) {
	c := credentialVerdictRow(backend.KeyVerification{}, errors.New("dial tcp: connection refused"),
		"https://assistant.daintree.org", "")

	if c.Status != StatusUnknown {
		t.Errorf("status = %s, want unknown", c.Status)
	}
}

// A rejected credential routes its fix to whoever can apply it — and that is ALWAYS the
// deployment, whether or not a caller-supplied bearer is set.
//
// This row reports /v1/daintree/auth/verify, which answers for the credential the backend
// would SPEND, and that is the backend's own upstream credential on every install —
// because the CLI ships no provider key and signing in does not give it one. DAINTREE_API_KEY and
// --api-key-file supply an ACCOUNT bearer: they say who is CALLING, never who pays. The
// copy used to route a rejection at the user whenever one was set, which told them their
// key had been refused upstream when their key never reached the provider at all — and
// contradicted what the same condition says in a turn.
func TestARejectedCredentialAlwaysNamesTheDeployment(t *testing.T) {
	ver := backend.KeyVerification{Valid: false, Reason: backend.ReasonProviderRejected, Detail: "rejected"}

	backends := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")
	callers := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "sk-fake-caller-key")

	if backends.Status != StatusFail || callers.Status != StatusFail {
		t.Fatalf("statuses = %s / %s, want fail", backends.Status, callers.Status)
	}
	for name, hint := range map[string]string{"no caller key": backends.Hint, "caller key set": callers.Hint} {
		if !strings.Contains(hint, "backend-side problem, not yours") {
			t.Errorf("%s: hint = %q, want the deployment named as the owner", name, hint)
		}
	}
	// With a bearer set the reader is told it is in play — and told it is NOT the cause,
	// which is the part that stops them spending an afternoon on it.
	if !strings.Contains(callers.Hint, "not the cause") {
		t.Errorf("caller hint = %q, want it to rule the bearer out explicitly", callers.Hint)
	}
	if strings.Contains(backends.Hint, "caller-supplied") {
		t.Errorf("no-key hint = %q mentions a bearer they never set", backends.Hint)
	}
	if backends.Data["reason"] != backend.ReasonProviderRejected {
		t.Errorf("Data[reason] = %v, want the stable reason", backends.Data["reason"])
	}
}

// `credits_exhausted` alone must decide the copy, with no balance to lean on. Without a
// case where the limit is ABSENT, a passing exhausted test proves only that the
// arithmetic still works — which the implementation this replaced also did.
func TestAnExhaustedReasonAloneDrivesTheCopy(t *testing.T) {
	ver := backend.KeyVerification{Valid: true, Usable: boolPtr(false), Reason: backend.ReasonCreditsExhausted}

	c := credentialVerdictRow(ver, nil, "https://assistant.daintree.org", "")

	if c.Status != StatusFail {
		t.Errorf("status = %s, want fail", c.Status)
	}
	if !strings.Contains(c.Detail, "NO CREDIT") || !strings.Contains(c.Hint, "backend's own upstream account") {
		t.Errorf("detail=%q hint=%q — the reason alone should have selected the exhausted copy", c.Detail, c.Hint)
	}
	if _, ok := c.Data["limitRemaining"]; ok {
		t.Error("limitRemaining present for a verification that reported none")
	}
}

// A reason naming a failure, with NO usable flag and no balance, must not pass green.
// The fallback reads absence as "nothing is wrong", and a backend that states a failure
// in the one field we just declared stable has not stated an absence.
func TestAStatedFailureReasonAloneStillFails(t *testing.T) {
	for _, reason := range []string{backend.ReasonCreditsExhausted, backend.ReasonProviderRejected, "account_suspended"} {
		c := credentialVerdictRow(backend.KeyVerification{Valid: true, Reason: reason},
			nil, "https://assistant.daintree.org", "")
		if c.Status != StatusFail {
			t.Errorf("reason=%s: status = %s, want fail — doctor would exit 0 for an account that cannot run", reason, c.Status)
		}
	}
	// `ok` is a stated SUCCESS and must not be swept up by the same rule.
	c := credentialVerdictRow(backend.KeyVerification{Valid: true, Reason: backend.ReasonOK},
		nil, "https://assistant.daintree.org", "")
	if c.Status != StatusOK {
		t.Errorf("reason=ok: status = %s, want ok", c.Status)
	}
	if c.Data["reason"] != backend.ReasonOK {
		t.Errorf("Data[reason] = %v, want it carried onto the machine-readable half", c.Data["reason"])
	}
}

// A settled PLAN verdict is as blocking as a settled identity one: the login is perfect
// and the account still cannot fund a turn. Reporting it as "could not check" leaves
// doctor exiting 0 for an install that fails on its first turn.
func TestSettledPlanVerdictsGate(t *testing.T) {
	blocking := []string{
		backend.CodeSubscriptionRequired,
		backend.CodeSubscriptionInactive,
		backend.CodeUsageLimitReached,
	}
	seen := map[string]bool{}
	for _, code := range blocking {
		c := credentialVerdictRow(backend.KeyVerification{},
			&backend.Error{HTTPStatus: 402, Code: code, Message: "no"},
			"https://assistant.daintree.org", "")
		if c.Status != StatusFail {
			t.Errorf("%s: status = %s, want fail — it is a verdict, not a gap", code, c.Status)
		}
		if c.Hint == "" {
			t.Errorf("%s: no hint", code)
		}
		if seen[c.Hint] {
			t.Errorf("%s: reuses another verdict's advice — the codes bought nothing", code)
		}
		seen[c.Hint] = true
	}

	// The other half of the rule. "We could not CHECK" is not a verdict, and failing on
	// an outage is how a gate teaches people to ignore it.
	for _, code := range []string{
		backend.CodeAuthDependencyUnavailable,
		backend.CodeEntitlementUnavailable,
		backend.CodeUsageAccountingUnavailable,
		backend.CodeAccountRateLimited,
	} {
		c := credentialVerdictRow(backend.KeyVerification{},
			&backend.Error{HTTPStatus: 503, Code: code, Message: "later"},
			"https://assistant.daintree.org", "")
		if c.Status != StatusUnknown {
			t.Errorf("%s: status = %s, want unknown — an outage is not a verdict", code, c.Status)
		}
	}
}

// The regression Codex caught: an enforcing backend answering `auth_required` was told
// its endpoint must be OLDER than this build, when the actual next step is to sign in.
// Every stable 401 code has a different next step, and the copy has to name it.
func TestEach401CodeGetsItsOwnNextStep(t *testing.T) {
	codes := []string{
		backend.CodeAuthRequired,
		backend.CodeAuthTokenInvalid,
		backend.CodeAuthTokenExpired,
		backend.CodeAuthSessionRevoked,
	}
	hints := map[string]string{}
	for _, code := range codes {
		c := credentialVerdictRow(backend.KeyVerification{},
			&backend.Error{HTTPStatus: 401, Code: code, Message: "no"},
			"https://assistant.daintree.org", "")
		if c.Status != StatusFail {
			t.Errorf("%s: status = %s, want fail", code, c.Status)
		}
		if strings.Contains(c.Hint, "older than this build") {
			t.Errorf("%s: hint = %q — blames the endpoint's age for a current account refusal", code, c.Hint)
		}
		if !strings.Contains(c.Hint, "auth login") {
			t.Errorf("%s: hint = %q, want it to name the credential action", code, c.Hint)
		}
		hints[code] = c.Hint
	}
	if hints[backend.CodeAuthRequired] == hints[backend.CodeAuthSessionRevoked] {
		t.Error("auth_required and auth_session_revoked read identically — never signed in and signed out are different facts")
	}

	// An untyped 401 from an older backend or a rewriting proxy still gates, and must
	// not invent a reason it does not have.
	untyped := credentialVerdictRow(backend.KeyVerification{},
		&backend.Error{HTTPStatus: 401, Message: "nope"}, "https://assistant.daintree.org", "")
	if untyped.Status != StatusFail || untyped.Hint == "" {
		t.Errorf("untyped 401: status=%s hint=%q", untyped.Status, untyped.Hint)
	}
}

// The reason is rendered verbatim so a newer backend can name a condition this build has
// no copy for — which makes it a string from another process reaching the normal screen
// buffer, where the attached session never clears it. Control characters must not survive.
func TestAHostileReasonCannotRewriteTheTerminal(t *testing.T) {
	hostile := "\x1b[2Jwiped\x00" + strings.Repeat("x", 500)
	c := credentialVerdictRow(
		backend.KeyVerification{Valid: true, Usable: boolPtr(false), Reason: hostile},
		nil, "https://assistant.daintree.org", "")

	if strings.ContainsAny(c.Detail, "\x1b\x00") {
		t.Errorf("detail carries control characters: %q", c.Detail)
	}
	if len(c.Detail) > 300 {
		t.Errorf("detail is %d bytes — an unbounded reason buries the rest of the report", len(c.Detail))
	}
	// The machine channel keeps the value whole: a JSON encoder escapes control
	// characters itself, and truncating there would corrupt the one copy a consumer
	// can act on.
	if c.Data["reason"] != hostile {
		t.Error("Data[reason] was altered — the bounding belongs to the human rendering only")
	}
}

// Every verdict the backend can produce, rendered. The point is coverage of the SHAPE
// space rather than the copy: any combination that panics or leaves a failing row
// without an action is a bug, and enumerating them is the cheapest guard there is.
func TestEveryVerificationShapeRenders(t *testing.T) {
	shapes := []struct {
		name string
		ver  backend.KeyVerification
	}{
		{"empty", backend.KeyVerification{}},
		{"valid, nothing else", backend.KeyVerification{Valid: true}},
		{"valid + usable, no limit", backend.KeyVerification{Valid: true, Usable: boolPtr(true)}},
		{"valid + unusable, no limit", backend.KeyVerification{Valid: true, Usable: boolPtr(false)}},
		{"valid + unusable + reason", backend.KeyVerification{Valid: true, Usable: boolPtr(false), Reason: backend.ReasonCreditsExhausted}},
		{"valid + unusable + limit", backend.KeyVerification{Valid: true, Usable: boolPtr(false), LimitRemaining: floatPtr(-3)}},
		{"invalid + limit", backend.KeyVerification{Valid: false, LimitRemaining: floatPtr(12)}},
		{"valid + label", backend.KeyVerification{Valid: true, Usable: boolPtr(true), Label: "fixture key"}},
		{"valid + free tier", backend.KeyVerification{Valid: true, Usable: boolPtr(true), IsFreeTier: true}},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			c := credentialVerdictRow(tc.ver, nil, "https://assistant.daintree.org", "")
			if c.Detail == "" {
				t.Error("no detail")
			}
			if c.Status == StatusFail && c.Hint == "" {
				t.Error("a failing row with no next action")
			}
			// The status is checked against the SHAPE, not against whatever the row
			// chose — otherwise a wrong StatusOK satisfies the hint rule by never
			// reaching it, and the sweep passes on the misclassification it should
			// have caught.
			want := StatusOK
			if !tc.ver.Valid || !tc.ver.IsUsable() {
				want = StatusFail
			}
			if c.Status != want {
				t.Errorf("status = %s, want %s for this shape", c.Status, want)
			}
		})
	}
}
