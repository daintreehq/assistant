package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// verdictwiring_test.go drives a REAL backend.Client against a real Manager.
//
// The state machine already had its own tests and they all passed while the whole thing
// was disconnected: nothing in production ever called ApplyBackendVerdict or MarkIdentityLive,
// so every transition was exercised by a test calling it directly and by nothing else.
// Testing through the client is what makes these assertions mean anything — they fail if
// the seam comes unplugged, which is the failure that actually happened.

// deployment is one fake Daintree backend: the discovery manifest, the OAuth token
// endpoint, and a scriptable protected route.
type deployment struct {
	srv *httptest.Server

	mu sync.Mutex
	// respond is what /v1/daintree/capabilities answers next. Consumed in order; the
	// last entry repeats, so a test scripts only the responses it cares about.
	responses []func(w http.ResponseWriter)

	protectedCalls atomic.Int64
	// accountResponses scripts /v1/daintree/account, and accountCalls counts it. Kept
	// separate from the protected route's pair so a test can assert "exactly one
	// account request" without a turn's calls muddying the count.
	accountResponses []func(w http.ResponseWriter)
	accountCalls     atomic.Int64
	bearers          chan string
	issued           atomic.Int64
	refreshTokens    map[string]bool
}

func newDeployment(t *testing.T) *deployment {
	t.Helper()
	d := &deployment{
		bearers:       make(chan string, 32),
		refreshTokens: map[string]bool{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc(DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.manifest())
	})

	mux.HandleFunc("/auth/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		presented := r.Form.Get("refresh_token")
		d.mu.Lock()
		live := d.refreshTokens[presented]
		d.mu.Unlock()
		if !live {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		n := d.issued.Add(1)
		next := fmt.Sprintf("refresh-token-%d", n)
		d.mu.Lock()
		delete(d.refreshTokens, presented) // one-time use, as Supabase does
		d.refreshTokens[next] = true
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"access_token":"access-token-%d","refresh_token":%q,"token_type":"bearer","expires_in":3600}`,
			n, next)))
	})

	// A protected Daintree route. Capabilities is the convenient one: it is a plain
	// JSON GET through doJSON, so it exercises the same ladder a turn does without a
	// stream to script.
	mux.HandleFunc("/v1/daintree/capabilities", func(w http.ResponseWriter, r *http.Request) {
		d.protectedCalls.Add(1)
		select {
		case d.bearers <- r.Header.Get("Authorization"):
		default:
		}
		d.mu.Lock()
		var next func(w http.ResponseWriter)
		switch {
		case len(d.responses) > 1:
			next, d.responses = d.responses[0], d.responses[1:]
		case len(d.responses) == 1:
			next = d.responses[0] // the last one repeats
		}
		d.mu.Unlock()
		if next == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		next(w)
	})

	// The streamed turn route. A turn is the OTHER protected call a session makes all
	// day, and it reaches the account seam by a completely different path (the SSE
	// ladder in client.go, not doJSON) — so "a turn must not grant entitlement either"
	// has to be asserted against a turn and not by analogy with capabilities.
	mux.HandleFunc("/v1/daintree/respond", func(w http.ResponseWriter, r *http.Request) {
		d.protectedCalls.Add(1)
		select {
		case d.bearers <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(turnStream))
	})

	// The account status route, scripted independently of the protected route above so
	// a test can drive the two in one run — which is the point of several of the
	// account tests: what a status read does must not depend on what a turn just did.
	mux.HandleFunc(backend.AccountStatusPath, func(w http.ResponseWriter, r *http.Request) {
		d.accountCalls.Add(1)
		d.mu.Lock()
		var next func(w http.ResponseWriter)
		switch {
		case len(d.accountResponses) > 1:
			next, d.accountResponses = d.accountResponses[0], d.accountResponses[1:]
		case len(d.accountResponses) == 1:
			next = d.accountResponses[0]
		}
		d.mu.Unlock()
		if next == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next(w)
	})

	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// scriptAccount sets the account route's answers, in order. The last one repeats.
func (d *deployment) scriptAccount(fns ...func(w http.ResponseWriter)) {
	d.mu.Lock()
	d.accountResponses = fns
	d.mu.Unlock()
}

// accountBody writes one version-1 account status document.
func accountBody(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (d *deployment) manifest() Manifest {
	base := d.srv.URL
	return Manifest{
		Version:               1,
		Environment:           "development",
		Issuer:                base + "/auth/v1",
		AuthorizationEndpoint: base + "/auth/v1/oauth/authorize",
		TokenEndpoint:         base + "/auth/v1/oauth/token",
		JWKSURI:               base + "/auth/v1/.well-known/jwks.json",
		ClientID:              "test-client",
		RedirectURI:           RedirectURI(),
		Scopes:                []string{"openid", "email"},
		SessionPolicy:         SessionPolicy{AccessTokenSeconds: 3600, SessionMaxAgeSeconds: 2592000},
	}
}

// script sets the protected route's answers, in order. The last one repeats.
func (d *deployment) script(fns ...func(w http.ResponseWriter)) {
	d.mu.Lock()
	d.responses = fns
	d.mu.Unlock()
}

// accountError writes one Daintree account error.
func accountError(status int, code string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"error":{"type":"authentication_error","code":%q,"message":"no"}}`, code)))
	}
}

// turnStream is the minimal successful turn: meta, one delta, done.
const turnStream = "event: meta\ndata: {}\n\n" +
	"event: delta\ndata: {\"content\":\"Done.\"}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

func okJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// signedIn returns a Manager holding a live session against d, plus its client.
func signedIn(t *testing.T, d *deployment, store Store) (*Manager, *backend.Client, CredentialKey) {
	t.Helper()
	return signedInAt(t, d, store, nil)
}

// steppingClock returns a clock that advances a fixed second on every read.
//
// The liveness stamp is now the only thing a protected success writes, so several tests
// turn on "did lastVerifiedAt move". Against the real clock that comparison is a
// nanosecond race dressed up as an assertion; a clock that visibly steps makes "this
// call recorded nothing" a fact rather than a coincidence.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return t0.Add(time.Duration(n) * time.Second)
	}
}

// signedInAt is signedIn with an explicit clock. A nil clock means the real one.
func signedInAt(t *testing.T, d *deployment, store Store, now func() time.Time) (*Manager, *backend.Client, CredentialKey) {
	t.Helper()
	m, err := NewManager(Options{
		StateRoot:  t.TempDir(),
		BackendURL: d.srv.URL,
		Store:      store,
		Opener:     NoOpener{},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	key := m.key(man)
	d.mu.Lock()
	d.refreshTokens["refresh-seed"] = true
	d.mu.Unlock()
	if err := store.Save(context.Background(), key, StoredSession{
		Version: storedSessionVersion, RefreshToken: "refresh-seed",
		Issuer: man.Issuer, ClientID: man.ClientID, Environment: man.Environment,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := saveKeyRef(m.AuthDirPath(), key); err != nil {
		t.Fatalf("saveKeyRef: %v", err)
	}

	// Retry.MaxAttempts 1 disables the TRANSIENT retry, so nothing below is confused by
	// a backoff replay. The auth ladder is deliberately not governed by that budget,
	// which is itself one of the things these tests pin.
	c := backend.NewClient(backend.ClientConfig{
		BaseURL:     d.srv.URL,
		TokenSource: m,
		Retry:       backend.RetryPolicy{MaxAttempts: 1},
	})
	return m, c, key
}

// The compile-time half: the manager the client credentials from must be the object the
// client reports outcomes to. A seam satisfied by nothing is how this stayed unplugged.
func TestTheManagerIsBothTheTokenSourceAndTheObserver(t *testing.T) {
	var _ backend.TokenSource = (*Manager)(nil)
	var _ backend.AccountObserver = (*Manager)(nil)
}

// A protected 2xx is a verdict too, and the only one that can say the CREDENTIAL is
// still honoured. It says nothing about billing, and this pins both halves.
//
// Capabilities is the endpoint that matters here: it is protected, it answers 2xx to a
// caller with no plan whatsoever, and it runs at boot. Reading it as an entitlement is
// how every session came to report itself as granted on its first call.
func TestAProtectedSuccessRecordsLivenessWithoutGrantingEntitlement(t *testing.T) {
	d := newDeployment(t)
	d.script(okJSON)
	m, c, _ := signedIn(t, d, NewMemoryStore())

	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	got := m.Status()
	if got.State != StateSignedInUnverified {
		t.Errorf("state = %q, want %q — a 2xx that never consulted billing changed the plan verdict", got.State, StateSignedInUnverified)
	}
	if got.State.CanSpend() {
		t.Error("an unchecked session was cleared to spend by a capabilities 200")
	}
	if got.LastVerifiedAt == nil {
		t.Error("the success recorded no liveness at all — the credential half is the part this DOES prove")
	}
	if bearer := <-d.bearers; bearer == "" {
		t.Error("the request went out with no Authorization header")
	}
}

// The same rule on the streamed path. A turn is the call a session actually makes all
// day, and it reaches the observer through the SSE ladder rather than doJSON — so it
// would be perfectly possible to fix one path and leave the other granting.
func TestATurnSuccessRecordsLivenessWithoutGrantingEntitlement(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())

	if _, err := c.RespondStream(context.Background(), backend.RespondRequest{}, backend.StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}

	got := m.Status()
	if got.State != StateSignedInUnverified {
		t.Errorf("state = %q, want %q — a completed turn is not an entitlement check", got.State, StateSignedInUnverified)
	}
	if got.State.CanSpend() {
		t.Error("a turn cleared an unchecked session to spend")
	}
	if got.LastVerifiedAt == nil {
		t.Error("a completed turn recorded no liveness")
	}
}

// An ANONYMOUS success must confirm nothing. There is no session to confirm, and marking
// one active would report a signed-in account on an install that has never signed in.
func TestAnAnonymousSuccessConfirmsNothing(t *testing.T) {
	d := newDeployment(t)
	d.script(okJSON)
	m, err := NewManager(Options{
		StateRoot: t.TempDir(), BackendURL: d.srv.URL,
		Store: NewMemoryStore(), Opener: NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	c := backend.NewClient(backend.ClientConfig{BaseURL: d.srv.URL, TokenSource: m})

	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	got := m.Status()
	if got.State == StateSignedInActive {
		t.Error("an anonymous request reported a confirmed account session")
	}
	if got.LastVerifiedAt != nil {
		t.Error("an anonymous request stamped a verification time onto a session that does not exist")
	}
	if bearer := <-d.bearers; bearer != "" {
		t.Errorf("an anonymous request carried %q", bearer)
	}
}

// The refresh-and-replay ladder, end to end: an expired token is renewed and the request
// completes, once. Without it a routine hourly expiry is a hard failure on a call a
// single renewal would have finished.
func TestAnExpiredTokenIsRefreshedAndTheRequestReplayedOnce(t *testing.T) {
	d := newDeployment(t)
	d.script(accountError(401, backend.CodeAuthTokenExpired), okJSON)
	m, c, _ := signedIn(t, d, NewMemoryStore())

	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("the request was not replayed after a refresh: %v", err)
	}

	if got := d.protectedCalls.Load(); got != 2 {
		t.Errorf("protected calls = %d, want exactly 2 (the original and one replay)", got)
	}
	first, second := <-d.bearers, <-d.bearers
	if first == second {
		t.Error("the replay presented the SAME token the backend had just refused")
	}
	// The replay's own 2xx confirms the freshly refreshed credential — and confirms only
	// that. A refresh proves the login works; the plan is still a question nobody asked.
	if got := m.Status(); got.LastVerifiedAt == nil {
		t.Error("the successful replay recorded no liveness for the new credential")
	} else if got.State != StateSignedInUnverified {
		t.Errorf("state = %q, want %q after a successful replay", got.State, StateSignedInUnverified)
	}
}

// At most ONE replay. A second means the refresh did not help, and looping there is how
// a client hammers an endpoint that will keep saying no.
func TestTheRefreshReplayHappensAtMostOnce(t *testing.T) {
	d := newDeployment(t)
	d.script(accountError(401, backend.CodeAuthTokenExpired)) // repeats forever
	_, c, _ := signedIn(t, d, NewMemoryStore())

	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("a permanently expired credential reported success")
	}

	if got := d.protectedCalls.Load(); got != 2 {
		t.Errorf("protected calls = %d, want exactly 2 — the ladder is looping", got)
	}
}

// A revoked session is gone upstream. The stored credential is dead weight and must be
// removed, and the shared revision must move so a daemon in another process notices.
func TestARevokedSessionIsClearedAndTheRevisionBumped(t *testing.T) {
	d := newDeployment(t)
	d.script(accountError(401, backend.CodeAuthSessionRevoked))
	store := NewMemoryStore()
	m, c, key := signedIn(t, d, store)
	before := m.Revision().Current()

	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("a revoked session reported success")
	}

	if got := m.State(); got != StateRevoked {
		t.Errorf("state = %q, want %q", got, StateRevoked)
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Errorf("the dead credential survived: %v", err)
	}
	if m.Revision().Current() == before {
		t.Error("the revision did not move — another process would never learn of the revocation")
	}
}

// The other half of the rule, and the more damaging one to get wrong. A plan problem and
// a dependency outage are not credential problems: clearing there makes someone sign in
// again to reach the identical refusal, or turns a blip into a re-login for every user.
func TestPlanAndDependencyVerdictsPreserveTheCredential(t *testing.T) {
	cases := []struct {
		code   string
		status int
		want   State
	}{
		{backend.CodeSubscriptionRequired, 402, StateSubscriptionRequired},
		{backend.CodeSubscriptionInactive, 402, StateSubscriptionInactive},
		{backend.CodeAuthDependencyUnavailable, 503, StateTemporarilyUnavailable},
		{backend.CodeEntitlementUnavailable, 503, StateTemporarilyUnavailable},
		{backend.CodeUsageAccountingUnavailable, 503, StateTemporarilyUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			d := newDeployment(t)
			d.script(accountError(tc.status, tc.code))
			store := NewMemoryStore()
			m, c, key := signedIn(t, d, store)
			if _, err := c.Capabilities(context.Background()); err != nil {
				_ = err // the call is expected to fail; the credential is the assertion
			}

			if _, err := store.Load(context.Background(), key); err != nil {
				t.Fatalf("the credential was discarded over a non-credential problem: %v", err)
			}
			if got := m.State(); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// The two 403s keep the credential and must NOT start a refresh loop — a new token
// carries the same rejected client, or the same insufficient authority.
func TestASettledRefusalKeepsTheCredentialAndDoesNotRetry(t *testing.T) {
	for _, code := range []string{backend.CodeAuthClientNotAllowed, backend.CodeAuthPermissionDenied} {
		t.Run(code, func(t *testing.T) {
			d := newDeployment(t)
			d.script(accountError(403, code))
			store := NewMemoryStore()
			m, c, key := signedIn(t, d, store)

			if _, err := c.Capabilities(context.Background()); err == nil {
				t.Fatal("a refused credential reported success")
			}

			if got := d.protectedCalls.Load(); got != 1 {
				t.Errorf("protected calls = %d, want 1 — a settled 403 was replayed", got)
			}
			if _, err := store.Load(context.Background(), key); err != nil {
				t.Fatalf("a settled refusal discarded the credential: %v", err)
			}
			if got := m.State(); got != StateAccessRefused {
				t.Errorf("state = %q, want %q", got, StateAccessRefused)
			}
		})
	}
}

// Verdicts arrive late. One for a session the user has already replaced must not touch
// the replacement — which, for a revocation, means not deleting a credential that was
// created after the request went out.
func TestALateVerdictCannotAffectANewerLogin(t *testing.T) {
	d := newDeployment(t)
	store := NewMemoryStore()
	m, _, key := signedIn(t, d, store)
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	staleGen, staleToken := m.Generation(), currentToken(m)

	// Whatever happened in between, the identity has moved on.
	m.clearSession(context.Background())
	d.mu.Lock()
	d.refreshTokens["refresh-relogin"] = true
	d.mu.Unlock()
	if err := m.SeedForTest(context.Background(), ptrManifest(t, m), "refresh-relogin"); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	// SeedForTest writes the store and the descriptor; it does not touch state. Hydrate
	// is what a real login's caller does next, and without it the assertion below would
	// be reading the state this test's own clearSession left behind.
	m.Hydrate(context.Background())
	if got := m.State(); got == StateRevoked {
		t.Fatalf("setup: the re-login did not take, state = %q", got)
	}
	newGen := m.Generation()

	// The answer for the OLD request finally lands.
	m.ApplyBackendVerdict(context.Background(), staleGen, staleToken,
		&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthSessionRevoked})

	if _, err := store.Load(context.Background(), key); err != nil {
		t.Fatalf("a verdict for a replaced session deleted the new credential: %v", err)
	}
	if got := m.State(); got == StateRevoked {
		t.Error("a stale verdict revoked the session that replaced it")
	}
	if m.Generation() != newGen {
		t.Error("a stale verdict advanced the generation")
	}
}

// The same rule one level finer. A refresh does NOT move the generation, so a 401 for a
// token that was replaced seconds ago would otherwise be applied to its replacement.
func TestAVerdictForAReplacedTokenIsIgnored(t *testing.T) {
	d := newDeployment(t)
	m, _, _ := signedIn(t, d, NewMemoryStore())
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	gen, old := m.Generation(), currentToken(m)

	// An ordinary refresh, same generation, new token.
	m.Invalidate(old)
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if currentToken(m) == old {
		t.Fatal("setup: the token did not actually rotate")
	}

	m.ApplyBackendVerdict(context.Background(), gen, old,
		&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthSessionRevoked})

	if got := m.State(); got == StateRevoked {
		t.Error("a verdict about a superseded token revoked the session holding its replacement")
	}
}

// A logout in ANOTHER process reaches this one as a revision change, not as a local
// state write — so nothing advances the generation the ordinary way. A success already
// in flight when that happened would otherwise land with a generation that still
// matched and put a closed account back to work, which is precisely what the daemon's
// spend gate is built on top of.
func TestALateSuccessCannotVouchForASessionEndedElsewhere(t *testing.T) {
	d := newDeployment(t)
	d.script(okJSON)
	store := NewMemoryStore()
	m, c, _ := signedInAt(t, d, store, steppingClock())

	// Confirm it, so there is a live session for a late answer to vouch for.
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	confirmed := m.Status().LastVerifiedAt
	if confirmed == nil {
		t.Fatal("setup: the protected success recorded no liveness")
	}
	inFlightGen := m.Generation()

	// Somebody logs out in a terminal: another process bumps the shared marker.
	if err := NewRevision(m.AuthDirPath()).Bump(context.Background()); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	// This process notices the next time anything asks for a credential.
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	// The late 2xx for the request that was already in flight.
	m.MarkIdentityLive(inFlightGen)

	// Nothing is recorded for it, and nothing survives from before either.
	//
	// Noticing the identity change already cleared the stamp — a verification time
	// belongs to the identity that earned it, and leaving it would let `auth status`
	// report the replacement session as freshly checked on the strength of the one it
	// replaced. So the late call has to write nothing at all: the assertion is that the
	// field is still nil, which fails both ways an applied call could go, since the
	// clock steps on every read and any write would land on a new instant.
	if got := m.Status().LastVerifiedAt; got != nil {
		t.Errorf("verification time = %s (was %s before the identity changed) — a success in flight during a logout elsewhere vouched for the session", got, confirmed)
	}
}

// A confirmation may only CONFIRM. Vouching for a session from a state that says there
// is none is how a status line comes to report a freshly verified account on a machine
// holding no credential at all.
func TestMarkIdentityLiveOnlyVouchesForAnExistingSession(t *testing.T) {
	d := newDeployment(t)
	m, _, _ := signedIn(t, d, NewMemoryStore())

	for _, dead := range []State{StateSignedOut, StateRevoked, StateAccountsUnavailable} {
		m.mu.Lock()
		m.state = dead
		m.lastVerifiedAt = nil
		gen := m.generation
		m.mu.Unlock()

		m.MarkIdentityLive(gen)

		if got := m.State(); got != dead {
			t.Errorf("MarkIdentityLive revived %q into %q", dead, got)
		}
		m.mu.Lock()
		stamped := m.lastVerifiedAt
		m.mu.Unlock()
		if stamped != nil {
			t.Errorf("MarkIdentityLive stamped a verification onto %q", dead)
		}
	}
}

// The states a success must LEAVE ALONE, which is the whole of the fix.
//
// Each of these is a signed-in session carrying a specific, expensive truth: the plan
// was refused, the plan was never looked up, the check itself could not be made, the
// deployment rejects this credential. A protected 2xx knows none of that — it knows a
// route answered — and every one of these used to be overwritten with "signed in and
// entitled" by the first capabilities call of the session.
func TestAProtectedSuccessPreservesEveryUncertainVerdict(t *testing.T) {
	preserved := []State{
		StateSignedInUnverified,
		StateSubscriptionRequired,
		StateSubscriptionInactive,
		StateTemporarilyUnavailable,
		StateAccessRefused,
		StateStorageUnavailable,
		// StateRefreshing was promoted too. It resolves on its own — a completed
		// refresh writes StateSignedInUnverified — so a success arriving mid-flight
		// has no business guessing the outcome ahead of it.
		StateRefreshing,
	}
	for _, before := range preserved {
		d := newDeployment(t)
		d.script(okJSON)
		m, c, _ := signedIn(t, d, NewMemoryStore())
		if _, err := m.AccessToken(context.Background()); err != nil {
			t.Fatalf("AccessToken: %v", err)
		}
		// The error is seeded alongside the state, because the two are retired together
		// or not at all. MarkActive cleared lastErr, and it could: it also moved the
		// state past whatever the error explained. Without this assertion, putting
		// `m.lastErr = nil` back would pass every check below while leaving
		// `temporarily_unavailable` and `access_refused` naming a problem and then
		// refusing to say which.
		seeded := &backend.Error{Code: backend.CodeAuthDependencyUnavailable, Type: "api_error", Message: "seeded"}
		m.mu.Lock()
		m.state = before
		m.lastErr = seeded
		m.mu.Unlock()

		if _, err := c.Capabilities(context.Background()); err != nil {
			t.Fatalf("Capabilities: %v", err)
		}

		got := m.Status()
		if got.State != before {
			t.Errorf("state %q became %q after an unrelated 200", before, got.State)
		}
		if got.State.CanSpend() {
			t.Errorf("state %q was cleared to spend by an unrelated 200", before)
		}
		if got.LastVerifiedAt == nil {
			t.Errorf("state %q: the success recorded no liveness", before)
		}
		if got.LastErrorCode != seeded.Code {
			t.Errorf("state %q: last error code = %q, want %q — the success dropped the diagnosis its state still depends on",
				before, got.LastErrorCode, seeded.Code)
		}
	}
}

// The replay must not go out without a credential. When a refresh fails because the
// grant was rejected, the session is deleted — and an anonymous replay to an open
// backend then SUCCEEDS as the wrong principal, reporting a confirmed session for one
// that has just been removed.
func TestAFailedRefreshDoesNotReplayAnonymously(t *testing.T) {
	d := newDeployment(t)
	d.script(accountError(401, backend.CodeAuthTokenExpired), okJSON)
	store := NewMemoryStore()
	m, c, _ := signedIn(t, d, store)

	// Kill the refresh token, so the renewal the ladder attempts cannot succeed.
	d.mu.Lock()
	d.refreshTokens = map[string]bool{}
	d.mu.Unlock()

	if _, err := c.Capabilities(context.Background()); err == nil {
		t.Fatal("the request reported success after its refresh failed")
	}

	if got := d.protectedCalls.Load(); got != 1 {
		t.Errorf("protected calls = %d, want 1 — a replay went out with no usable credential", got)
	}
	if got := m.State(); got == StateSignedInActive {
		t.Error("a session with no working refresh token was reported as confirmed")
	}
}

// ptrManifest fetches the manifest for a seeded re-login.
func ptrManifest(t *testing.T, m *Manager) *Manifest {
	t.Helper()
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return man
}
