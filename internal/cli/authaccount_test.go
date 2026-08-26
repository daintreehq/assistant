package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/backend/accountfixture"
	"github.com/daintreehq/assistant/internal/config"
)

// authaccount_test.go covers what `auth status --refresh` and `auth login` actually DO
// and SAY about a plan.
//
// The two rules underneath every case:
//
//  1. A missing or lapsed plan is a perfectly good login. Rendering either as a sign-in
//     problem sends someone to re-authenticate their way out of a billing problem, which
//     cannot work — and a lapsed plan must never be offered a second checkout.
//  2. `--refresh` is the ONLY path that contacts the backend. A plain status read stays
//     answerable while the network is down, because that is when it is asked.

// accountDeployment is a fake backend serving discovery, the OAuth token endpoint and
// the account status route.
type accountDeployment struct {
	srv           *httptest.Server
	accountCalls  atomic.Int64
	tokenCalls    atomic.Int64
	configPresent bool
	// reply answers the account route. Nil serves a granted standard plan.
	reply func(w http.ResponseWriter)
}

func newAccountDeployment(t *testing.T, configured bool) *accountDeployment {
	t.Helper()
	d := &accountDeployment{configPresent: configured}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/daintree/auth/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !d.configPresent {
			// The four-field answer a deployment with no identity provider gives: a
			// CORRECT response that fails manifest validation by design.
			_, _ = w.Write([]byte(`{"version":1,"environment":"development","configured":false,"required":false}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "environment": "development", "configured": true, "required": false,
			"issuer":                 d.srv.URL + "/auth/v1",
			"authorization_endpoint": d.srv.URL + "/auth/v1/oauth/authorize",
			"token_endpoint":         d.srv.URL + "/auth/v1/oauth/token",
			"jwks_uri":               d.srv.URL + "/auth/v1/.well-known/jwks.json",
			"client_id":              "test-client",
			"redirect_uri":           auth.RedirectURI(),
			"scopes":                 []string{"openid", "email"},
			"account_url":            "https://staging.daintree.org/account",
			"subscribe_url":          "https://staging.daintree.org/subscribe",
			"session_policy":         map[string]any{"access_token_seconds": 3600, "session_max_age_seconds": 2592000},
		})
	})

	mux.HandleFunc("/auth/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		n := d.tokenCalls.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"access_token":"access-%d","refresh_token":"refresh-%d","token_type":"bearer","expires_in":3600}`, n, n)))
	})

	mux.HandleFunc(backend.AccountStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		d.accountCalls.Add(1)
		if d.reply != nil {
			d.reply(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// THE CANONICAL BODY, not a copy of it. Four packages decode this contract, and
		// four hand-written examples of one response is how both ends of it came to be
		// green against documents that did not match.
		_, _ = w.Write(accountfixture.Body(accountfixture.GrantedStandard))
	})

	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// signedInAgainst returns a Manager holding a live session against d, plus a matching
// config for the command under test.
func signedInAgainst(t *testing.T, d *accountDeployment) (*auth.Manager, config.AppConfig) {
	t.Helper()
	root := t.TempDir()
	mgr, err := auth.NewManager(auth.Options{
		StateRoot: root, BackendURL: d.srv.URL, Store: auth.NewMemoryStore(), Opener: auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if d.configPresent {
		man, mErr := mgr.Manifest(context.Background())
		if mErr != nil {
			t.Fatalf("Manifest: %v", mErr)
		}
		if err := mgr.SeedForTest(context.Background(), man, "refresh-seed"); err != nil {
			t.Fatalf("SeedForTest: %v", err)
		}
	}
	return mgr, config.AppConfig{StateRoot: root, BackendURL: d.srv.URL}
}

// runStatus executes the status command and returns its human output and exit code.
func runStatus(t *testing.T, mgr *auth.Manager, cfg config.AppConfig, refresh bool) (string, int) {
	t.Helper()
	var out bytes.Buffer
	exit := runAuthStatus(context.Background(), authWriter{out: &out, err: &out}, mgr, cfg,
		AuthOptions{Action: AuthStatus, Refresh: refresh})
	return out.String(), exit
}

// --refresh makes EXACTLY ONE account request and puts the answer on the status block.
// This is the whole gap the command had: a flag that renewed the Supabase token and then
// printed plan fields nothing populated.
func TestRefreshMakesExactlyOneLiveAccountRequest(t *testing.T) {
	d := newAccountDeployment(t, true)
	mgr, cfg := signedInAgainst(t, d)

	got, exit := runStatus(t, mgr, cfg, true)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if n := d.accountCalls.Load(); n != 1 {
		t.Fatalf("account requests = %d, want exactly 1", n)
	}
	for _, want := range []string{accountfixture.Email, "standard", "polar", "signed in"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Without --refresh nothing reaches the backend. Someone running plain `auth status`
// during an outage is asking what this machine knows, and a silent billing round trip
// there would make the command fail exactly when it is needed.
func TestPlainStatusNeverContactsTheAccountEndpoint(t *testing.T) {
	d := newAccountDeployment(t, true)
	mgr, cfg := signedInAgainst(t, d)

	if _, exit := runStatus(t, mgr, cfg, false); exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if n := d.accountCalls.Load(); n != 0 {
		t.Errorf("account requests = %d on a plain status read, want 0", n)
	}
}

// A deployment that says it has no accounts is not asked about one, and no credential
// work is attempted either — touching the store there reports a keychain problem on a
// deployment whose answer is "nothing to do".
func TestRefreshSkipsEverythingWhenAccountsAreNotConfigured(t *testing.T) {
	d := newAccountDeployment(t, false)
	mgr, cfg := signedInAgainst(t, d)

	got, exit := runStatus(t, mgr, cfg, true)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if n := d.accountCalls.Load(); n != 0 {
		t.Errorf("account requests = %d against a deployment with no accounts, want 0", n)
	}
	if n := d.tokenCalls.Load(); n != 0 {
		t.Errorf("token requests = %d against a deployment with no accounts, want 0", n)
	}
	if !strings.Contains(got, "not offered by this backend") {
		t.Errorf("did not report the deployment shape:\n%s", got)
	}
	for _, banned := range []string{"Could not verify", "Could not check", "auth login"} {
		if strings.Contains(got, banned) {
			t.Errorf("rendered %q, which is the wrong reading here:\n%s", banned, got)
		}
	}
}

// The three account outcomes that are NOT a login problem, each with its own next step.
func TestRefreshRendersEachPlanOutcomeWithItsOwnRemedy(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantHuman   []string
		bannedHuman []string
	}{
		{
			name: "no plan sends the user to choose one",
			body: `{"version":1,"email":"a@b.test","subject_hash":"` + accountfixture.SubjectHash + `",` +
				`"access":"subscription_required","subscription_status":"none",` +
				`"entitlement_source":"polar","entitlement_stale":false,` +
				`"checked_at":"2026-08-25T12:00:00Z"}`,
			wantHuman: []string{"no plan yet", "Choose a plan", "subscribe"},
			// Not a sign-in problem, and not a billing-portal problem.
			bannedHuman: []string{"auth login", "Check billing"},
		},
		{
			name: "a lapsed plan sends the user to billing, never a second checkout",
			body: `{"version":1,"email":"a@b.test","subject_hash":"` + accountfixture.SubjectHash + `",` +
				`"access":"subscription_inactive","plan_id":"pro",` +
				`"subscription_status":"past_due","entitlement_source":"polar",` +
				`"entitlement_stale":false,"checked_at":"2026-08-25T12:00:00Z"}`,
			wantHuman: []string{"plan inactive", "billing", "/account"},
			// The single most expensive wrong line available here.
			bannedHuman: []string{"Choose a plan", "/subscribe", "auth login"},
		},
		{
			name:        "an unverified rollout says so rather than inventing a verdict",
			body:        `{"version":1,"email":"a@b.test","subject_hash":"` + accountfixture.SubjectHash + `","access":"unverified"}`,
			wantHuman:   []string{"plan not checked", "--refresh"},
			bannedHuman: []string{"auth login", "Choose a plan"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newAccountDeployment(t, true)
			body := tc.body
			d.reply = func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}
			mgr, cfg := signedInAgainst(t, d)

			got, exit := runStatus(t, mgr, cfg, true)
			// A plan problem is not a "not signed in" exit. Exit 3 is reserved for
			// NeedsLogin, and a script branching on it must not try to log in here.
			if exit != 0 {
				t.Errorf("exit = %d, want 0 — a plan problem is not a login problem", exit)
			}
			for _, want := range tc.wantHuman {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, banned := range tc.bannedHuman {
				if strings.Contains(got, banned) {
					t.Errorf("contains %q, which is the wrong remedy here:\n%s", banned, got)
				}
			}
		})
	}
}

// A billing outage keeps the credential and never reads as "not subscribed".
func TestABillingOutageIsNotReportedAsUnsubscribed(t *testing.T) {
	d := newAccountDeployment(t, true)
	d.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"entitlement_unavailable","message":"polar down"}}`))
	}
	mgr, cfg := signedInAgainst(t, d)

	got, exit := runStatus(t, mgr, cfg, true)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	// ONE request. A 503 is retriable under the default policy, so an account client
	// built with it would grind through the whole backoff schedule — making a person
	// wait out ten round trips to be told the billing service is down, which the first
	// answer already said.
	if n := d.accountCalls.Load(); n != 1 {
		t.Errorf("account requests = %d on a dependency outage, want exactly 1", n)
	}
	if !strings.Contains(got, "could not check") {
		t.Errorf("did not report an outage as an outage:\n%s", got)
	}
	for _, banned := range []string{"no plan yet", "Choose a plan", "auth login", "signed out"} {
		if strings.Contains(got, banned) {
			t.Errorf("an outage rendered as %q:\n%s", banned, got)
		}
	}
	if !mgr.Status().State.SignedIn() {
		t.Error("an outage signed the user out")
	}
}

// A backend that refuses this client keeps the credential and must not offer a login —
// a fresh one would be refused identically, which is a loop with no exit.
func TestARefusedClientDoesNotOfferAnotherLogin(t *testing.T) {
	d := newAccountDeployment(t, true)
	d.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","code":"auth_client_not_allowed","message":"no"}}`))
	}
	mgr, cfg := signedInAgainst(t, d)

	got, exit := runStatus(t, mgr, cfg, true)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.Contains(got, "refuses these credentials") {
		t.Errorf("did not name the refusal:\n%s", got)
	}
	if strings.Contains(got, "Run `daintree-assistant auth login`") {
		t.Errorf("offered a login that would be refused identically:\n%s", got)
	}
	if n := d.accountCalls.Load(); n != 1 {
		t.Errorf("account requests = %d — a settled refusal was retried", n)
	}
}

// A malformed answer leaves the session exactly as it was and says only that it could
// not be checked.
func TestAMalformedAccountAnswerChangesNothing(t *testing.T) {
	d := newAccountDeployment(t, true)
	d.reply = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":7,"access":"granted"}`))
	}
	mgr, cfg := signedInAgainst(t, d)

	got, exit := runStatus(t, mgr, cfg, true)
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	if n := d.accountCalls.Load(); n != 1 {
		t.Errorf("account requests = %d on a malformed body, want exactly 1 — it would replay identically", n)
	}
	if !strings.Contains(got, "Could not check the plan") {
		t.Errorf("did not report the contract failure:\n%s", got)
	}
	if strings.Contains(got, "plan         ") {
		t.Errorf("a malformed body produced a plan:\n%s", got)
	}
	if !mgr.Status().State.SignedIn() {
		t.Error("a malformed body signed the user out")
	}
}

// A stale cached entitlement is flagged as such. "You are subscribed" and "you were
// subscribed when we last managed to ask" are different claims.
func TestAStaleEntitlementSaysSoInTheStatusBlock(t *testing.T) {
	checked := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	st := auth.Status{
		State: auth.StateSignedInActive, Authenticated: true, StorageTier: auth.TierKeychain,
		Plan: "pro", EntitlementSource: "cache", EntitlementStale: true,
		EntitlementCheckedAt: &checked,
	}.WithAvailability(auth.Availability{Known: true, Configured: true})

	var out bytes.Buffer
	renderAuthStatus(authWriter{out: &out, err: &out}, st, config.AppConfig{})
	got := out.String()
	for _, want := range []string{"pro", "cache", "may be out of date", "plan checked"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A memory-only credential is warned about even once a plan check has succeeded. The
// state now follows the account verdict, so a warning keyed off the state string would
// vanish on exactly the run where the user is most likely reading this block.
func TestAMemoryOnlyCredentialIsWarnedAboutEvenWhenThePlanIsActive(t *testing.T) {
	st := auth.Status{
		State: auth.StateSignedInActive, Authenticated: true,
		StorageTier: auth.TierMemory, Plan: "standard",
	}.WithAvailability(auth.Availability{Known: true, Configured: true})

	var out bytes.Buffer
	renderAuthStatus(authWriter{out: &out, err: &out}, st, config.AppConfig{})
	if !strings.Contains(out.String(), "disappears when this process exits") {
		t.Errorf("the non-persistence warning was lost:\n%s", out.String())
	}
}

// The JSON contract stays one line, versioned, camel-case, and free of anything
// credential-shaped — with the account fields now actually populated.
func TestTheStatusEventCarriesThePopulatedAccountFields(t *testing.T) {
	d := newAccountDeployment(t, true)
	mgr, cfg := signedInAgainst(t, d)

	var out, errBuf bytes.Buffer
	exit := runAuthStatus(context.Background(), authWriter{json: true, out: &out, err: &errBuf}, mgr, cfg,
		AuthOptions{Action: AuthStatus, Refresh: true})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly 1:\n%s", len(lines), out.String())
	}
	var ev struct {
		V    int `json:"v"`
		Type string
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("unparseable event: %v\n%s", err, lines[0])
	}
	if ev.V != 1 || ev.Type != "auth:status" {
		t.Errorf("event = v%d %q", ev.V, ev.Type)
	}
	// The existing camel-case names, populated rather than renamed.
	for key, want := range map[string]any{
		"email":             accountfixture.Email,
		"subjectHash":       accountfixture.SubjectHash,
		"planId":            "standard",
		"entitlementSource": "polar",
	} {
		if got := ev.Data[key]; got != want {
			t.Errorf("data[%q] = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{"lastVerifiedAt", "entitlementCheckedAt", "configured", "authRequired"} {
		if _, ok := ev.Data[key]; !ok {
			t.Errorf("data is missing %q", key)
		}
	}
	// Nothing credential-shaped, ever. The seed refresh token and every minted access
	// token are checked by name.
	for _, secret := range []string{"refresh-seed", "access-1", "refresh-1", "Bearer"} {
		if strings.Contains(lines[0], secret) {
			t.Fatalf("the event carried %q:\n%s", secret, lines[0])
		}
	}
}

// The state string domain is OPEN. A consumer that switched without a default would
// break on the next state added, so the coarse booleans have to stay usable on their own.
func TestTheJSONStateDomainStaysOpen(t *testing.T) {
	var out bytes.Buffer
	w := authWriter{json: true, out: &out, err: &bytes.Buffer{}}
	w.event(authEvent{Type: "auth:status", Extra: auth.Status{
		State: auth.State("a_state_no_consumer_has_heard_of"), Authenticated: true,
		StorageTier: auth.TierKeychain,
	}})
	var ev struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &ev); err != nil {
		t.Fatalf("an unknown state broke the line: %v", err)
	}
	if ev.Data["state"] != "a_state_no_consumer_has_heard_of" {
		t.Errorf("state = %v", ev.Data["state"])
	}
	if ev.Data["authenticated"] != true {
		t.Error("the coarse boolean did not survive an unknown state")
	}
}

// runLoginCheck drives the post-login plan check the way runAuthLogin does.
func runLoginCheck(t *testing.T, mgr *auth.Manager, cfg config.AppConfig, jsonMode bool) (stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	man, err := mgr.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	reportPlanAfterLogin(context.Background(), authWriter{json: jsonMode, out: &out, err: &errBuf}, mgr, cfg, man)
	if !jsonMode {
		return out.String(), out.String()
	}
	return out.String(), errBuf.String()
}

// THE post-login rule: a courtesy plan check must never be able to destroy the login it
// just followed.
//
// The observing account client acts on what it hears, and `auth_session_revoked` reaches
// RemedyClear — which deletes the stored refresh token. A backend mid-deploy, a proxy
// rewriting a body, or a misconfigured deployment can produce that answer seconds after a
// token exchange the provider itself completed. The user would be told "Signed in.", the
// command would exit 0, and the credential would be gone.
func TestThePostLoginPlanCheckCannotDestroyTheLogin(t *testing.T) {
	for _, code := range []string{"auth_session_revoked", "auth_token_invalid", "auth_required"} {
		t.Run(code, func(t *testing.T) {
			d := newAccountDeployment(t, true)
			status := http.StatusUnauthorized
			body := `{"error":{"type":"authentication_error","code":"` + code + `","message":"no"}}`
			d.reply = func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}
			mgr, cfg := signedInAgainst(t, d)
			if !mgr.Hydrate(context.Background()) {
				t.Fatal("the seeded session did not hydrate")
			}

			got, _ := runLoginCheck(t, mgr, cfg, false)

			// The session survives, in the state machine AND on disk.
			if st := mgr.State(); !st.SignedIn() {
				t.Errorf("state = %q — the plan check ended the session it followed", st)
			}
			if !strings.Contains(got, "Signed in") {
				t.Errorf("did not report the login as successful:\n%s", got)
			}
			if strings.Contains(got, "failed") {
				t.Errorf("reported the login as failed over a plan check:\n%s", got)
			}
		})
	}
}

// The other half: a successful check still lands, so the plan is named.
func TestThePostLoginPlanCheckNamesAnActivePlan(t *testing.T) {
	d := newAccountDeployment(t, true)
	mgr, cfg := signedInAgainst(t, d)
	mgr.Hydrate(context.Background())

	got, _ := runLoginCheck(t, mgr, cfg, false)
	if !strings.Contains(got, "standard plan is active") {
		t.Errorf("did not name the plan:\n%s", got)
	}
	if n := d.accountCalls.Load(); n != 1 {
		t.Errorf("account requests = %d, want exactly 1", n)
	}
}

// Login's plan outcomes: each keeps the credential, and a lapsed plan is never offered a
// second checkout.
func TestLoginReportsEachPlanOutcomeWithoutFailing(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantHuman   []string
		bannedHuman []string
	}{
		{
			"no plan offers the subscribe link",
			`{"version":1,"subject_hash":"` + accountfixture.SubjectHash + `","access":"subscription_required","entitlement_source":"polar","entitlement_stale":false,"checked_at":"2026-08-25T12:00:00Z"}`,
			[]string{"does not have a plan", "/subscribe"},
			[]string{"/account", "failed"},
		},
		{
			"a lapsed plan offers billing, never a checkout",
			`{"version":1,"subject_hash":"` + accountfixture.SubjectHash + `","access":"subscription_inactive","plan_id":"pro","entitlement_source":"polar","entitlement_stale":false,"checked_at":"2026-08-25T12:00:00Z"}`,
			[]string{"not currently active", "Manage billing", "/account"},
			[]string{"/subscribe", "failed"},
		},
		{
			"an unverified rollout says the plan was not reported",
			`{"version":1,"subject_hash":"` + accountfixture.SubjectHash + `","access":"unverified"}`,
			[]string{"did not report a plan"},
			[]string{"failed", "/subscribe"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newAccountDeployment(t, true)
			body := tc.body
			d.reply = func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}
			mgr, cfg := signedInAgainst(t, d)
			mgr.Hydrate(context.Background())

			got, _ := runLoginCheck(t, mgr, cfg, false)
			for _, want := range tc.wantHuman {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, banned := range tc.bannedHuman {
				if strings.Contains(got, banned) {
					t.Errorf("contains %q, which is the wrong remedy here:\n%s", banned, got)
				}
			}
			if !mgr.State().SignedIn() {
				t.Error("a plan outcome ended the session")
			}
		})
	}
}

// Under --json every post-login outcome puts a status line on stdout, including the
// failures. Daintree reads this stream as its only view of account state, and human
// prose goes to stderr where it never sees it.
func TestLoginEmitsAStatusEventOnEveryPlanOutcome(t *testing.T) {
	cases := []struct {
		name  string
		reply func(w http.ResponseWriter)
	}{
		{"granted", nil},
		{"outage", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"entitlement_unavailable","message":"down"}}`))
		}},
		{"refused", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","code":"auth_client_not_allowed","message":"no"}}`))
		}},
		{"malformed", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":4}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newAccountDeployment(t, true)
			d.reply = tc.reply
			mgr, cfg := signedInAgainst(t, d)
			mgr.Hydrate(context.Background())

			stdout, stderr := runLoginCheck(t, mgr, cfg, true)
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			if len(lines) != 1 {
				t.Fatalf("stdout carried %d lines, want exactly 1:\n%s", len(lines), stdout)
			}
			var ev struct {
				V    int `json:"v"`
				Type string
			}
			if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
				t.Fatalf("unparseable event: %v\n%s", err, lines[0])
			}
			if ev.V != 1 || ev.Type != "auth:status" {
				t.Errorf("event = v%d %q, want v1 auth:status", ev.V, ev.Type)
			}
			// Human prose never contaminates the machine channel.
			if tc.reply != nil && stderr == "" {
				t.Error("a failed plan check said nothing to the human at all")
			}
		})
	}
}

// A backend URL carrying userinfo must not reach any status output, in either mode.
//
// Defence in depth: backend.NormalizeBaseURL now refuses userinfo at every endpoint
// source, so a configured `https://user:secret@host` fails the launch instead of
// arriving here. This constructs the Manager directly, bypassing that check, because
// what is being pinned is the status rendering itself — the guarantee has to hold on
// the value the Manager was handed, not only on the one path that vets it.
func TestAStatusNeverCarriesCredentialsFromTheBackendURL(t *testing.T) {
	mgr, err := auth.NewManager(auth.Options{
		StateRoot:  t.TempDir(),
		BackendURL: "https://someone:hunter2@backend.example.test",
		Store:      auth.NewMemoryStore(),
		Opener:     auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	st := mgr.Status()
	if strings.Contains(st.BackendURL, "hunter2") {
		t.Fatalf("Status carried the password: %q", st.BackendURL)
	}

	var out bytes.Buffer
	w := authWriter{json: true, out: &out, err: &bytes.Buffer{}}
	w.event(authEvent{Type: "auth:status", Extra: st})
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("the event carried the password:\n%s", out.String())
	}

	var human bytes.Buffer
	renderAuthStatus(authWriter{out: &human, err: &human}, st, config.AppConfig{})
	if strings.Contains(human.String(), "hunter2") {
		t.Fatalf("the human block carried the password:\n%s", human.String())
	}
}
