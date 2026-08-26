package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/host"
)

// hostsecrets_test.go drives a COMPLETE sign-in through the embedded host's real command
// adapter and inspects everything it hands the host to emit.
//
// It exists because the guarantee it checks is a whole-path property and every previous
// test of it was a part-path one. `internal/auth` proves the authorization URL is not
// passed to progress; `internal/cli` proves the CLI's own status event carries no
// credential. Neither can see what an embedded HOST emits — and the host has more than
// one channel out. A command's result text is the obvious one; the progress notices a
// slow command posts while it runs are the one that was added most recently, by the same
// change that made `/login` reachable from a panel at all.
//
// So this asserts on BOTH of the adapter's outputs — the returned text and every progress
// stage — because those are the two channels that carry a command's CONTENT.
//
// It stops one layer short of the encoder, and the limit is worth stating precisely
// rather than waving at. The host adds its own fields around this content on the way out
// (internal/host/loop.go: the original command line and the outcome booleans on
// `command:result`, a separate MCP-status frame after it; internal/host/host.go: the
// session id, type and sequence). None of those is derived from the sign-in, so nothing
// there can carry one of these secrets today — but "text and progress are the only frame
// content" would be false, and a future event synthesised from something else is outside
// what this covers. The transport itself cannot be driven from this package because a
// host shutdown calls os.Exit.

// The canaries. Distinctive enough that a substring match cannot fire by accident, and
// each one stands for a different secret the flow handles.
const (
	canaryAccess  = "CANARY-ACCESS-TOKEN-8f14e45f"
	canaryRefresh = "CANARY-REFRESH-TOKEN-c9f0f895"
	canaryCode    = "CANARY-AUTH-CODE-45c48cce"
)

// secretIDP is an identity provider that hands out canary credentials and records the
// authorization request it was sent.
type secretIDP struct {
	srv *httptest.Server
	mu  sync.Mutex
	// authURL is the full authorization URL the browser was asked to open, captured so
	// the assertion can look for the REAL string rather than a guess at its shape.
	authURL  string
	verifier string
}

func newSecretIDP(t *testing.T) *secretIDP {
	t.Helper()
	p := &secretIDP{}
	mux := http.NewServeMux()

	mux.HandleFunc(auth.DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "environment": "development", "configured": true, "required": false,
			"issuer":                 p.srv.URL + "/auth/v1",
			"authorization_endpoint": p.srv.URL + "/auth/v1/oauth/authorize",
			"token_endpoint":         p.srv.URL + "/auth/v1/oauth/token",
			"jwks_uri":               p.srv.URL + "/auth/v1/.well-known/jwks.json",
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
		p.mu.Lock()
		p.verifier = r.Form.Get("code_verifier")
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"access_token":%q,"refresh_token":%q,"token_type":"bearer","expires_in":3600}`,
			canaryAccess, canaryRefresh)
	})
	mux.HandleFunc(backend.AccountStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"version":1,"email":"person@example.com","subject_hash":"b4c864ea44cbb4a1",`+
			`"access":"granted","plan_id":"pro","subscription_status":"active",`+
			`"entitlement_source":"polar","entitlement_stale":false,"checked_at":"2026-08-25T12:00:00Z"}`)
	})
	// Everything else 404s rather than being quietly served: a request this test did not
	// anticipate is a path it is not actually covering.
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// browser completes the callback, and records the URL it was asked to open.
func (p *secretIDP) browser(ctx context.Context, authURL string) error {
	p.mu.Lock()
	p.authURL = authURL
	p.mu.Unlock()
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	state := u.Query().Get("state")
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			req, rErr := http.NewRequestWithContext(ctx, http.MethodGet,
				auth.RedirectURI()+"?code="+canaryCode+"&state="+url.QueryEscape(state), nil)
			if rErr != nil {
				return
			}
			resp, cErr := http.DefaultClient.Do(req)
			if cErr == nil {
				_ = resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return nil
}

func (p *secretIDP) captured() (authURL, verifier string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authURL, p.verifier
}

// openerFn adapts a function to auth.Opener.
type openerFn func(context.Context, string) error

func (f openerFn) Open(ctx context.Context, u string) error { return f(ctx, u) }

// loginWithPortRetry drives a real Login, retrying while the fixed callback port is held.
//
// The callback address is compiled in — the identity provider matches redirect URIs
// exactly, so it cannot be varied per test — and `go test ./...` runs PACKAGES
// concurrently. Three packages now drive real sign-ins, so without this they contend for
// 127.0.0.1:42813 and whichever loses fails with a port collision that has nothing to do
// with what it was testing.
//
// Only the port collision is retried. Every other failure is the thing under test.
func loginWithPortRetry(t *testing.T, m *auth.Manager) error {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := m.Login(context.Background(), true, nil, nil)
		if auth.CodeOf(err) != auth.CodeCallbackPortInUse || time.Now().After(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A complete embedded sign-in puts no credential into either channel the host can emit.
//
// The two channels are exhaustive rather than representative, and that is what makes this
// boundary the right one: the host builds a slow command's `notice` frames from the
// PROGRESS stages the adapter reports, and its `command:result` frame from the TEXT the
// adapter returns. It has no third source of content for a command — so a secret cannot
// reach a frame without first passing through one of the two things captured here.
func TestNoHostChannelCarriesACredentialDuringSignIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", dir)
	p := newSecretIDP(t)

	mgr, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: p.srv.URL, Store: auth.NewMemoryStore(),
		Opener: openerFn(p.browser),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	overrides, err := overridesFromOptions(Options{
		Offline: boolPtr(true), Project: t.TempDir(), BackendURL: p.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides, AuthManager: mgr})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	adapter := &hostAppAdapter{app: a}

	// EVERYTHING the adapter says, both channels, for a real sign-in and the account
	// read that follows it.
	var mu sync.Mutex
	var emitted []string
	record := func(stage string) {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, "notice: "+stage)
	}
	for _, line := range []string{"/login", "/account"} {
		// The same fixed-port contention loginWithPortRetry handles, one layer up: the
		// command owns the sign-in, so the retry has to be around the command.
		var out host.CommandOutcome
		deadline := time.Now().Add(30 * time.Second)
		for {
			out = adapter.RunCommandWithProgress(context.Background(), line, record)
			if !strings.Contains(out.Text, "already in use") || time.Now().After(deadline) {
				break
			}
			mu.Lock()
			emitted = nil
			mu.Unlock()
			time.Sleep(25 * time.Millisecond)
		}
		if out.Unknown {
			t.Fatalf("%s came back unknown", line)
		}
		mu.Lock()
		emitted = append(emitted, "result: "+out.Text)
		mu.Unlock()
	}

	authURL, verifier := p.captured()
	if authURL == "" {
		t.Fatal("the browser was never asked to open an authorization URL, so no sign-in happened")
	}

	// The RECORDED values, not guesses at their shape. A test that searched for
	// "code_challenge" would pass against a surface that emitted the URL with the
	// parameter renamed, and would say nothing about the verifier at all.
	// UNCONDITIONAL. Guarding the verifier assertion on it being non-empty made the
	// strongest canary opt out of itself: if the exchange ever stopped sending
	// code_verifier, this fake provider would happily accept the request anyway and the
	// one secret most worth checking would silently drop out of the banned set.
	if verifier == "" {
		t.Fatal("the token exchange sent no code_verifier, so PKCE is not being exercised " +
			"and the verifier canary would have been silently skipped")
	}
	banned := map[string]string{
		"the authorization URL":  authURL,
		"the access token":       canaryAccess,
		"the refresh token":      canaryRefresh,
		"the authorization code": canaryCode,
		"the PKCE code verifier": verifier,
	}

	mu.Lock()
	defer mu.Unlock()
	for _, line := range emitted {
		for what, secret := range banned {
			if strings.Contains(line, secret) {
				t.Errorf("an embedded surface carried %s:\n%s", what, line)
			}
		}
	}

	// The flow has to have actually PRODUCED something, or the absence above is the
	// absence of output rather than a guarantee about it. A successful sign-in against
	// this deployment reports the pro plan.
	joined := strings.Join(emitted, "\n")
	if !strings.Contains(joined, "pro") {
		t.Fatalf("the sign-in did not reach a signed-in account card, so nothing meaningful was inspected:\n%s", joined)
	}
	if len(emitted) < 3 {
		t.Fatalf("only %d outputs were captured; the progress channel was probably never used:\n%s", len(emitted), joined)
	}
}

// A sign-in in ANOTHER process reaches this one, without a restart.
//
// This is the mechanism the shared revision marker exists for, and the sequence the brief
// names first: an embedded session starts signed out, a separate `auth login` stores a
// credential and bumps the revision, and the session's next protected operation picks it
// up. Both processes are modelled by two managers over one state root, which is exactly
// what they are — the credential store and the revision file are per USER, at the state
// root, so one login covers every session sharing it.
func TestASignInInAnotherProcessReachesAnAlreadyRunningSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", dir)
	p := newSecretIDP(t)

	// Shared, because a real second process reads the SAME credential store — the store
	// and the revision file are per user, at the state root. A separate memory store
	// would make the two "processes" independent and the test vacuous.
	//
	// The fake's one inaccuracy, stated: it reports TierMemory, so both sessions are
	// non-persistent in a way a real keychain-backed pair would not be. That affects the
	// STORAGE TIER a status renders and nothing this test asserts — the revision marker,
	// the credential's presence and the account read behave identically on either tier.
	store := auth.NewMemoryStore()

	// The long-lived session. It starts knowing nothing.
	session, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: p.srv.URL, Store: store, Opener: auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	overrides, err := overridesFromOptions(Options{
		Offline: boolPtr(true), Project: t.TempDir(), BackendURL: p.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides, AuthManager: session})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	adapter := &hostAppAdapter{app: a}

	before := adapter.RunCommand(context.Background(), "/account")
	if strings.Contains(before.Text, "pro") {
		t.Fatalf("the session is already signed in, so the transition proves nothing:\n%s", before.Text)
	}

	// The OTHER process signs in.
	other, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: p.srv.URL, Store: store, Opener: openerFn(p.browser),
	})
	if err != nil {
		t.Fatalf("NewManager (other process): %v", err)
	}
	if err := loginWithPortRetry(t, other); err != nil {
		t.Fatalf("the other process could not sign in: %v", err)
	}

	// The running session's next protected operation finds it.
	after := adapter.RunCommand(context.Background(), "/account")
	if !strings.Contains(after.Text, "pro") {
		t.Errorf("a sign-in in another process never reached this one:\n%s", after.Text)
	}
	if !strings.Contains(after.Text, "person@example.com") {
		t.Errorf("the account is missing after another process signed in:\n%s", after.Text)
	}
}

// A sign-out in ANOTHER process stops this one spending.
//
// The counterpart, and the one that matters for unattended work: a logout elsewhere bumps
// the shared revision, and a session that went on presenting its old access token would
// keep spending against an account the user believes they have signed out of.
func TestASignOutInAnotherProcessStopsThisOneSpending(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", dir)
	p := newSecretIDP(t)
	store := auth.NewMemoryStore()

	session, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: p.srv.URL, Store: store, Opener: openerFn(p.browser),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := loginWithPortRetry(t, session); err != nil {
		t.Fatalf("Login: %v", err)
	}
	overrides, err := overridesFromOptions(Options{
		Offline: boolPtr(true), Project: t.TempDir(), BackendURL: p.srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides, AuthManager: session})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	adapter := &hostAppAdapter{app: a}

	if out := adapter.RunCommand(context.Background(), "/account"); !strings.Contains(out.Text, "pro") {
		t.Fatalf("the session did not start signed in, so the sign-out proves nothing:\n%s", out.Text)
	}

	// The OTHER process signs out.
	other, err := auth.NewManager(auth.Options{
		StateRoot: dir, BackendURL: p.srv.URL, Store: store, Opener: auth.NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager (other process): %v", err)
	}
	other.Hydrate(context.Background())
	if _, err := other.Logout(context.Background()); err != nil {
		t.Fatalf("the other process could not sign out: %v", err)
	}

	// The credential is gone from the shared store, and this session must not be able to
	// produce one — that is what stops paid work rather than any UI state.
	//
	// EMPTY, not an error. "No credential" is not a fault here: it means "send no
	// Authorization header", which is what an install with no account does all day, and
	// the backend is the authority on whether that is allowed. An error would abort the
	// request locally instead of letting the backend answer.
	tok, err := session.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "" {
		t.Error("the running session still produced a credential after another process signed out")
	}
	if st := session.Status(); st.State.CanSpend() {
		t.Errorf("the running session still reports that it may spend: %q", st.State)
	}
	out := adapter.RunCommand(context.Background(), "/account")
	if strings.Contains(out.Text, "pro") {
		t.Errorf("the card still names a plan after another process signed out:\n%s", out.Text)
	}
}
