package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// --- test rig -----------------------------------------------------------------------

// idp is a scriptable identity provider plus the backend manifest endpoint, so a whole
// login and refresh can run end to end without a network or a browser.
type idp struct {
	mu sync.Mutex
	// refreshTokens maps a live refresh token to its successor. Modelling ROTATION is
	// the point: Supabase issues one-time-use refresh tokens, and presenting a consumed
	// one must fail exactly as it would in production.
	refreshTokens map[string]string
	consumed      map[string]bool
	issued        atomic.Int64
	refreshCalls  atomic.Int64
	// failNext makes the next token call fail with this OAuth error code.
	failNext string
	// hang blocks token calls until closed, for concurrency tests.
	hang chan struct{}

	srv      *httptest.Server
	manifest Manifest
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{
		refreshTokens: map[string]string{},
		consumed:      map[string]bool{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc(DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		m := p.manifest
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})

	mux.HandleFunc("/auth/v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if p.hang != nil {
			<-p.hang
		}
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.failNext != "" {
			code := p.failNext
			p.failNext = ""
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + code + `","error_description":"LEAKY-PROVIDER-TEXT"}`))
			return
		}

		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
				return
			}
		case "refresh_token":
			p.refreshCalls.Add(1)
			presented := r.Form.Get("refresh_token")
			if p.consumed[presented] {
				// Reuse detection, as a real provider does.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"already used"}`))
				return
			}
			if _, ok := p.refreshTokens[presented]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			p.consumed[presented] = true
		}

		n := p.issued.Add(1)
		access := fmt.Sprintf("access-token-%d", n)
		refresh := fmt.Sprintf("refresh-token-%d", n)
		p.refreshTokens[refresh] = ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"access_token":%q,"refresh_token":%q,"token_type":"bearer","expires_in":3600}`, access, refresh)))
	})

	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	// The manifest points its OAuth endpoints at this same server. The issuer anchor is
	// satisfied by the loopback exemption, which is exactly the local-development shape.
	base := p.srv.URL
	p.manifest = Manifest{
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
	return p
}

// seed installs a live refresh token, as a completed login would.
func (p *idp) seed(token string) {
	p.mu.Lock()
	p.refreshTokens[token] = ""
	p.mu.Unlock()
}

// newManager builds a Manager against the fake IdP with an isolated state root.
func newManager(t *testing.T, p *idp, store Store) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		StateRoot:  t.TempDir(),
		BackendURL: p.srv.URL,
		Store:      store,
		Opener:     NoOpener{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// storedFor writes a session directly, standing in for a completed login.
func storedFor(t *testing.T, m *Manager, p *idp, store Store, refresh string) CredentialKey {
	t.Helper()
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	key := m.key(man)
	p.seed(refresh)
	if err := store.Save(context.Background(), key, StoredSession{
		Version: storedSessionVersion, RefreshToken: refresh,
		Issuer: man.Issuer, ClientID: man.ClientID, Environment: man.Environment,
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A real login also records the descriptor; AccessToken uses its presence as the
	// cheap "has this machine ever signed in?" answer.
	if err := saveKeyRef(m.AuthDirPath(), key); err != nil {
		t.Fatalf("saveKeyRef: %v", err)
	}
	return key
}

// --- the seam --------------------------------------------------------------------

func TestManagerSatisfiesTheBackendTokenSource(t *testing.T) {
	var _ backend.TokenSource = (*Manager)(nil)
	var _ backend.TokenScrubber = (*Manager)(nil)
}

func TestAccessTokenRefreshesFromAStoredSession(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if !strings.HasPrefix(tok, "access-token-") {
		t.Fatalf("token = %q", tok)
	}
	// A second call must NOT refresh again — the token is fresh.
	before := p.refreshCalls.Load()
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("second AccessToken: %v", err)
	}
	if p.refreshCalls.Load() != before {
		t.Fatal("a fresh token was refreshed again — every request would spend a rotating token")
	}
}

// Signed out means "send no Authorization header", NOT an error.
//
// This is the contract that keeps the current open-door backend working. Returning an
// error here aborts the request inside setHeaders, so a machine that has simply never
// signed in could not reach a backend that was perfectly willing to serve it
// anonymously. The backend decides whether anonymous access is allowed; when it stops
// allowing it, it answers 401 with an account code.
func TestBeingSignedOutSendsNoHeaderRatherThanFailing(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("a signed-out AccessToken failed: %v — every request would abort locally", err)
	}
	if tok != "" {
		t.Fatalf("token = %q, want empty", tok)
	}
	if m.State() != StateSignedOut {
		t.Fatalf("state = %q, want signed_out", m.State())
	}
}

// ...and it must cost nothing. Discovery on every request would make the signed-out path
// — which is every install today — depend on a backend endpoint older deployments do not
// even serve.
func TestTheSignedOutPathMakesNoNetworkCall(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	p.srv.Close() // nothing is reachable

	done := make(chan error, 1)
	go func() {
		_, err := m.AccessToken(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("signed-out AccessToken failed with the backend down: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signed-out AccessToken tried to reach the network")
	}
}

// A REAL fault must still fail loudly — proceeding anonymously would silently downgrade
// a session the user believes they have.
func TestALockedCredentialStoreStillFails(t *testing.T) {
	p := newIDP(t)
	store := &lockedStore{}
	m := newManager(t, p, store)
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if err := saveKeyRef(m.AuthDirPath(), m.key(man)); err != nil {
		t.Fatalf("saveKeyRef: %v", err)
	}
	if _, err := m.AccessToken(context.Background()); err == nil {
		t.Fatal("a locked credential store produced an anonymous request instead of an error")
	}
}

// lockedStore refuses every read, as a locked keychain does.
type lockedStore struct{}

func (lockedStore) Load(context.Context, CredentialKey) (StoredSession, error) {
	return StoredSession{}, ErrStoreLocked
}
func (lockedStore) Save(context.Context, CredentialKey, StoredSession) error { return ErrStoreLocked }
func (lockedStore) Delete(context.Context, CredentialKey) error              { return nil }
func (lockedStore) Tier(context.Context) StorageTier                         { return TierUnavailable }

// Hydrate is what makes `auth status` able to answer at all: Status is I/O-free by
// design, so a manager freshly built in a new process starts at unknown.
func TestHydrateSettlesStateFromTheStoredCredential(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	fresh, err := NewManager(Options{StateRoot: m.stateRoot, BackendURL: p.srv.URL, Store: store, Opener: NoOpener{}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if fresh.State() != StateUnknown {
		t.Fatalf("a new manager started at %q, want unknown", fresh.State())
	}
	fresh.Hydrate(context.Background())
	if !fresh.State().SignedIn() {
		t.Fatalf("state after Hydrate = %q — `auth status` would report unknown right after a login", fresh.State())
	}
	// It must NOT refresh: asking about an account should not spend a one-time-use token.
	if p.refreshCalls.Load() != 0 {
		t.Fatalf("Hydrate performed %d refreshes", p.refreshCalls.Load())
	}
}

func TestHydrateReportsSignedOutWithNoCredential(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	m.Hydrate(context.Background())
	if m.State() != StateSignedOut {
		t.Fatalf("state = %q, want signed_out", m.State())
	}
}

// THE concurrency property. Supabase refresh tokens are one-time use, so N concurrent
// callers must produce exactly ONE refresh — not N, of which N-1 present a consumed
// token and trip the provider's reuse detection.
func TestConcurrentCallersProduceExactlyOneRefresh(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	p.hang = make(chan struct{})
	const n = 16
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = m.AccessToken(context.Background())
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let them all pile up on the singleflight
	close(p.hang)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := p.refreshCalls.Load(); got != 1 {
		t.Fatalf("%d refresh calls for %d concurrent callers, want 1 — a rotating token would be spent per caller", got, n)
	}
	for i, tok := range tokens {
		if tok != tokens[0] {
			t.Fatalf("caller %d got %q, caller 0 got %q — callers disagree about the current token", i, tok, tokens[0])
		}
	}
}

// The rotated token must be persisted, or the next process cannot refresh at all.
func TestARotatedRefreshTokenIsPersisted(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")

	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	got, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken == "refresh-seed" {
		t.Fatal("the rotated refresh token was not persisted — the next process could not refresh")
	}
	if !strings.HasPrefix(got.RefreshToken, "refresh-token-") {
		t.Fatalf("stored refresh token = %q", got.RefreshToken)
	}
}

// A rotation must NOT move the identity marker.
//
// This test asserted the opposite until the review showed what that produces: the marker
// clears every other process's still-valid access token, so P1 rotating makes P2 discard
// a good token and rotate, which makes P1 discard ITS good token and rotate — a
// perpetual exchange spending a one-time-use refresh token per round trip. A rotation
// changes only the stored refresh token, which every process re-reads under the lock when
// it next needs one.
func TestARotationDoesNotMoveTheIdentityMarker(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	before := m.Revision().Current()
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if m.Revision().Current() != before {
		t.Fatal("a routine rotation moved the identity marker — every other process would discard a valid token and rotate too")
	}
}

// THE most important error rule in the package: "we could not check" must never be
// rendered as "you are signed out". A network failure keeps the credential.
func TestATransientRefreshFailureKeepsTheCredential(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")

	p.mu.Lock()
	p.failNext = "temporarily_unavailable"
	p.mu.Unlock()

	if _, err := m.AccessToken(context.Background()); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := store.Load(context.Background(), key); err != nil {
		t.Fatalf("the credential was deleted on a transient failure: %v", err)
	}
	if m.State() == StateRevoked || m.State() == StateSignedOut {
		t.Fatalf("state = %q — an outage was rendered as a logout", m.State())
	}
	if m.State() != StateTemporarilyUnavailable {
		t.Fatalf("state = %q, want temporarily_unavailable", m.State())
	}
}

// ...and the one provider answer that DOES mean the session is gone deletes it.
func TestAnInvalidGrantDeletesTheCredential(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")

	p.mu.Lock()
	p.failNext = "invalid_grant"
	p.mu.Unlock()

	if _, err := m.AccessToken(context.Background()); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after invalid_grant = %v, want ErrNotFound — the dead credential survived", err)
	}
	if m.State() != StateRevoked {
		t.Fatalf("state = %q, want revoked", m.State())
	}
}

// Provider error_description is attacker-influenced text that lands in scrollback and a
// debug log. The stable code is enough.
func TestProviderErrorTextNeverReachesTheError(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	p.mu.Lock()
	p.failNext = "invalid_request"
	p.mu.Unlock()

	_, err := m.AccessToken(context.Background())
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), "LEAKY-PROVIDER-TEXT") {
		t.Fatalf("the provider's error_description reached the error: %q", err.Error())
	}
}

// The cross-process invalidation signal: another process logged out, and this one must
// stop using its cached token before the next request.
func TestARevisionChangeDropsTheCachedToken(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	first, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	// Another process rotates the credential and bumps the marker.
	other := NewRevision(m.AuthDirPath())
	if err := other.Bump(context.Background()); err != nil {
		t.Fatalf("Bump: %v", err)
	}

	second, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after bump: %v", err)
	}
	if second == first {
		t.Fatal("the cached token survived a revision change — a logged-out daemon would keep spending")
	}
}

// Invalidate names the token being rejected, because refreshes race: a bare reset from
// one failing caller would discard the good token another just minted.
func TestInvalidateOnlyClearsTheNamedToken(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	cur, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	m.Invalidate("some-other-stale-token")
	if currentToken(m) != cur {
		t.Fatal("invalidating a DIFFERENT token discarded the live one")
	}

	m.Invalidate(cur)
	if currentToken(m) == cur {
		t.Fatal("the named token is still the one that would be sent")
	}
	// It stays MASKABLE, which is a different question. A request that went out with it
	// can still be in flight, and its error can echo the header back; a scrub list that
	// only knew the current token would leave the old one in terminal scrollback.
	if !contains(m.Secrets(), cur) {
		t.Errorf("Secrets() = %v, want the superseded token still maskable", m.Secrets())
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// --- logout -------------------------------------------------------------------------

// A user must always be able to remove access from their own machine, network or not.
func TestLogoutDeletesLocallyAndBumpsTheRevision(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	before := m.Revision().Current()
	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the credential survived logout: %v", err)
	}
	if m.State() != StateSignedOut {
		t.Fatalf("state = %q, want signed_out", m.State())
	}
	if m.Revision().Current() == before {
		t.Fatal("logout did not move the revision — other processes would keep spending")
	}
	if got := m.Secrets(); len(got) != 0 {
		t.Fatal("an access token survived logout in memory")
	}
}

// --- status -------------------------------------------------------------------------

// The type must have nowhere to put a credential, so a future caller cannot render one.
func TestStatusCarriesNoCredential(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	body, err := json.Marshal(m.Status())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rendered := string(body)
	if strings.Contains(rendered, tok) {
		t.Fatalf("the access token appeared in status: %s", rendered)
	}
	if strings.Contains(rendered, "refresh") && strings.Contains(rendered, "token") {
		// Not a substring ban on the word — check the actual keys.
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		for k := range fields {
			if strings.Contains(strings.ToLower(k), "token") {
				t.Errorf("status has a token-shaped field %q", k)
			}
		}
	}
}

// A status call must be answerable while the network is down and the keychain is locked
// — precisely when someone asks what is going on.
func TestStatusPerformsNoIO(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	p.srv.Close() // the backend is now unreachable

	done := make(chan Status, 1)
	go func() { done <- m.Status() }()
	select {
	case s := <-done:
		if s.State != StateUnknown {
			t.Fatalf("state = %q, want unknown before anything happened", s.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Status blocked — it must not perform I/O")
	}
}

func TestSubjectHashIsStableAndOneWay(t *testing.T) {
	const sub = "8f14e45f-ea8b-4c1d-9b2a-0000feedface"
	h := SubjectHash(sub)
	if h == "" || len(h) != 16 {
		t.Fatalf("hash = %q, want 16 hex characters", h)
	}
	if h != SubjectHash(sub) {
		t.Fatal("the hash is not stable")
	}
	if strings.Contains(h, sub[:8]) {
		t.Fatal("the hash echoes the subject")
	}
	if SubjectHash("") != "" {
		t.Fatal("an empty subject produced a hash")
	}
}

// --- backend verdicts ------------------------------------------------------------

// The seam between the two taxonomies: the backend says what is true about the account,
// this decides what it means for the credential here. Only RemedyClear deletes.
func TestBackendVerdictsMapToLocalStateWithoutOverreacting(t *testing.T) {
	cases := []struct {
		code       string
		want       State
		mustDelete bool
	}{
		{backend.CodeSubscriptionRequired, StateSubscriptionRequired, false},
		{backend.CodeSubscriptionInactive, StateSubscriptionInactive, false},
		{backend.CodeEntitlementUnavailable, StateTemporarilyUnavailable, false},
		{backend.CodeAuthDependencyUnavailable, StateTemporarilyUnavailable, false},
		{backend.CodeAuthSessionRevoked, StateRevoked, true},
	}
	for _, tc := range cases {
		p := newIDP(t)
		store := NewMemoryStore()
		m := newManager(t, p, store)
		key := storedFor(t, m, p, store, "refresh-seed")
		if _, err := m.AccessToken(context.Background()); err != nil {
			t.Fatalf("%s: AccessToken: %v", tc.code, err)
		}

		m.ApplyBackendVerdict(context.Background(), m.Generation(), currentToken(m), &backend.Error{Code: tc.code})

		if got := m.State(); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.code, got, tc.want)
		}
		_, loadErr := store.Load(context.Background(), key)
		deleted := errors.Is(loadErr, ErrNotFound)
		if deleted != tc.mustDelete {
			t.Errorf("%s: credential deleted = %v, want %v", tc.code, deleted, tc.mustDelete)
		}
	}
}

// auth_client_not_allowed is a valid token this deployment will not take. Nothing about
// the credential is wrong, so it must survive — and a refresh loop must not start.
func TestAClientMismatchDoesNotDiscardTheCredential(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	m.ApplyBackendVerdict(context.Background(), m.Generation(), currentToken(m), &backend.Error{
		HTTPStatus: 403, Code: backend.CodeAuthClientNotAllowed,
	})
	if _, err := store.Load(context.Background(), key); err != nil {
		t.Fatalf("a client mismatch discarded the credential: %v", err)
	}
}

// --- state machine ------------------------------------------------------------------

func TestStatePredicatesDrawTheRightLines(t *testing.T) {
	// A plan problem is an AUTHENTICATED session. Treating it as signed out sends the
	// user through a browser flow to reach the identical 402.
	for _, s := range []State{StateSubscriptionRequired, StateSubscriptionInactive} {
		if !s.SignedIn() {
			t.Errorf("%s: SignedIn() = false", s)
		}
		if s.NeedsLogin() {
			t.Errorf("%s: NeedsLogin() = true", s)
		}
		if !s.NeedsPlan() {
			t.Errorf("%s: NeedsPlan() = false", s)
		}
	}
	// "We could not check" is not "you are signed out".
	if StateTemporarilyUnavailable.NeedsLogin() {
		t.Error("temporarily_unavailable demanded a login — an outage would discard a working credential")
	}
	if !StateTemporarilyUnavailable.SignedIn() {
		t.Error("temporarily_unavailable reported signed out")
	}
	// Only these two genuinely need a fresh login.
	for _, s := range []State{StateSignedOut, StateRevoked} {
		if !s.NeedsLogin() {
			t.Errorf("%s: NeedsLogin() = false", s)
		}
		if s.SignedIn() {
			t.Errorf("%s: SignedIn() = true", s)
		}
	}
	// An unattended process must not discover its login is dead by spending money.
	if StateSignedInUnverified.CanSpend() {
		t.Error("an unverified session was allowed to spend unattended")
	}
	if !StateSignedInActive.CanSpend() {
		t.Error("a confirmed active session was not allowed to spend")
	}
	if StateStorageUnavailable.CanSpend() {
		t.Error("a non-persisted session was allowed to spend unattended")
	}
}

func TestBackendRemediesMapToStates(t *testing.T) {
	for r, want := range map[backend.AuthRemedy]State{
		backend.RemedyClear:           StateRevoked,
		backend.RemedySignIn:          StateSignedOut,
		backend.RemedyRefresh:         StateRefreshing,
		backend.RemedyRefreshOrSignIn: StateRefreshing,
		// NOT StateTemporarilyUnavailable, which is where this used to land. A rejected
		// OAuth client or a credential without the required permission is SETTLED: the
		// backend answered, and no retry, refresh or re-login changes the answer.
		backend.RemedyReconfigure: StateAccessRefused,
		backend.RemedyNone:        StateUnknown,
	} {
		if got := StateForRemedy(r); got != want {
			t.Errorf("StateForRemedy(%s) = %q, want %q", r, got, want)
		}
	}
}

// --- token lifetimes ------------------------------------------------------------

func TestImplausibleTokenLifetimesAreRefused(t *testing.T) {
	for _, expiresIn := range []int64{1, int64((48 * time.Hour).Seconds())} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"access_token":"a","refresh_token":"r","token_type":"bearer","expires_in":%d}`, expiresIn)))
		}))
		m := Manifest{TokenEndpoint: srv.URL, ClientID: "c"}
		_, err := newTokenClient(nil).Refresh(context.Background(), &m, "r")
		if err == nil {
			t.Errorf("expires_in=%d was accepted", expiresIn)
		}
		srv.Close()
	}
}

// A token type this client cannot send must not be accepted — composing an
// Authorization header we do not understand would fail at the backend, opaquely.
func TestANonBearerTokenTypeIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"mac","expires_in":3600}`))
	}))
	defer srv.Close()
	m := Manifest{TokenEndpoint: srv.URL, ClientID: "c"}
	if _, err := newTokenClient(nil).Refresh(context.Background(), &m, "r"); err == nil {
		t.Fatal("a non-Bearer token type was accepted")
	}
}

// The token request body carries the authorization code AND the PKCE verifier, and a
// 307 replays a POST body at the new location.
func TestTheTokenClientRefusesRedirects(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"bearer","expires_in":3600}`))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	m := Manifest{TokenEndpoint: srv.URL, ClientID: "c"}
	if _, err := newTokenClient(nil).Exchange(context.Background(), &m, "code", "verifier"); err == nil {
		t.Fatal("the token client followed a redirect")
	}
	if atomic.LoadInt32(&targetHits) != 0 {
		t.Fatal("the redirect target received the authorization code and PKCE verifier")
	}
}

func TestProactiveRefreshWindow(t *testing.T) {
	now := time.Now()
	fresh := TokenSet{AccessToken: "a", ExpiresAt: now.Add(time.Hour)}
	if fresh.NeedsRefresh(now) {
		t.Error("a token with an hour left wanted refreshing")
	}
	soon := TokenSet{AccessToken: "a", ExpiresAt: now.Add(4 * time.Minute)}
	if !soon.NeedsRefresh(now) {
		t.Error("a token inside the 5-minute lead time was not refreshed proactively")
	}
	// No expiry known: proactive refresh is disabled rather than guessed at, so the
	// reactive 401 path handles it.
	unknown := TokenSet{AccessToken: "a"}
	if unknown.NeedsRefresh(now) {
		t.Error("a token with no known expiry was refreshed on a schedule nobody chose")
	}
	empty := TokenSet{}
	if !empty.NeedsRefresh(now) {
		t.Error("an absent token did not need refreshing")
	}
}

// currentToken reads the manager's live access token, for tests that must pass it back
// with a verdict.
// currentToken reads the access token that would actually be SENT. Deliberately not via
// Secrets(), which is the scrub list and now also carries superseded tokens.
func currentToken(m *Manager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.access.AccessToken
}

// THE fix for the refresh storm. Two processes sharing one credential must not refresh
// each other: a rotation leaves every other process's access token valid, so it must not
// move the identity marker.
func TestARotationDoesNotInvalidateOtherProcesses(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	root := t.TempDir()

	newProc := func() *Manager {
		m, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: NoOpener{}})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}
	p1, p2 := newProc(), newProc()
	storedFor(t, p1, p, store, "refresh-seed")

	// P1 refreshes (one rotation).
	if _, err := p1.AccessToken(context.Background()); err != nil {
		t.Fatalf("p1: %v", err)
	}
	// P2 refreshes (a second rotation, using the token P1 stored).
	if _, err := p2.AccessToken(context.Background()); err != nil {
		t.Fatalf("p2: %v", err)
	}
	afterTwo := p.refreshCalls.Load()

	// Now each of them makes several more requests. If a rotation bumped the identity
	// marker, every one of these would discard a perfectly good access token and rotate
	// again — the ping-pong.
	for i := 0; i < 5; i++ {
		if _, err := p1.AccessToken(context.Background()); err != nil {
			t.Fatalf("p1 request %d: %v", i, err)
		}
		if _, err := p2.AccessToken(context.Background()); err != nil {
			t.Fatalf("p2 request %d: %v", i, err)
		}
	}
	if got := p.refreshCalls.Load(); got != afterTwo {
		t.Fatalf("%d refreshes after 10 further requests (was %d) — the processes are refreshing each other", got, afterTwo)
	}
}

// A logout in one process MUST still reach the other. This is the signal the marker
// exists for, and the rotation fix must not have disabled it.
func TestALogoutStillReachesAnotherProcess(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	root := t.TempDir()
	mk := func() *Manager {
		m, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: NoOpener{}})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}
	daemon, terminal := mk(), mk()
	storedFor(t, daemon, p, store, "refresh-seed")
	if _, err := daemon.AccessToken(context.Background()); err != nil {
		t.Fatalf("daemon: %v", err)
	}

	if _, err := terminal.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	// The daemon must stop presenting the old token. It reports signed out rather than
	// failing, which is the correct anonymous-request contract — what matters is that the
	// cached credential is gone.
	tok, err := daemon.AccessToken(context.Background())
	if err == nil && tok != "" {
		t.Fatal("the daemon kept using its cached token after a logout elsewhere — it would keep spending")
	}
}

// A user must be able to sign out of their own machine with no network.
func TestLogoutWorksOffline(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	key := storedFor(t, m, p, store, "refresh-seed")
	// Record the descriptor as a real login would.
	if err := saveKeyRef(m.AuthDirPath(), key); err != nil {
		t.Fatalf("saveKeyRef: %v", err)
	}
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	p.srv.Close()             // the backend is now unreachable
	m.discoverer.Invalidate() // and its manifest is not cached

	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("offline Logout: %v", err)
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the credential survived an offline logout: %v — the keychain entry would outlive the user's decision", err)
	}
}

// Verdicts arrive late. One for a credential this process has already replaced must not
// delete the replacement.
func TestAStaleVerdictCannotDestroyACurrentSession(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	staleGen := m.Generation()
	staleToken := currentToken(m)

	// The user logs out and signs back in; the generation moves twice.
	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	newKey := storedFor(t, m, p, store, "refresh-second")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken after re-login: %v", err)
	}

	// The 401 from the FIRST session finally lands.
	m.ApplyBackendVerdict(context.Background(), staleGen, staleToken,
		&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthSessionRevoked})

	if _, err := store.Load(context.Background(), newKey); err != nil {
		t.Fatalf("a stale revocation deleted the CURRENT session: %v", err)
	}
	if m.State() == StateRevoked {
		t.Fatal("a stale revocation marked the current session revoked")
	}
}

// ...and a late confirmation must not resurrect a session that has been logged out.
func TestAStaleMarkActiveCannotResurrectALogout(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	gen := m.Generation()
	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	m.MarkActive(gen)
	if m.State() != StateSignedOut {
		t.Fatalf("state = %q — a late confirmation showed the user signed in with no credential", m.State())
	}
}

// auth_token_expired on a token with no readable expiry would otherwise be re-presented
// forever, because proactive refresh is disabled without an expiry.
func TestAnExpiredVerdictDropsTheTokenSoTheNextCallRefreshes(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	first, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	m.ApplyBackendVerdict(context.Background(), m.Generation(), first,
		&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthTokenExpired})

	second, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after expiry verdict: %v", err)
	}
	if second == first {
		t.Fatal("the rejected token was handed back again — the request would 401 forever")
	}
}

// A token born already needing a refresh would rotate a one-time-use token on every
// single request.
func TestALifetimeShorterThanTheRefreshLeadIsRefused(t *testing.T) {
	if minTokenLifetime <= refreshLeadTime {
		t.Fatalf("minTokenLifetime (%s) must exceed refreshLeadTime (%s), or accepted tokens are born needing a refresh",
			minTokenLifetime, refreshLeadTime)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"bearer","expires_in":60}`))
	}))
	defer srv.Close()
	man := Manifest{TokenEndpoint: srv.URL, ClientID: "c"}
	if _, err := newTokenClient(nil).Refresh(context.Background(), &man, "r"); err == nil {
		t.Fatal("a 60-second token was accepted, so every request would rotate")
	}
}

// A JWT claiming a year-long expiry must not walk around the bound that expires_in obeys
// — otherwise a revocation would go unnoticed for a year.
func TestAJWTExpiryObeysTheSameSanityBound(t *testing.T) {
	far := time.Now().Add(365 * 24 * time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, far)))
	jwt := "eyJhbGciOiJub25lIn0." + payload + ".sig"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"access_token":%q,"refresh_token":"r","token_type":"bearer"}`, jwt)))
	}))
	defer srv.Close()
	man := Manifest{TokenEndpoint: srv.URL, ClientID: "c"}
	set, err := newTokenClient(nil).Refresh(context.Background(), &man, "r")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !set.ExpiresAt.IsZero() {
		t.Fatalf("a year-long JWT expiry was adopted (%s) — a revocation would go unnoticed until then", set.ExpiresAt)
	}
}

// A cancelled caller must be able to leave the refresh queue.
func TestACancelledCallerDoesNotWaitOutTheRefreshQueue(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")

	p.hang = make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = m.AccessToken(context.Background()) // holds the singleflight
	}()
	<-started
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.AccessToken(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled caller succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled caller was stuck in the refresh queue")
	}
	close(p.hang)
}

// Opening the credential store must be exactly-once: two concurrent callers each
// creating a fallback MemoryStore would lose whichever session landed in the loser.
func TestTheCredentialStoreIsOpenedExactlyOnce(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, nil) // no preset store: force the real open path

	var wg sync.WaitGroup
	stores := make([]Store, 8)
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i] = m.ensureStore(context.Background())
		}(i)
	}
	wg.Wait()
	for i, s := range stores {
		if s != stores[0] {
			t.Fatalf("caller %d got a different store instance — a session saved in one would be invisible to the other", i)
		}
	}
}
