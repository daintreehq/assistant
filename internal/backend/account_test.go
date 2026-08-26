package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The account taxonomy exists so that identical HTTP statuses can produce opposite
// actions. These tests pin the collisions, because every one of them is a loop or a
// wrong instruction if it regresses.

// Every other test in this file feeds a constant into a classifier that keys on the
// same constant, so a typo in the constant declaration would leave all of them green
// while the backend's correctly-spelled code stopped matching anything. These are the
// strings on the wire, written out by hand, and they are the contract.
func TestTheAccountCodesAreExactlyTheseWireStrings(t *testing.T) {
	want := map[string]string{
		"auth_required":                CodeAuthRequired,
		"auth_token_invalid":           CodeAuthTokenInvalid,
		"auth_token_expired":           CodeAuthTokenExpired,
		"auth_session_revoked":         CodeAuthSessionRevoked,
		"auth_client_not_allowed":      CodeAuthClientNotAllowed,
		"auth_permission_denied":       CodeAuthPermissionDenied,
		"subscription_required":        CodeSubscriptionRequired,
		"subscription_inactive":        CodeSubscriptionInactive,
		"usage_limit_reached":          CodeUsageLimitReached,
		"account_rate_limited":         CodeAccountRateLimited,
		"auth_dependency_unavailable":  CodeAuthDependencyUnavailable,
		"entitlement_unavailable":      CodeEntitlementUnavailable,
		"usage_accounting_unavailable": CodeUsageAccountingUnavailable,
	}
	for literal, constant := range want {
		if literal != constant {
			t.Errorf("wire string %q is declared as %q", literal, constant)
		}
	}
	if len(accountCodes) != len(want) {
		t.Errorf("accountCodes has %d members, want %d — a code was added without a wire test", len(accountCodes), len(want))
	}
	for literal := range want {
		if !accountCodes[literal] {
			t.Errorf("%q is not in accountCodes, so the status fallbacks can still override it", literal)
		}
	}
	// The local code is NOT an account verdict: it never reached the backend.
	if accountCodes[CodeCredentialUnavailable] {
		t.Error("auth_credential_unavailable must not be an account code — no request was sent")
	}
}

// A recognised code must decide on its own. A status is whatever the backend, a proxy,
// or a future refactor happened to attach, and letting it win re-creates every
// collision the taxonomy exists to remove.
func TestARecognisedCodeBeatsAContradictoryStatus(t *testing.T) {
	// A plan problem arriving as a 401 must not send the user to sign in again, only to
	// reach the same 402.
	e := &Error{HTTPStatus: 401, Code: CodeSubscriptionRequired}
	if e.IsAuth() {
		t.Error("subscription_required + 401: IsAuth() = true — would clear a good login")
	}
	if got := e.AuthRemedy(); got != RemedyNone {
		t.Errorf("subscription_required + 401: remedy = %s, want none", got)
	}
	if !e.IsSubscription() {
		t.Error("subscription_required + 401: IsSubscription() = false")
	}
	if isRetriable(e) {
		t.Error("subscription_required + 401: isRetriable() = true")
	}

	// A dependency outage arriving as a 500 must still back off, not fail the turn.
	if !isRetriable(&Error{HTTPStatus: 500, Code: CodeEntitlementUnavailable}) {
		t.Error("entitlement_unavailable + 500: isRetriable() = false, want true")
	}
	// ...and one tagged as a rate limit must still not be replayed as a quota reset.
	quota := &Error{HTTPStatus: 429, Type: "rate_limit_error", Code: CodeUsageLimitReached}
	if quota.IsRateLimited() {
		t.Error("usage_limit_reached tagged rate_limit_error still reads as a rate limit")
	}
	if isRetriable(quota) {
		t.Error("usage_limit_reached tagged rate_limit_error became retriable")
	}
}

// The three 503s reach the CLI mid-stream with HTTPStatus 0, where the status switch
// never fires. Before this was fixed, an auth or billing blip during a turn failed the
// turn outright — the precise opposite of "we could not check, back off and keep the
// user's login".
func TestDependencyOutagesStillBackOffMidStream(t *testing.T) {
	for _, code := range []string{CodeAuthDependencyUnavailable, CodeEntitlementUnavailable, CodeUsageAccountingUnavailable} {
		e := &Error{Code: code, Stream: true} // HTTPStatus 0 — the 200 was already committed
		if !isRetriable(e) {
			t.Errorf("%s (mid-stream): isRetriable() = false — the turn fails instead of backing off", code)
		}
		if got := e.AuthRemedy(); got != RemedyNone {
			t.Errorf("%s: remedy = %s, want none — an outage must not touch credentials", code, got)
		}
		if e.IsAuth() {
			t.Errorf("%s: IsAuth() = true — would log the user out during an outage", code)
		}
	}
}

// A mid-stream 429 carries no status. Recognising it required the stable code, not the
// backend also remembering to set Type.
func TestAccountRateLimitIsRetriableFromItsCodeAlone(t *testing.T) {
	e := &Error{Code: CodeAccountRateLimited, Stream: true}
	if !e.IsRateLimited() {
		t.Error("IsRateLimited() = false for a mid-stream account_rate_limited")
	}
	if !isRetriable(e) {
		t.Error("isRetriable() = false — a transient per-account limit was treated as final")
	}
}

// The two 402s look alike and are not: one wants a checkout, the other a billing
// portal. Suggesting a second purchase to someone whose card merely failed is how
// people end up paying twice.
func TestTheTwo402sAreDistinguishable(t *testing.T) {
	need := &Error{HTTPStatus: 402, Code: CodeSubscriptionRequired}
	have := &Error{HTTPStatus: 402, Code: CodeSubscriptionInactive}
	if need.Code == have.Code {
		t.Fatal("the two subscription codes collapsed")
	}
	if !need.IsSubscription() || !have.IsSubscription() {
		t.Fatal("both must report IsSubscription")
	}
	// The provider's own 402 shares the status and is somebody else's balance entirely.
	credit := &Error{HTTPStatus: 402, Code: CodeProviderInsufficientCredit}
	if credit.IsSubscription() {
		t.Error("provider_insufficient_credits read as a Daintree subscription problem — would send the user to buy a plan they already have")
	}
	if !credit.IsProviderAccount() {
		t.Error("provider_insufficient_credits lost its provider classification")
	}
}

// A local credential failure is not a backend verdict: nothing was sent.
func TestALocalCredentialFailureIsNotABackendRejection(t *testing.T) {
	e := &Error{Code: CodeCredentialUnavailable, Type: "authentication_error"}
	if e.IsAuth() {
		t.Error("IsAuth() = true — doctor would report the backend refused a request it never received")
	}
	if got := e.AuthRemedy(); got != RemedyNone {
		t.Errorf("remedy = %s, want none — a locked keychain is not fixed by signing in", got)
	}
	if isRetriable(e) {
		t.Error("isRetriable() = true — replaying cannot unlock a keychain")
	}
	if taskMayHaveBilled(e) {
		t.Error("taskMayHaveBilled() = true — would permanently caveat the session total over spend that never happened")
	}
}

// errors.Is must still recover whatever sentinel the auth layer raised; a formatted
// message throws that away.
func TestACredentialFailurePreservesItsCause(t *testing.T) {
	sentinel := errSourceUnavailable{}
	wrapped := &Error{Code: CodeCredentialUnavailable, Message: "x", cause: sentinel}
	if !errors.Is(wrapped, sentinel) {
		t.Fatal("the underlying cause was lost")
	}
	if (*Error)(nil).Unwrap() != nil {
		t.Error("Unwrap on a nil *Error should be nil, not a panic")
	}
}

// The predicates guard nil; IsAuth did not, and cli/run.go can reach it through
// errors.As on a wrapped typed-nil.
func TestTheClassifiersSurviveANilError(t *testing.T) {
	var e *Error
	if e.IsAuth() || e.IsSubscription() || e.IsUsageLimited() || e.IsAccountDependency() ||
		e.IsAccountIdentity() || e.IsAccountCode() || e.IsRateLimited() {
		t.Error("a nil *Error classified as something")
	}
	if got := e.AuthRemedy(); got != RemedyNone {
		t.Errorf("nil remedy = %s, want none", got)
	}
}

func TestAuthRemedyStringsAreStable(t *testing.T) {
	want := map[AuthRemedy]string{
		RemedyNone: "none", RemedySignIn: "sign_in", RemedyRefresh: "refresh",
		RemedyRefreshOrSignIn: "refresh_or_sign_in", RemedyClear: "clear",
		RemedyReconfigure: "reconfigure",
	}
	for r, s := range want {
		if got := r.String(); got != s {
			t.Errorf("AuthRemedy(%d).String() = %q, want %q", r, got, s)
		}
	}
}

func TestAuthRemedyDistinguishesTheFourWaysA401CanFail(t *testing.T) {
	cases := []struct {
		code string
		want AuthRemedy
	}{
		{CodeAuthRequired, RemedySignIn},
		{CodeAuthTokenExpired, RemedyRefresh},
		{CodeAuthTokenInvalid, RemedyRefreshOrSignIn},
		{CodeAuthSessionRevoked, RemedyClear},
		{CodeAuthClientNotAllowed, RemedyReconfigure},
	}
	for _, tc := range cases {
		e := &Error{HTTPStatus: 401, Code: tc.code}
		if got := e.AuthRemedy(); got != tc.want {
			t.Errorf("%s: remedy = %s, want %s", tc.code, got, tc.want)
		}
	}
}

// An older backend, or one behind a proxy that rewrote the body, answers 401 with no
// code we know. The safe reading is "ask for a sign-in", never "refresh a token
// nothing said was expired".
func TestAnUntyped401AsksForSignInRatherThanARefresh(t *testing.T) {
	if got := (&Error{HTTPStatus: 401}).AuthRemedy(); got != RemedySignIn {
		t.Fatalf("untyped 401 remedy = %s, want sign_in", got)
	}
	// A 403 is not a login problem by default — see auth_client_not_allowed.
	if got := (&Error{HTTPStatus: 403}).AuthRemedy(); got != RemedyNone {
		t.Fatalf("untyped 403 remedy = %s, want none", got)
	}
}

// The provider account codes ride 401/402/403 too, and they are somebody else's
// problem entirely. A refresh here would re-mint a Daintree credential to fix an
// OpenRouter balance.
//
// Every status shape is swept rather than one being pinned: these codes arrive with no
// status at all mid-stream, and `provider_invalid_api_key` is moving off 401 to a 5xx on
// the backend. The remedy has to be none under all of them. A build that answered on the
// number instead would be offering a sign-in TODAY for the deployment's own upstream
// credential, under the 401 — and after the move it would reach "none" by accident, for a
// reason that says nothing about the code, while every other classification of it stayed
// wrong.
func TestProviderAccountCodesAreNotAnAuthRemedy(t *testing.T) {
	for _, code := range []string{CodeProviderInvalidAPIKey, CodeProviderInsufficientCredit, CodeProviderKeyForbidden} {
		for _, status := range []int{0, 401, 402, 403, 503} {
			e := &Error{HTTPStatus: status, Code: code, Stream: status == 0}
			if got := e.AuthRemedy(); got != RemedyNone {
				t.Errorf("%s at status %d: remedy = %s, want none", code, status, got)
			}
			if e.IsAuth() {
				t.Errorf("%s at status %d: IsAuth() = true, want false", code, status)
			}
		}
	}
}

// The regression this whole file guards, in both its forms: a 403 carrying a valid
// token. Reading either as "sign in" licenses a refresh loop that cannot terminate —
// every refresh produces another token from the same rejected client, or another token
// with the same insufficient authority.
func TestNeither403IsAuthOrRetriable(t *testing.T) {
	for _, code := range []string{CodeAuthClientNotAllowed, CodeAuthPermissionDenied} {
		e := &Error{HTTPStatus: 403, Code: code}
		if e.IsAuth() {
			t.Errorf("%s: IsAuth() = true — a refresh loop; want false", code)
		}
		if isRetriable(e) {
			t.Errorf("%s: isRetriable() = true — a replay loop; want false", code)
		}
		if got := e.AuthRemedy(); got != RemedyReconfigure {
			t.Errorf("%s: remedy = %s, want reconfigure", code, got)
		}
		if !e.IsAccountIdentity() {
			t.Errorf("%s: IsAccountIdentity() = false — it is a verdict about WHO is calling", code)
		}
	}
}

// Both are 429. One clears in seconds; the other does not clear until the billing
// period rolls over. Retrying the second burns the entire backoff budget to re-derive
// the same refusal.
func TestTheTwo429sAreClassifiedOppositely(t *testing.T) {
	rate := &Error{HTTPStatus: 429, Code: CodeAccountRateLimited}
	if !isRetriable(rate) {
		t.Error("account_rate_limited: isRetriable() = false, want true")
	}
	if !rate.IsAccountRateLimited() {
		t.Error("account_rate_limited: IsAccountRateLimited() = false")
	}

	quota := &Error{HTTPStatus: 429, Code: CodeUsageLimitReached}
	if isRetriable(quota) {
		t.Error("usage_limit_reached: isRetriable() = true — burns the backoff budget; want false")
	}
	if !quota.IsUsageLimited() {
		t.Error("usage_limit_reached: IsUsageLimited() = false")
	}
	if quota.IsAccountRateLimited() {
		t.Error("usage_limit_reached must not read as a rate limit")
	}
}

// A subscription problem must never be rendered as a login problem: the login is fine,
// and clearing it would make the user sign in again to reach the same 402.
func TestSubscriptionCodesPreserveTheLogin(t *testing.T) {
	for _, code := range []string{CodeSubscriptionRequired, CodeSubscriptionInactive} {
		e := &Error{HTTPStatus: 402, Code: code}
		if !e.IsSubscription() {
			t.Errorf("%s: IsSubscription() = false", code)
		}
		if e.IsAuth() {
			t.Errorf("%s: IsAuth() = true — would clear a good login", code)
		}
		if got := e.AuthRemedy(); got != RemedyNone {
			t.Errorf("%s: remedy = %s, want none", code, got)
		}
		if isRetriable(e) {
			t.Errorf("%s: isRetriable() = true, want false", code)
		}
	}
}

// "We could not check" is not "the answer is no". These three must keep the user's
// credentials and retry, never present them as signed out or unsubscribed.
func TestDependencyOutagesAreRetriableAndPreserveCredentials(t *testing.T) {
	for _, code := range []string{CodeAuthDependencyUnavailable, CodeEntitlementUnavailable, CodeUsageAccountingUnavailable} {
		e := &Error{HTTPStatus: 503, Code: code}
		if !e.IsAccountDependency() {
			t.Errorf("%s: IsAccountDependency() = false", code)
		}
		if !isRetriable(e) {
			t.Errorf("%s: isRetriable() = false, want true", code)
		}
		if e.IsAuth() || e.IsSubscription() {
			t.Errorf("%s: must not read as a login or plan verdict", code)
		}
	}
}

// The identity codes reach the CLI mid-stream with HTTPStatus 0 (the 200 was already
// committed), so classification must not depend on the status being present.
func TestIdentityCodesClassifyWithoutAnHTTPStatus(t *testing.T) {
	for _, code := range []string{CodeAuthRequired, CodeAuthTokenInvalid, CodeAuthTokenExpired, CodeAuthSessionRevoked} {
		e := &Error{Code: code, Stream: true}
		if !e.IsAuth() {
			t.Errorf("%s (stream, status 0): IsAuth() = false", code)
		}
		if !e.IsAccountIdentity() {
			t.Errorf("%s: IsAccountIdentity() = false", code)
		}
		if isRetriable(e) {
			t.Errorf("%s: isRetriable() = true — the auth ladder owns the one replay", code)
		}
		if e.AuthRemedy() == RemedyNone {
			t.Errorf("%s: lost its remedy mid-stream", code)
		}
	}
	// The two 403s are the odd ones out in every direction: still identity codes, still
	// non-retriable, but never something IsAuth claims. Mid-stream is where that has to
	// hold WITHOUT a status to lean on — a 200 was already committed, so the 403 they
	// would otherwise be recognised by never arrives.
	for _, code := range []string{CodeAuthClientNotAllowed, CodeAuthPermissionDenied} {
		nope := &Error{Code: code, Stream: true}
		if !nope.IsAccountIdentity() {
			t.Errorf("%s (mid-stream): IsAccountIdentity() = false", code)
		}
		if nope.IsAuth() {
			t.Errorf("%s (mid-stream): IsAuth() = true — a refresh loop", code)
		}
		if isRetriable(nope) {
			t.Errorf("%s (mid-stream): isRetriable() = true", code)
		}
		if got := nope.AuthRemedy(); got != RemedyReconfigure {
			t.Errorf("%s (mid-stream): remedy = %s, want reconfigure", code, got)
		}
	}
}

// The two 403s must stay DISTINGUISHABLE even though they share a remedy. Folding them
// into one code would be invisible to every classifier here and wrong only where it
// matters: told their OAuth client is not accepted when it plainly is, whoever reads
// that message goes looking for a registration problem that does not exist.
func TestTheTwo403sAreDistinguishable(t *testing.T) {
	if CodeAuthClientNotAllowed == CodeAuthPermissionDenied {
		t.Fatal("the two 403s collapsed into one wire string")
	}
	for _, code := range []string{CodeAuthClientNotAllowed, CodeAuthPermissionDenied} {
		if !unfixableIdentityCodes[code] {
			t.Errorf("%s is not in unfixableIdentityCodes — IsAuth would license a refresh loop", code)
		}
		// Not a membership restatement: this is the BEHAVIOUR that membership buys.
		// A proxy or a future backend reshaping one of these can attach a rate-limit
		// type to it, and IsRateLimited would then read it as transient — so the
		// deterministic check has to win, which it only does by being reached first.
		mislabelled := &Error{HTTPStatus: 403, Code: code, Type: "rate_limit_error"}
		if !mislabelled.IsRateLimited() {
			t.Fatalf("%s: the test's premise is gone — IsRateLimited no longer reads Type", code)
		}
		if isRetriable(mislabelled) {
			t.Errorf("%s carrying a rate_limit_error type is being replayed — a settled 403 must beat IsRateLimited", code)
		}
	}
	// The set is exactly those two: a code that a refresh CAN fix must never be added
	// here, because that is the one mistake this set makes silent.
	if len(unfixableIdentityCodes) != 2 {
		t.Errorf("unfixableIdentityCodes has %d members, want 2", len(unfixableIdentityCodes))
	}
}

// The negative half of the taxonomy. A code in two category sets at once is how a
// settled refusal starts being retried as an outage or rendered as a plan problem, and
// every classifier below answers from a DIFFERENT map — so nothing else here would
// notice the overlap.
func TestNeither403LeaksIntoAnotherCategory(t *testing.T) {
	for _, code := range []string{CodeAuthClientNotAllowed, CodeAuthPermissionDenied} {
		e := &Error{HTTPStatus: 403, Code: code}
		if e.IsAccountDependency() {
			t.Errorf("%s: IsAccountDependency() = true — it would be replayed as an outage", code)
		}
		if e.IsSubscription() {
			t.Errorf("%s: IsSubscription() = true — it would be rendered as a plan problem", code)
		}
		if e.IsUpstreamAuth() || e.IsProviderAccount() {
			t.Errorf("%s: read as an UPSTREAM account problem — it is a verdict at our own door", code)
		}
		if e.IsUsageLimited() || e.IsAccountRateLimited() {
			t.Errorf("%s: read as a 429", code)
		}
		if !e.IsAccountCode() {
			t.Errorf("%s: IsAccountCode() = false — the status fallbacks can still override it", code)
		}
	}
}

// --- TokenSource ------------------------------------------------------------------

// recordingSource counts fetches so a test can prove a probe never asked for one.
type recordingSource struct {
	mu     sync.Mutex
	token  string
	err    error
	calls  int
	killed []string
}

func (r *recordingSource) AccessToken(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.token, r.err
}

func (r *recordingSource) Invalidate(tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.killed = append(r.killed, tok)
}

func (r *recordingSource) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// The bootstrap property: /healthz, /readyz, /version and the discovery endpoint are
// what someone reaches for when their login is broken. Making them wait on — or fail
// with — a credential fetch takes the diagnostic offline exactly when it is needed.
func TestPublicProbesNeverRequestACredential(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	src := &recordingSource{token: "must-not-be-sent"}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: src})

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatalf("Version: %v", err)
	}
	if src.count() != 0 {
		t.Errorf("public probes fetched a token %d times, want 0", src.count())
	}
	for i, h := range gotAuth {
		if h != "" {
			t.Errorf("probe %d sent Authorization %q, want none", i, h)
		}
	}
}

func TestProtectedRequestsCarryTheCurrentToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()

	src := &recordingSource{token: "tok-one"}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: src})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if got != "Bearer tok-one" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok-one")
	}

	// The whole point of the indirection: a rotation reaches the next request without
	// rebuilding the client.
	src.mu.Lock()
	src.token = "tok-two"
	src.mu.Unlock()
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey after rotation: %v", err)
	}
	if got != "Bearer tok-two" {
		t.Fatalf("after rotation Authorization = %q, want %q — the client is pinned to a stale token", got, "Bearer tok-two")
	}
}

// Sending the request bare would silently downgrade an authenticated session to an
// anonymous one — billing or refusing the wrong principal behind a successful-looking
// call. Failing loudly is the only safe answer.
func TestACredentialFailureAbortsRatherThanSendingAnonymously(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	src := &recordingSource{err: errSourceUnavailable{}}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: src, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.VerifyKey(context.Background())
	if err == nil {
		t.Fatal("VerifyKey succeeded with an unavailable credential source")
	}
	if reached {
		t.Error("the request was sent anyway, without an Authorization header")
	}
	// Not auth_required: nothing was sent, so this is not the backend asking us to sign
	// in. Conflating the two would tell someone with a locked keychain to open a browser.
	if !strings.Contains(err.Error(), CodeCredentialUnavailable) {
		t.Errorf("error = %v, want it to carry %s", err, CodeCredentialUnavailable)
	}
	if strings.Contains(err.Error(), CodeAuthRequired) {
		t.Errorf("error = %v must not claim the backend demanded a sign-in", err)
	}
}

type errSourceUnavailable struct{}

func (errSourceUnavailable) Error() string { return "credential store locked" }

// APIKey is sugar for a StaticTokenSource, so every existing caller keeps working.
func TestAPIKeyStillConfiguresTheBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: "  legacy-key  "})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if got != "Bearer legacy-key" {
		t.Fatalf("Authorization = %q, want the trimmed legacy key", got)
	}
}

// A caller that built a live source has strictly more information than one that froze
// a string. Preferring the frozen one would pin the client to a token that stops
// working in an hour.
func TestTokenSourceWinsOverAFrozenAPIKey(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: "frozen", TokenSource: StaticTokenSource{Token: "live"}})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if got != "Bearer live" {
		t.Fatalf("Authorization = %q, want the TokenSource value", got)
	}
}

func TestAnUnconfiguredClientSendsNoAuthorizationHeader(t *testing.T) {
	seen := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if seen != "" {
		t.Fatalf("Authorization = %q, want none — the open door expects an anonymous request", seen)
	}
}

// Scrubbing used to read the client's one frozen key. With a rotating credential there
// is no single key to read, so the source reports what it issued.
func TestScrubbingFollowsTheTokenSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"bad","message":"rejected credential sekrit-abc"}}`))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: "sekrit-abc", Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.VerifyKey(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sekrit-abc") {
		t.Fatalf("the echoed credential survived into %q", err.Error())
	}
}

// NoTokenSource must not claim any secrets — an install with no credential should cost
// nothing on the error path.
func TestNoTokenSourceReportsNoSecrets(t *testing.T) {
	if _, ok := any(NoTokenSource{}).(TokenScrubber); ok {
		t.Fatal("NoTokenSource should not implement TokenScrubber")
	}
	if got := (StaticTokenSource{}).Secrets(); got != nil {
		t.Fatalf("an empty StaticTokenSource reported secrets %v", got)
	}
}
