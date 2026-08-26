package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/backend/accountfixture"
	"github.com/daintreehq/assistant/internal/config"
)

// accountlive_test.go covers the half of /login and /account that is about the BACKEND
// rather than about the command surface.
//
// The gap it exists to close: both commands used to render whatever the manager happened
// to hold in this process, and a manager that has only hydrated a stored credential holds
// almost nothing — it knows a refresh token exists and deliberately makes no request. So
// /account on a perfectly good keychain sign-in said "signed in (not yet verified against
// the backend)" and named no plan, while `auth status --refresh` against the SAME
// credential named it. The parity test between the two command surfaces could not see
// this, because both surfaces shared the omission.

// liveDeployment is a fake backend serving discovery, the token endpoint and the account
// route. The account reply is swappable so a test can act out a checkout.
type liveDeployment struct {
	srv          *httptest.Server
	accountCalls atomic.Int64
	body         atomic.Value // string
	// entered is closed-over signalling for the one test that needs the request to be
	// demonstrably IN FLIGHT before it acts. beforeReply parks the handler until that
	// test says the world has moved on underneath it.
	entered     chan struct{}
	beforeReply func()
	// status is the code served with body. Non-2xx is how a typed refusal — a revocation,
	// a billing outage — is acted out, since those are what the observing client folds
	// into local state.
	status int
}

func newLiveDeployment(t *testing.T) *liveDeployment {
	t.Helper()
	d := &liveDeployment{entered: make(chan struct{}, 1)}
	d.body.Store(accountfixture.String(accountfixture.GrantedStandard))

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/daintree/auth/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"access-1","refresh_token":"refresh-1","token_type":"bearer","expires_in":3600}`)
	})
	mux.HandleFunc(backend.AccountStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		d.accountCalls.Add(1)
		select {
		case d.entered <- struct{}{}:
		default:
		}
		if d.beforeReply != nil {
			d.beforeReply()
		}
		body, _ := d.body.Load().(string)
		if d.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(d.status)
			_, _ = fmt.Fprint(w, body)
			return
		}
		if body == "" {
			// The outage case: reached, and unable to answer. Deliberately a 503 rather
			// than a dropped connection, because "the billing service is down" is the
			// failure that must never be rendered as "you are not subscribed".
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"error":{"type":"api_error","code":"entitlement_unavailable","message":"billing is unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	})

	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *liveDeployment) serve(body string) { d.body.Store(body) }
func (d *liveDeployment) fail()             { d.body.Store("") }

// signedInApp builds an App holding a live session against d.
//
// The manager is INJECTED rather than left to Create, so the test can seed a credential
// into it first — and, more importantly, so the App and the test hold the same object.
// Create's own manager would be a second one, and a verdict landing on an object nothing
// else reads is the exact bug CreateOptions.AuthManager exists to prevent.
func signedInApp(t *testing.T, d *liveDeployment) *app.App {
	t.Helper()
	dir := t.TempDir()
	mgr, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: d.srv.URL, Store: auth.NewMemoryStore(), Opener: auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	man, err := mgr.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := mgr.SeedForTest(context.Background(), man, "refresh-seed"); err != nil {
		t.Fatalf("SeedForTest: %v", err)
	}

	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    strPtr(dir),
			ProjectPath: strPtr(dir),
			Tier:        strPtr("operator"),
			BackendURL:  strPtr(d.srv.URL),
		},
		AuthManager:     mgr,
		BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

// switchableApp is signedInApp for the tests that need `/backend` to work.
//
// The endpoint arrives as the STORED preference rather than as an override, because an
// override PINS the session — a harness or a CI run must never be silently redirected —
// and a pinned session refuses `/backend` outright. The stored preference is the
// switchable one, and it is what a real desktop session has.
func switchableApp(t *testing.T, d *liveDeployment) *app.App {
	t.Helper()
	dir := t.TempDir()
	if err := config.SaveBackendURL(config.EndpointPath(dir), d.srv.URL); err != nil {
		t.Fatalf("SaveBackendURL: %v", err)
	}
	mgr, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: d.srv.URL, Store: auth.NewMemoryStore(), Opener: auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	man, err := mgr.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := mgr.SeedForTest(context.Background(), man, "refresh-seed"); err != nil {
		t.Fatalf("SeedForTest: %v", err)
	}
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: strPtr(dir), ProjectPath: strPtr(dir),
			Tier: strPtr("operator"),
		},
		AuthManager: mgr, BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

// /account ASKS. Without the live read it renders a hydrated credential and nothing else:
// no account, no plan, and a state line saying the backend has not confirmed anything —
// while the identical credential through `auth status --refresh` names the plan.
func TestAccountAsksTheBackendRatherThanRenderingWhatThisProcessRemembers(t *testing.T) {
	d := newLiveDeployment(t)
	a := signedInApp(t, d)

	out := accountText(context.Background(), a)

	if n := d.accountCalls.Load(); n != 1 {
		t.Fatalf("account requests = %d, want exactly 1 — /account did not ask the backend", n)
	}
	if !strings.Contains(out, "standard") {
		t.Errorf("the plan is missing from the card:\n%s", out)
	}
	if !strings.Contains(out, "operator@example.com") {
		t.Errorf("the account is missing from the card:\n%s", out)
	}
	if strings.Contains(out, "not yet verified") {
		t.Errorf("a live read landed and the card still says the backend has not confirmed it:\n%s", out)
	}
}

// The returning customer. Someone who buys a plan on the website comes back to a process
// whose local state still says `subscription_required`; only a live read can move it, and
// requiring a second OAuth round trip to pick up a purchase they have already paid for is
// not a thing to ask of anyone.
func TestACheckoutBecomesActiveWithoutAnotherSignIn(t *testing.T) {
	d := newLiveDeployment(t)
	d.serve(accountfixture.String(accountfixture.SubscriptionRequired))
	a := signedInApp(t, d)

	before := accountText(context.Background(), a)
	if !strings.Contains(before, "no plan") {
		t.Fatalf("the priming state is not 'no plan', so the transition proves nothing:\n%s", before)
	}

	// The checkout happens elsewhere; all this session sees is a different answer.
	d.serve(accountfixture.String(accountfixture.GrantedPro))

	after := accountText(context.Background(), a)
	if !strings.Contains(after, "pro") {
		t.Errorf("the new plan did not reach the card:\n%s", after)
	}
	if strings.Contains(after, "no plan") || strings.Contains(after, "Choose a plan") {
		t.Errorf("the card still tells a paying customer to buy a plan:\n%s", after)
	}
	if n := d.accountCalls.Load(); n != 2 {
		t.Errorf("account requests = %d, want 2 — one per /account", n)
	}
}

// An outage is not a verdict. The single most expensive wrong sentence available on this
// card is "you are not subscribed" to somebody whose billing provider is merely down —
// they go and buy a second subscription.
func TestABillingOutageNeverReadsAsUnsubscribed(t *testing.T) {
	d := newLiveDeployment(t)
	a := signedInApp(t, d)
	// Prime with a real grant, so there is something for the outage to wrongly erase.
	_ = accountText(context.Background(), a)
	d.fail()

	out := accountText(context.Background(), a)

	for _, banned := range []string{"no plan", "Choose a plan", "signed out", "not subscribed"} {
		if strings.Contains(out, banned) {
			t.Errorf("an outage rendered as a billing verdict (%q):\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "could not be re-checked") {
		t.Errorf("the card does not say the check failed:\n%s", out)
	}
	// The credential is untouched: a failure to VERIFY is not a reason to sign anyone out.
	if st := a.AuthManager().Status(); !st.State.SignedIn() {
		t.Errorf("a billing outage ended the session: %q", st.State)
	}
	// And the plan that was already known is still shown. Blanking it would tell someone
	// their subscription had gone away because a dependency did.
	if !strings.Contains(out, "standard") {
		t.Errorf("the last known plan was erased by an outage:\n%s", out)
	}
}

// A deployment with no identity provider has nothing to check, and must not be reported
// as a failed check.
func TestADeploymentWithoutAccountsIsSilentRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/daintree/auth/config" {
			w.Header().Set("Content-Type", "application/json")
			// The four-field answer: a CORRECT response that fails manifest validation
			// by design.
			_, _ = fmt.Fprint(w, `{"version":1,"environment":"development","configured":false,"required":false}`)
			return
		}
		t.Errorf("a deployment with no accounts was asked for %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: strPtr(dir), ProjectPath: strPtr(dir),
			Tier: strPtr("operator"), BackendURL: strPtr(srv.URL),
		},
		BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	out := accountText(context.Background(), a)
	if strings.Contains(out, "could not be re-checked") {
		t.Errorf("a deployment with no accounts reported a failed check:\n%s", out)
	}
	if !strings.Contains(out, "does not use accounts") {
		t.Errorf("the card does not say this deployment has no accounts:\n%s", out)
	}
}

// A caller key and a deployment without accounts both leave the manager nil, and they are
// NOT the same sentence. Telling an operator who exported DAINTREE_API_KEY that "this
// backend does not use accounts" sends them looking for a deployment fault that is not
// there — the cause is a variable in their own environment.
func TestACallerKeySaysWhyThereIsNoAccountRatherThanBlamingTheBackend(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DAINTREE_API_KEY", "dk-test-caller-key-value")
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: strPtr(dir), ProjectPath: strPtr(dir),
			Tier: strPtr("operator"), BackendURL: strPtr(deadBackend),
		},
		BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	if a.AuthManager() != nil {
		t.Fatal("a caller key left an account manager in place — the premise of this test is gone")
	}

	for name, out := range map[string]string{
		"/account": accountText(context.Background(), a),
		"/login":   loginText(context.Background(), a, nil),
		"/logout":  logoutText(context.Background(), a),
	} {
		if !strings.Contains(out, "DAINTREE_API_KEY") {
			t.Errorf("%s does not name the cause:\n%s", name, out)
		}
		if strings.Contains(out, "does not use accounts") {
			t.Errorf("%s blames the backend for a local environment variable:\n%s", name, out)
		}
		// The key itself is the one thing that must never appear.
		if strings.Contains(out, "dk-test-caller-key-value") {
			t.Errorf("%s echoed the caller key:\n%s", name, out)
		}
	}
}

// A failed sign-in has to say what to DO. The hint is the actionable half and it was
// being dropped: auth errors carry it out of band (Error() deliberately omits it), the
// standalone CLI renders it, and this card did not — so a browser that would not open
// produced a failure with no way out of it.
func TestASignInFailureCarriesItsRemedy(t *testing.T) {
	d := newLiveDeployment(t)
	dir := t.TempDir()
	mgr, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: d.srv.URL, Store: auth.NewMemoryStore(),
		// An opener that always fails, which is what a headless box, a broken xdg-open
		// or a locked-down desktop all look like from here.
		Opener: failingOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: strPtr(dir), ProjectPath: strPtr(dir),
			Tier: strPtr("operator"), BackendURL: strPtr(d.srv.URL),
		},
		AuthManager: mgr, BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	out := loginText(context.Background(), a, func(string) {})

	if !strings.Contains(out, "daintree-assistant auth login --no-open") {
		t.Errorf("the failure names no runnable remedy:\n%s", out)
	}
	// "Re-run with --no-open" is what this used to say, and it names neither a command
	// nor a place to type one when the reader is inside an embedded panel.
	if strings.Contains(out, "Re-run with") {
		t.Errorf("the remedy is still the ambiguous re-run phrasing:\n%s", out)
	}
	// The authorization URL is never a remedy. It carries the state and the challenge,
	// and this string reaches host events, logs and support bundles.
	if strings.Contains(out, "/oauth/authorize") || strings.Contains(out, "code_challenge") {
		t.Errorf("the sign-in URL reached the card:\n%s", out)
	}
}

// failingOpener stands in for every way a browser fails to launch.
type failingOpener struct{}

func (failingOpener) Open(context.Context, string) error {
	return fmt.Errorf("no browser here")
}

// A switch replaces the manager, and the two managers' account state stays separate.
//
// This is the PREMISE the discard guard rests on rather than the guard itself: a switch
// hands the session a different object, so the generation check inside ApplyAccountStatus
// — which compares numbers within ONE manager — can say nothing about a request that
// started against the other. Pinning it here means the guard's reason for existing cannot
// quietly stop being true. The guard itself is exercised by the test below.
func TestASwitchGivesTheSessionAManagerThatKnowsNothingOfTheOldEndpoint(t *testing.T) {
	d := newLiveDeployment(t)
	a := switchableApp(t, d)
	dir := t.TempDir()
	before := a.AuthManager()
	if before == nil {
		t.Fatal("the App has no account manager")
	}

	// The switch, exactly as `/backend` performs it: a new endpoint means a new
	// credential key, so the manager is rebuilt rather than carried over.
	if _, err := a.SetBackendURL(backend.LocalBaseURL); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	after := a.AuthManager()
	if after == before {
		t.Fatal("the switch did not replace the manager — the premise of this test is gone")
	}

	// An answer that started against the OLD endpoint, applied after the switch. This is
	// what a request in flight across the switch produces.
	res := app.RefreshAccountWith(context.Background(),
		config.AppConfig{StateRoot: dir, BackendURL: d.srv.URL}, before, app.AccountRefreshOptions{})
	if !res.Applied() {
		t.Fatalf("the priming read did not land: %+v", res)
	}

	// The new manager knows nothing about the old endpoint's account, and must not: a
	// plan granted by one deployment is not a plan granted by another.
	if st := after.Status(); st.Plan != "" || st.Email != "" {
		t.Errorf("the old endpoint's account reached the new endpoint's manager: %+v", st)
	}

}

// The App-level read REPORTS a crossing rather than believing the answer through it.
//
// The switch happens while the request is IN FLIGHT, which is the only ordering that
// matters: a check before the request cannot make the two atomic, so what is on offer is
// that an answer which arrived for a manager this session no longer holds is marked as
// discarded instead of being folded in as current.
func TestAReadThatCrossesAnEndpointSwitchIsReportedAsDiscarded(t *testing.T) {
	d := newLiveDeployment(t)
	a := switchableApp(t, d)

	// The account handler parks until the switch has happened, so the answer is
	// guaranteed to arrive on the far side of it.
	switched := make(chan struct{})
	// Released on EVERY exit, including a t.Fatalf below. Without this a failure while
	// the handler is parked leaves httptest.Server.Close waiting on it forever, and the
	// test that was going to tell us what broke hangs instead.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(switched) }) }
	t.Cleanup(release)
	d.beforeReply = func() { <-switched }

	done := make(chan app.AccountRefresh, 1)
	go func() { done <- a.RefreshAccount(context.Background(), app.AccountRefreshOptions{}) }()

	// Wait until the request is actually parked in the handler before switching, so this
	// is a crossing rather than a race the test might lose. Bounded, because an unbounded
	// receive on a request that never arrives is a hang with no message attached to it.
	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the account request never reached the backend")
	}
	if _, err := a.SetBackendURL(backend.LocalBaseURL); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	release()

	var res app.AccountRefresh
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the account read never returned")
	}
	if !res.Discarded {
		t.Fatalf("an answer for the endpoint this session has left was not discarded: %+v", res)
	}
	if res.Applied() {
		t.Error("a discarded answer reported itself as applied")
	}
	// The new endpoint's manager is untouched by the old endpoint's account.
	if st := a.AuthManager().Status(); st.Plan != "" || st.Email != "" {
		t.Errorf("the discarded answer still reached the current manager: %+v", st)
	}
}

// An ordinary read, with nothing moving underneath it, applies and says so.
//
// The counterpart to the discard tests: Applied() has to be true in the normal case, or
// the callers that branch on it would render every successful refresh as a failure.
func TestAnUndisturbedReadAppliesAndReportsItself(t *testing.T) {
	d := newLiveDeployment(t)
	a := signedInApp(t, d)

	res := a.RefreshAccount(context.Background(), app.AccountRefreshOptions{})
	if !res.Applied() {
		t.Fatalf("an ordinary read did not apply: %+v", res)
	}
	if res.Discarded || res.Skipped || res.Err != nil {
		t.Errorf("an ordinary read reported an exceptional outcome: %+v", res)
	}
}

// A revocation must not be footnoted with "your sign-in is unaffected".
//
// The observing client acts on `auth_session_revoked` by deleting the credential, so the
// card above the note already says "access disconnected" and offers /login. Deciding the
// note from the ERROR alone produced all three sentences at once, about one session:
// disconnected, sign in again, and nothing has changed.
func TestARevocationIsNotFootnotedAsHarmless(t *testing.T) {
	d := newLiveDeployment(t)
	a := signedInApp(t, d)
	// Prime, so there is a live session for the revocation to end.
	_ = accountText(context.Background(), a)

	d.status = http.StatusUnauthorized
	d.serve(`{"error":{"type":"invalid_request_error","code":"auth_session_revoked","message":"this session was revoked"}}`)

	out := accountText(context.Background(), a)

	if strings.Contains(out, "sign-in is unaffected") {
		t.Errorf("a revoked session was reported as unaffected:\n%s", out)
	}
	// The revocation itself still has to be visible — silently dropping it would be the
	// opposite failure.
	if !strings.Contains(out, "could not be re-checked") {
		t.Errorf("the failed check is not reported at all:\n%s", out)
	}
	if st := a.AuthManager().Status(); st.State.SignedIn() {
		t.Errorf("a revocation left the session signed in: %q", st.State)
	}
}

// An ERROR is as endpoint-specific as a status. One raised against the endpoint this
// session has since left describes a backend the reader is no longer talking to, and
// reporting it under the new endpoint's card blames the wrong one.
func TestAnErrorThatCrossesAnEndpointSwitchIsAlsoDiscarded(t *testing.T) {
	d := newLiveDeployment(t)
	a := switchableApp(t, d)

	switched := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(switched) }) }
	t.Cleanup(release)
	d.beforeReply = func() { <-switched }
	d.fail() // the old endpoint answers 503 once it is released

	done := make(chan app.AccountRefresh, 1)
	go func() { done <- a.RefreshAccount(context.Background(), app.AccountRefreshOptions{}) }()

	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the account request never reached the backend")
	}
	if _, err := a.SetBackendURL(backend.LocalBaseURL); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	release()

	var res app.AccountRefresh
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the account read never returned")
	}
	if !res.Discarded {
		t.Fatalf("an error from the endpoint this session has left was not discarded: %+v", res)
	}
	if res.Err != nil {
		t.Errorf("the old endpoint's error was carried out under the new endpoint: %v", res.Err)
	}
}
