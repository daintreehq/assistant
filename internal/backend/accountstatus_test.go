package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// accountstatus_test.go pins the /v1/daintree/account contract.
//
// One theme runs through every case: a STATUS READ must never be able to turn a working
// login into a signed-out or unsubscribed one. So the malformed cases assert the error
// CLASS as well as the failure — a contract complaint that IsAuth or IsSubscription
// answered true for would send the user to a sign-in or a checkout on the strength of a
// backend bug — and they assert the returned status is EMPTY, because a half-decoded
// `granted` handed back beside an error is the same failure wearing a different hat.

// accountRecorder is a scripted account endpoint. Every field a handler goroutine writes
// is behind the mutex and read through an accessor: the test goroutine reads these after
// the call returns, and the race detector runs on CI.
type accountRecorder struct {
	mu      sync.Mutex
	hits    int
	methods []string
	paths   []string
	auth    []string
}

func (r *accountRecorder) record(req *http.Request) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.hits
	r.hits++
	r.methods = append(r.methods, req.Method)
	r.paths = append(r.paths, req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	return n
}

func (r *accountRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func (r *accountRecorder) bearers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

func (r *accountRecorder) requests() (methods, paths []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.methods...), append([]string(nil), r.paths...)
}

// accountServer serves a body chosen per attempt. contentType is honoured so a
// non-JSON body can be exercised.
func accountServer(t *testing.T, tokens []string, reply func(n int) (status int, body string)) (*Client, *accountRecorder, *fakeObserver) {
	t.Helper()
	rec := &accountRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rec.record(r)
		status, body := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	obs := &fakeObserver{tokens: tokens}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs, Retry: RetryPolicy{MaxAttempts: 1}})
	return c, rec, obs
}

// accountOK is the shorthand for a server that always answers 200 with one body.
func accountOK(t *testing.T, body string) (*Client, *accountRecorder) {
	t.Helper()
	c, rec, _ := accountServer(t, nil, func(int) (int, string) { return http.StatusOK, body })
	return c, rec
}

const grantedBody = `{"version":1,"email":"person@example.com","subject_hash":"0123456789abcdef",` +
	`"access":"granted","plan_id":"standard","subscription_status":"active",` +
	`"entitlement_source":"polar","entitlement_stale":false,"checked_at":"2026-08-25T12:00:00Z"}`

// All four access verdicts decode, and each keeps the fields a caller renders.
func TestAccountDecodesEveryAccessVerdict(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		access      string
		plan        string
		hash        string
		wantInstant string
	}{
		{"granted", grantedBody, AccessGranted, PlanStandard, "0123456789abcdef", "2026-08-25T12:00:00Z"},
		{
			"subscription required",
			`{"version":1,"access":"subscription_required","subscription_status":"none","checked_at":"2026-08-25T12:00:00+02:00"}`,
			AccessSubscriptionRequired, "", "", "2026-08-25T10:00:00Z",
		},
		{
			"subscription inactive",
			`{"version":1,"access":"subscription_inactive","plan_id":"pro","subscription_status":"past_due",` +
				`"entitlement_source":"polar","checked_at":"2026-08-25T12:00:00Z"}`,
			AccessSubscriptionInactive, PlanPro, "", "2026-08-25T12:00:00Z",
		},
		{
			"unverified",
			`{"version":1,"access":"unverified","checked_at":"2026-08-25T12:00:00Z"}`,
			AccessUnverified, "", "", "2026-08-25T12:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := accountOK(t, tc.body)
			got, err := c.Account(context.Background())
			if err != nil {
				t.Fatalf("Account: %v", err)
			}
			if got.Access != tc.access {
				t.Errorf("access = %q, want %q", got.Access, tc.access)
			}
			if got.PlanID != tc.plan {
				t.Errorf("plan = %q, want %q", got.PlanID, tc.plan)
			}
			if got.SubjectHash != tc.hash {
				t.Errorf("subjectHash = %q, want %q", got.SubjectHash, tc.hash)
			}
			// The exact instant, not merely "non-zero": the +02:00 row exists to prove
			// the offset is honoured rather than dropped, and a zero-check would pass
			// on a parser that read the wall clock and ignored the zone.
			if got := got.CheckedAtTime.UTC().Format("2006-01-02T15:04:05Z"); got != tc.wantInstant {
				t.Errorf("checkedAt = %s, want %s", got, tc.wantInstant)
			}
			if got.Granted() != (tc.access == AccessGranted) {
				t.Errorf("Granted() = %v for access %q", got.Granted(), tc.access)
			}
		})
	}
}

// A backend must be able to add optional metadata without an older CLI refusing the
// whole answer — otherwise every future field is a breaking change.
func TestAccountIgnoresUnknownFieldsAndAbsentOptionals(t *testing.T) {
	const body = `{"version":1,"access":"unverified","checked_at":"2026-08-25T12:00:00Z",` +
		`"future_field":{"nested":[1,2,3]},"plan_id":null,"email":null}`
	c, _ := accountOK(t, body)
	got, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("an unknown field broke the whole response: %v", err)
	}
	if got.Email != "" || got.PlanID != "" {
		t.Errorf("a null optional decoded to something: email=%q plan=%q", got.Email, got.PlanID)
	}
}

// Every rejection fails CLOSED: a protocol complaint, an EMPTY status, and a credential
// nothing was asked to do anything about.
//
// The empty-status half is the one worth stating plainly. Returning a half-decoded body
// beside an error invites a caller that logs the error and uses the value anyway — and
// the tempting-verdict row below is exactly the body that would make that mistake
// expensive, since it carries a fully-formed `granted` under a version we cannot read.
func TestAccountRejectsMalformedBodiesAsProtocolErrors(t *testing.T) {
	cases := []struct{ name, body, code string }{
		{"unknown version", `{"version":2,"access":"granted","plan_id":"pro","entitlement_source":"polar","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"missing version", `{"access":"granted","plan_id":"pro","entitlement_source":"polar","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"unknown access", `{"version":1,"access":"maybe","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"absent access", `{"version":1,"checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"granted with no plan", `{"version":1,"access":"granted","entitlement_source":"polar","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"granted with no entitlement source", `{"version":1,"access":"granted","plan_id":"pro","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"unknown plan", `{"version":1,"access":"granted","plan_id":"platinum","entitlement_source":"polar","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"unknown entitlement source", `{"version":1,"access":"unverified","entitlement_source":"guesswork","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"stale without a cache source", `{"version":1,"access":"granted","plan_id":"pro","entitlement_source":"polar","entitlement_stale":true,"checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"missing checked_at", `{"version":1,"access":"unverified"}`, CodeAccountContractInvalid},
		{"naive checked_at", `{"version":1,"access":"unverified","checked_at":"2026-08-25T12:00:00"}`, CodeAccountContractInvalid},
		{"uppercase subject hash", `{"version":1,"access":"unverified","subject_hash":"0123456789ABCDEF","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"short subject hash", `{"version":1,"access":"unverified","subject_hash":"0123456789abcde","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"long subject hash", `{"version":1,"access":"unverified","subject_hash":"0123456789abcdef0","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"non-hex subject hash", `{"version":1,"access":"unverified","subject_hash":"zzzzzzzzzzzzzzzz","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"oversized email", `{"version":1,"access":"unverified","email":"` + strings.Repeat("a", 400) + `","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		{"oversized subscription status", `{"version":1,"access":"unverified","subscription_status":"` + strings.Repeat("a", 400) + `","checked_at":"2026-08-25T12:00:00Z"}`, CodeAccountContractInvalid},
		// The decode failures carry the transport's own code, not ours. They are in the
		// same table because the SAFETY property is identical: neither may read as an
		// account verdict, and neither may hand back a usable status.
		{"wrong type for version", `{"version":"1","access":"unverified","checked_at":"2026-08-25T12:00:00Z"}`, "decode"},
		{"wrong type for stale flag", `{"version":1,"access":"unverified","entitlement_stale":"yes","checked_at":"2026-08-25T12:00:00Z"}`, "decode"},
		{"empty body", ``, "decode"},
		{"not JSON at all", `<html><body>gateway error</body></html>`, "decode"},
		{"a JSON array", `[{"version":1,"access":"granted"}]`, "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := accountOK(t, tc.body)
			got, err := c.Account(context.Background())
			if err == nil {
				t.Fatal("a malformed account body was accepted")
			}
			var be *Error
			if !errors.As(err, &be) {
				t.Fatalf("error is not a *backend.Error: %v", err)
			}
			if be.Code != tc.code {
				t.Errorf("code = %q, want %q (%v)", be.Code, tc.code, err)
			}
			// The safety property. None of these may read as a statement about the
			// account, or the credential layer will act on a backend bug.
			if be.IsAuth() || be.IsSubscription() || be.IsAccountCode() || be.IsAccountDependency() {
				t.Errorf("a contract failure was classified as an account verdict (code %q)", be.Code)
			}
			if be.AuthRemedy() != RemedyNone {
				t.Errorf("a contract failure asked the auth layer to act: %v", be.AuthRemedy())
			}
			if isRetriable(be) {
				t.Error("a malformed body would replay identically; it must not be retriable")
			}
			// Nothing usable comes back, so a caller that logs the error and reads the
			// value anyway still cannot act on a verdict the backend never validly sent.
			if got.Access != "" || got.PlanID != "" || got.Granted() || !got.CheckedAtTime.IsZero() {
				t.Errorf("a rejected response still handed back a usable status: %+v", got)
			}
			if n := rec.count(); n != 1 {
				t.Errorf("requests = %d, want exactly 1 — a deterministic refusal was replayed", n)
			}
		})
	}
}

// The rule failures name the rule in OUR words. The offending value is never quoted: a
// backend that echoed a bearer into a field would otherwise have it repeated through the
// error, the debug log and a support bundle.
func TestAccountContractFailuresNameTheRuleWithoutQuotingTheValue(t *testing.T) {
	const secret = "super-secret-bearer-value"
	c, _, _ := accountServer(t, []string{secret}, func(int) (int, string) {
		return http.StatusOK, `{"version":1,"access":"unverified","email":"` + strings.Repeat(secret, 40) + `","checked_at":"2026-08-25T12:00:00Z"}`
	})
	_, err := c.Account(context.Background())
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeAccountContractInvalid {
		t.Fatalf("code = %v, want %q", err, CodeAccountContractInvalid)
	}
	if !strings.Contains(be.Message, "email is too long") {
		t.Errorf("the message does not name the rule that failed: %q", be.Message)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the rejected value was quoted back into the error")
	}
}

// A bearer echoed by a hostile or broken backend must not reach a caller through ANY
// field. Each row echoes it into a different one, which is how the two halves of the
// defence get checked separately: the constrained fields REJECT the response (the token
// is not a valid enum member, hash or timestamp), and the two free-text fields SCRUB it.
func TestAccountNeverReturnsAnEchoedCredential(t *testing.T) {
	const secret = "sk-live-abcdef0123456789"
	fields := []string{"email", "subject_hash", "access", "plan_id", "subscription_status", "entitlement_source", "checked_at"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			body := map[string]string{
				"email":               `{"version":1,"access":"unverified","email":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"subject_hash":        `{"version":1,"access":"unverified","subject_hash":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"access":              `{"version":1,"access":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"plan_id":             `{"version":1,"access":"subscription_inactive","plan_id":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"subscription_status": `{"version":1,"access":"unverified","subscription_status":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"entitlement_source":  `{"version":1,"access":"unverified","entitlement_source":%q,"checked_at":"2026-08-25T12:00:00Z"}`,
				"checked_at":          `{"version":1,"access":"unverified","checked_at":%q}`,
			}[field]

			c, _, _ := accountServer(t, []string{secret}, func(int) (int, string) {
				return http.StatusOK, fmt.Sprintf(body, secret)
			})
			got, err := c.Account(context.Background())
			// Either outcome is acceptable — refused, or returned with the value gone.
			// What is NOT acceptable is the token reaching a caller either way.
			if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, secret) {
				t.Errorf("the bearer survived into the returned status: %s", rendered)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("the bearer survived into the error: %v", err)
			}
		})
	}
}

// The two free-text fields are scrubbed rather than merely happening to be rejected —
// the case above passes trivially if a field is refused, so this pins the scrub itself.
func TestAccountScrubsTheFreeTextDisplayFields(t *testing.T) {
	const secret = "sk-live-abcdef0123456789"
	c, _, _ := accountServer(t, []string{secret}, func(int) (int, string) {
		return http.StatusOK, `{"version":1,"access":"unverified","email":"` + secret +
			`","subscription_status":"` + secret + `","checked_at":"2026-08-25T12:00:00Z"}`
	})
	got, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if got.Email == "" || got.SubscriptionStatus == "" {
		t.Fatalf("the fields were dropped rather than scrubbed: %+v", got)
	}
	if strings.Contains(got.Email, secret) || strings.Contains(got.SubscriptionStatus, secret) {
		t.Fatalf("the bearer survived into a rendered field: %+v", got)
	}
}

// Validation runs on the RAW body, before scrubbing. A response whose oversized field is
// only under the bound BECAUSE a replacement is shorter must still be refused — otherwise
// the redaction step quietly widens the contract.
func TestAccountValidatesBeforeScrubbing(t *testing.T) {
	secret := strings.Repeat("s", 300) // alone, over the 320-byte email bound when doubled
	c, _, _ := accountServer(t, []string{secret}, func(int) (int, string) {
		return http.StatusOK, `{"version":1,"access":"unverified","email":"` + secret + secret +
			`","checked_at":"2026-08-25T12:00:00Z"}`
	})
	if _, err := c.Account(context.Background()); err == nil {
		t.Fatal("an oversized email passed because scrubbing shortened it first")
	}
}

// A backend refusal keeps its own taxonomy: the account endpoint is not special.
func TestAccountSurfacesBackendAccountErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		code     string
		wantAuth bool
		wantSub  bool
		wantDep  bool
		remedy   AuthRemedy
	}{
		{"no plan", http.StatusPaymentRequired, CodeSubscriptionRequired, false, true, false, RemedyNone},
		{"plan inactive", http.StatusPaymentRequired, CodeSubscriptionInactive, false, true, false, RemedyNone},
		{"identity dependency down", http.StatusServiceUnavailable, CodeAuthDependencyUnavailable, false, false, true, RemedyNone},
		{"billing dependency down", http.StatusServiceUnavailable, CodeEntitlementUnavailable, false, false, true, RemedyNone},
		{"session revoked", http.StatusUnauthorized, CodeAuthSessionRevoked, true, false, false, RemedyClear},
		{"client not allowed", http.StatusForbidden, CodeAuthClientNotAllowed, false, false, false, RemedyReconfigure},
		{"permission denied", http.StatusForbidden, CodeAuthPermissionDenied, false, false, false, RemedyReconfigure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, obs := accountServer(t, []string{"token-a"}, func(int) (int, string) {
				return tc.status, `{"error":{"type":"invalid_request_error","code":"` + tc.code + `","message":"no"}}`
			})
			_, err := c.Account(context.Background())
			var be *Error
			if !errors.As(err, &be) {
				t.Fatalf("the status endpoint lost its taxonomy: %v", err)
			}
			if be.Code != tc.code {
				t.Fatalf("code = %q, want %q", be.Code, tc.code)
			}
			if be.IsAuth() != tc.wantAuth || be.IsSubscription() != tc.wantSub || be.IsAccountDependency() != tc.wantDep {
				t.Errorf("classification wrong for %s: auth=%v sub=%v dep=%v", tc.code,
					be.IsAuth(), be.IsSubscription(), be.IsAccountDependency())
			}
			if be.AuthRemedy() != tc.remedy {
				t.Errorf("remedy = %v, want %v", be.AuthRemedy(), tc.remedy)
			}
			// The FAILURE observation is not suppressed — only the success one is. A
			// revocation that never reached the state machine would leave a dead
			// credential on disk, and a dependency outage that never reached it would
			// leave the state claiming a verified session.
			if _, verdicts := obs.counts(); verdicts != 1 {
				t.Errorf("verdicts = %d, want 1 — the account path swallowed a failure", verdicts)
			}
			if active, _ := obs.counts(); active != 0 {
				t.Errorf("MarkActive calls = %d on a failed status read", active)
			}
		})
	}
}

// The endpoint's own 200 must NOT confirm the session — the decoded body is the verdict,
// and a body saying subscription_required would otherwise be preceded by a MarkActive
// that reports the account as cleared to spend.
func TestAccountDoesNotMarkTheSessionActiveOnItsOwn200(t *testing.T) {
	c, _, obs := accountServer(t, []string{"token-a"}, func(int) (int, string) {
		return http.StatusOK, `{"version":1,"access":"subscription_required","checked_at":"2026-08-25T12:00:00Z"}`
	})
	if _, err := c.Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}
	if active, verdicts := obs.counts(); active != 0 || verdicts != 0 {
		t.Errorf("observed %d successes and %d verdicts; the status read must observe neither", active, verdicts)
	}
}

// Even a GRANTED body does not confirm through the transport. The verdict travels in the
// return value, so a caller that ignored it would learn nothing — which is the point:
// there is exactly one place the account answer is applied.
func TestAccountDoesNotMarkTheSessionActiveEvenWhenGranted(t *testing.T) {
	c, _, obs := accountServer(t, []string{"token-a"}, func(int) (int, string) {
		return http.StatusOK, grantedBody
	})
	if _, err := c.Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}
	if active, _ := obs.counts(); active != 0 {
		t.Errorf("MarkActive calls = %d, want 0", active)
	}
}

// Every OTHER protected endpoint keeps the automatic confirmation. The suppression above
// is one endpoint's exception, not a hole in the observer.
func TestOtherProtectedEndpointsStillMarkActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol_version":3}`))
	}))
	defer srv.Close()

	obs := &fakeObserver{tokens: []string{"token-a"}}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs, Retry: RetryPolicy{MaxAttempts: 1}})

	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if active, _ := obs.counts(); active != 1 {
		t.Errorf("MarkActive calls = %d, want 1", active)
	}
}

// The status read presents the bearer and inherits the one-refresh/one-replay ladder,
// exactly like every other protected JSON call.
func TestAccountRefreshesAndReplaysOnceOnAnExpiredToken(t *testing.T) {
	// Three entries because doJSONRetry SAMPLES the credential once before the first
	// attempt (to attribute the outcome) and then fetches it again per attempt: the
	// sample takes the first, the first request the second, the renewal the third.
	c, rec, obs := accountServer(t, []string{"token-a", "token-a", "token-b"}, func(n int) (int, string) {
		if n == 0 {
			return http.StatusUnauthorized,
				`{"error":{"type":"authentication_error","code":"auth_token_expired","message":"stale"}}`
		}
		return http.StatusOK, grantedBody
	})

	got, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("the expired token was not renewed: %v", err)
	}
	if got.Access != AccessGranted {
		t.Errorf("access = %q after the replay", got.Access)
	}
	if n := rec.count(); n != 2 {
		t.Fatalf("requests = %d, want exactly 2", n)
	}
	seen := rec.bearers()
	if seen[0] != "Bearer token-a" || seen[1] != "Bearer token-b" {
		t.Errorf("the replay did not present a fresh credential: %v", seen)
	}
	// The refresh is CAUSED by the verdict, not by the passage of time: exactly one
	// verdict reaches the observer, and it is the expiry. Without this a client that
	// dropped acct.failed and refreshed unconditionally would still pass.
	verdicts := obs.verdictCodes()
	if len(verdicts) != 1 || verdicts[0] != CodeAuthTokenExpired {
		t.Errorf("verdicts = %v, want exactly [%s]", verdicts, CodeAuthTokenExpired)
	}
	// The success still does not confirm — the ladder does not smuggle a MarkActive in
	// through the replay.
	if active, _ := obs.counts(); active != 0 {
		t.Errorf("MarkActive calls = %d after a replayed status read", active)
	}
}

// ONE replay is the ceiling. A backend that keeps saying "expired" gets asked twice and
// no more; looping on that is how a client hammers a door that will keep saying no.
func TestAccountReplaysAtMostOnce(t *testing.T) {
	c, rec, _ := accountServer(t, []string{"token-a", "token-a", "token-b", "token-c"}, func(int) (int, string) {
		return http.StatusUnauthorized,
			`{"error":{"type":"authentication_error","code":"auth_token_expired","message":"stale"}}`
	})
	if _, err := c.Account(context.Background()); err == nil {
		t.Fatal("a persistently expired credential reported an account")
	}
	if n := rec.count(); n != 2 {
		t.Errorf("requests = %d, want exactly 2 (one attempt, one replay)", n)
	}
}

// A settled refusal is answered once. Refreshing mints another credential wrong in the
// same way, so both 403s must stop at the first answer.
func TestAccountDoesNotReplayASettledRefusal(t *testing.T) {
	for _, code := range []string{CodeAuthClientNotAllowed, CodeAuthPermissionDenied} {
		t.Run(code, func(t *testing.T) {
			c, rec, _ := accountServer(t, []string{"token-a", "token-a", "token-b"}, func(int) (int, string) {
				return http.StatusForbidden,
					`{"error":{"type":"authentication_error","code":"` + code + `","message":"no"}}`
			})
			if _, err := c.Account(context.Background()); err == nil {
				t.Fatal("a refused credential reported an account")
			}
			if n := rec.count(); n != 1 {
				t.Fatalf("requests = %d, want 1 — a settled 403 was replayed", n)
			}
			if seen := rec.bearers(); seen[0] != "Bearer token-a" {
				t.Errorf("first request presented %q, want the sampled credential", seen[0])
			}
		})
	}
}

// A cancelled call stops without reaching the backend, and — the part that matters —
// without producing anything the account layer would act on.
//
// It deliberately does NOT assert errors.Is(err, context.Canceled): the transport wraps
// a dial failure as a bare `connect` *Error with no cause, so the sentinel is not
// recoverable on this path. That is pre-existing shared behaviour and not what this
// endpoint needs pinned. What it needs pinned is that walking away from a status read
// cannot be mistaken for a verdict about the session.
func TestAccountStopsOnCancellationWithoutAVerdict(t *testing.T) {
	c, rec, obs := accountServer(t, []string{"token-a"}, func(int) (int, string) {
		return http.StatusOK, grantedBody
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.Account(ctx)
	if err == nil {
		t.Fatal("a cancelled status read reported an account")
	}
	var be *Error
	if errors.As(err, &be) && (be.IsAuth() || be.IsSubscription() || be.IsAccountCode()) {
		t.Errorf("a cancellation was classified as an account verdict (code %q)", be.Code)
	}
	if got.Access != "" {
		t.Errorf("a cancelled read handed back a status: %+v", got)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("requests = %d, want 0 — the call went out after cancellation", n)
	}
	if active, _ := obs.counts(); active != 0 {
		t.Errorf("MarkActive calls = %d on a cancelled read", active)
	}
}

// The route and the METHOD are the ones the contract names. A regression to POST would
// otherwise stay green against a server that answers anything.
func TestAccountUsesTheContractRoute(t *testing.T) {
	if AccountStatusPath != "/v1/daintree/account" {
		t.Fatalf("AccountStatusPath = %q", AccountStatusPath)
	}
	c, rec := accountOK(t, grantedBody)
	if _, err := c.Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}
	methods, paths := rec.requests()
	if len(methods) != 1 {
		t.Fatalf("requests = %d, want 1", len(methods))
	}
	if methods[0] != http.MethodGet {
		t.Errorf("method = %s, want GET", methods[0])
	}
	if paths[0] != AccountStatusPath {
		t.Errorf("path = %s, want %s", paths[0], AccountStatusPath)
	}
}
