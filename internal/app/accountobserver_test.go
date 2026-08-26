package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// accountobserver_test.go pins the split the courtesy account client rests on: it may
// settle a SETTLED answer about the account, and no verdict it forwards may touch the
// credential. (Obtaining the credential can still refresh, and a refresh whose grant is
// rejected ends the session — the manager's rule for every caller, and not something this
// filter claims to change.)
//
// The client that runs after a sign-in used to observe nothing whatsoever, and the
// guarantee was structural in the crudest way — the AccountObserver interface was simply
// absent, so every verdict was an inert no-op. That bought absolute safety and cost the
// truth: a deployment answering 403 for a valid identity it has not approved had its
// answer PRINTED by the login and then discarded, leaving the state machine on
// `signed_in_unverified` while the user read "this backend does not accept this
// account". Every later surface then disagreed with the sentence they had just seen.
//
// The guarantee is now an allowlist, so it has to be pinned rather than assumed.

// courtesyIDP is the smallest deployment a Manager can be built against: discovery only.
// Nothing here makes an account request — the filter is exercised by calling the observer
// directly, which is also the only way to feed it a verdict the transport would never
// produce.
func courtesyIDP(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != auth.DiscoveryPath {
			t.Errorf("the courtesy observer reached the network at %s — it must not", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "environment": "development", "configured": true, "required": false,
			"issuer":                 srv.URL + "/auth/v1",
			"authorization_endpoint": srv.URL + "/auth/v1/oauth/authorize",
			"token_endpoint":         srv.URL + "/auth/v1/oauth/token",
			"jwks_uri":               srv.URL + "/auth/v1/.well-known/jwks.json",
			"client_id":              "test-client",
			"redirect_uri":           auth.RedirectURI(),
			"scopes":                 []string{"openid", "email"},
			"account_url":            "https://staging.daintree.org/account",
			"subscribe_url":          "https://staging.daintree.org/subscribe",
			"session_policy":         map[string]any{"access_token_seconds": 3600, "session_max_age_seconds": 2592000},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signedInManager returns a hydrated manager holding a stored credential.
func signedInManager(t *testing.T, srv *httptest.Server) *auth.Manager {
	t.Helper()
	mgr, err := auth.NewManager(auth.Options{
		StateRoot: t.TempDir(), BackendURL: srv.URL,
		Store: auth.NewMemoryStore(), Opener: auth.NoOpener{},
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
	if !mgr.Hydrate(context.Background()) {
		t.Fatal("the seeded credential did not hydrate")
	}
	if got := mgr.State(); got != auth.StateSignedInUnverified {
		t.Fatalf("state = %q before any verdict, want %q", got, auth.StateSignedInUnverified)
	}
	return mgr
}

// The compile-time half. The client type-asserts for AccountObserver, so a courtesy
// source that stopped implementing it would silently go back to observing nothing — the
// exact regression, with every test below still passing for the wrong reason.
func TestTheCourtesySourceIsATokenSourceAndAnObserver(t *testing.T) {
	var _ backend.TokenSource = courtesyAccountTokenSource{}
	var _ backend.TokenScrubber = courtesyAccountTokenSource{}
	var _ backend.AccountObserver = courtesyAccountTokenSource{}
}

// Every allowlisted code is non-destructive BY REMEDY, not by inspection.
//
// This is the invariant the allowlist exists to satisfy, checked against the taxonomy
// rather than against a second copy of it: a code reclassified upstream — today's 403
// becoming tomorrow's "clear the credential" — fails here rather than deleting somebody's
// refresh token seconds after they signed in.
func TestNoCourtesySettleCodeCanTouchTheCredential(t *testing.T) {
	for code := range courtesySettleCodes {
		remedy := (&backend.Error{Code: code}).AuthRemedy()
		switch remedy {
		case backend.RemedyClear:
			t.Errorf("%s is allowlisted for a courtesy read and DELETES the credential", code)
		case backend.RemedySignIn, backend.RemedyRefresh, backend.RemedyRefreshOrSignIn:
			t.Errorf("%s is allowlisted for a courtesy read and acts on the credential (%s)", code, remedy)
		}
	}
	// The one that must never be added, named explicitly. A test that only iterates the
	// set passes trivially if somebody empties it.
	if courtesySettleCodes[backend.CodeAuthSessionRevoked] {
		t.Error("a revocation is allowlisted for a courtesy read — this is the deletion the whole type prevents")
	}
	if !courtesySettleCodes[backend.CodeAuthPermissionDenied] {
		t.Error("a staging allowlist's refusal does not settle on a courtesy read, which is the gap this exists to close")
	}
	// The SECOND gate is positive, and that is the property worth pinning rather than its
	// current membership. "Not RemedyClear" only blocks the one remedy somebody thought
	// of: a code reclassified upstream as RemedyRefresh would pass it, the manager would
	// drop the access token, and the client's ladder would then attempt a renewal — whose
	// rejected grant deletes the session. Only RemedyReconfigure has a purely
	// state-writing local effect, so only RemedyReconfigure may pass.
	for code := range courtesySettleCodes {
		if got := (&backend.Error{Code: code}).AuthRemedy(); got != backend.RemedyReconfigure {
			t.Errorf("%s is allowlisted with remedy %s — the gate below admits only %s",
				code, got, backend.RemedyReconfigure)
		}
	}
}

// The behavioural half, verdict by verdict, against a REAL manager.
//
// Each case is a code the backend can answer a courtesy account read with, and the state
// it must leave behind. The two 403s settle; everything else — the 402s, every credential
// verdict, every outage, an unknown code and an error that is not a backend error at all —
// leaves the session exactly as the login left it.
func TestTheCourtesyObserverSettlesOnlyTheSettledAccountVerdicts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want auth.State
	}{
		{
			// THE case this work exists for.
			"a staging allowlist's refusal settles",
			&backend.Error{Code: backend.CodeAuthPermissionDenied, Type: "authentication_error", Message: "not approved"},
			auth.StateAccessRefused,
		},
		{
			"a refused OAuth client settles the same way",
			&backend.Error{Code: backend.CodeAuthClientNotAllowed, Type: "authentication_error", Message: "no"},
			auth.StateAccessRefused,
		},
		{
			// The 402s are DROPPED, and the reasoning is worth keeping beside the
			// assertion: they are as non-destructive as the 403s and were briefly
			// allowlisted for exactly that symmetry. What sank it is that a 402 also
			// returns an error, and no surface has a settled-billing branch to render
			// the two halves together — the card would say "signed in — no plan" and
			// then "the account could not be re-checked just now" about the same read.
			"an unsubscribed account is dropped, because nothing renders a settled 402 coherently yet",
			&backend.Error{Code: backend.CodeSubscriptionRequired, Type: "invalid_request_error", Message: "no plan"},
			auth.StateSignedInUnverified,
		},
		{
			"a lapsed plan is dropped for the same reason",
			&backend.Error{Code: backend.CodeSubscriptionInactive, Type: "invalid_request_error", Message: "past due"},
			auth.StateSignedInUnverified,
		},
		{
			// The destructive one. A backend mid-deploy, a proxy rewriting a body or a
			// misconfigured deployment produce this as easily as a real revocation, and
			// acting on it here deletes the token the login persisted seconds ago.
			"a revocation is dropped",
			&backend.Error{Code: backend.CodeAuthSessionRevoked, Type: "authentication_error", Message: "revoked"},
			auth.StateSignedInUnverified,
		},
		{
			"a demand to sign in is dropped",
			&backend.Error{Code: backend.CodeAuthRequired, Type: "authentication_error", Message: "sign in"},
			auth.StateSignedInUnverified,
		},
		{
			"an expired token is dropped — a courtesy read does not put the session into a refresh",
			&backend.Error{Code: backend.CodeAuthTokenExpired, Type: "authentication_error", Message: "expired"},
			auth.StateSignedInUnverified,
		},
		{
			"an invalid token is dropped",
			&backend.Error{Code: backend.CodeAuthTokenInvalid, Type: "authentication_error", Message: "invalid"},
			auth.StateSignedInUnverified,
		},
		{
			// An outage is not an answer. Recording it would leave a fresh sign-in
			// looking degraded, carrying a code nothing retires until a later read lands.
			"a billing outage is dropped",
			&backend.Error{Code: backend.CodeEntitlementUnavailable, Type: "api_error", Message: "down"},
			auth.StateSignedInUnverified,
		},
		{
			"a code this build does not classify is dropped",
			&backend.Error{Code: "something_new", Type: "api_error", Message: "?"},
			auth.StateSignedInUnverified,
		},
		{
			"an error that is not a backend verdict at all is dropped",
			errors.New("dial tcp: connection refused"),
			auth.StateSignedInUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := courtesyIDP(t)
			mgr := signedInManager(t, srv)
			src := courtesyAccountTokenSource{mgr: mgr}

			src.ApplyBackendVerdict(context.Background(), mgr.Generation(), "", tc.err)

			if got := mgr.State(); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
			// The credential survives EVERY case, including the ones that settled. A
			// refusal and a lapsed plan are statements about the account; nothing here is
			// a reason to make somebody sign in again.
			if !mgr.State().SignedIn() {
				t.Fatalf("state %q is not a session at all — the courtesy read ended the login it followed", mgr.State())
			}
			// Hydrate alone is not proof — an authoritative ABSENCE also returns true,
			// after settling the state to signed-out. The pair is what says a credential
			// is still there.
			if !mgr.Hydrate(context.Background()) || !mgr.Status().Authenticated {
				t.Error("the stored credential was deleted by a courtesy read")
			}
		})
	}
}

// A verdict for a session that has already been replaced changes nothing, because the
// generation guard is the manager's and the courtesy source reports the manager's number.
// An observer that answered with one of its own would let a stale answer land on a
// current session.
func TestACourtesyVerdictForAReplacedSessionIsIgnored(t *testing.T) {
	srv := courtesyIDP(t)
	mgr := signedInManager(t, srv)
	src := courtesyAccountTokenSource{mgr: mgr}

	stale := mgr.Generation() - 1
	src.ApplyBackendVerdict(context.Background(), stale, "",
		&backend.Error{Code: backend.CodeAuthPermissionDenied, Type: "authentication_error", Message: "not approved"})

	if got := mgr.State(); got != auth.StateSignedInUnverified {
		t.Errorf("state = %q — a verdict for a replaced session was applied", got)
	}
}

// A `/backend` switch cannot leave the previous deployment's links in a turn's advice.
//
// Not an observer test, but the same wiring seen from the other end: advice is rendered
// from links a MANAGER cached, and `/backend` replaces the manager precisely because a
// credential — and everything discovered beside it — belongs to one deployment. The
// failure this pins is a card that sends someone to the old deployment's billing page
// after the session has moved to a new one, which no assertion about the link's shape
// would ever catch.
//
// The other half — that a warmed link reaches a real turn's reply — belongs to a package
// this cannot reach from here.
func TestASwitchLeavesNoStaleAccountLinksInAdvice(t *testing.T) {
	srv := courtesyIDP(t)
	root := t.TempDir()
	// The STORED preference rather than an override: an override pins the session, and a
	// pinned session refuses `/backend` outright.
	if err := config.SaveBackendURL(config.EndpointPath(root), srv.URL); err != nil {
		t.Fatalf("SaveBackendURL: %v", err)
	}
	a, err := Create(CreateOptions{Overrides: config.ConfigOverrides{
		Offline: boolPtr(true), StateDir: &root, ProjectPath: &root,
		Tier: strPtr("operator"), WorkflowIntelligence: boolPtr(false),
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	mgr := a.AuthManager()
	if mgr == nil {
		t.Fatal("the session has no account manager, so there is nothing to cache links on")
	}
	// Warm discovery, which is what a real session does the first time it asks anything
	// about the account.
	if _, err := mgr.Manifest(context.Background()); err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	before := a.accountLinksForAdvice()
	if before.Account == "" || before.Subscribe == "" {
		t.Fatalf("no links were cached for the live endpoint, so the switch below proves nothing: %+v", before)
	}

	if _, err := a.SetBackendURL(backend.LocalBaseURL); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}

	after := a.accountLinksForAdvice()
	if after.Account != "" || after.Subscribe != "" {
		t.Errorf("advice still carries the previous deployment's links after a switch: %+v", after)
	}
}
