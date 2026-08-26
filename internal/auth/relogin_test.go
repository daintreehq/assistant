package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// relogin_test.go covers ONE window: the gap between a login persisting a new refresh
// token and that login publishing the identity the token belongs to.
//
// Login takes the cross-process credential lock to write the credential, and a revoked
// verdict takes the SAME lock before deleting one — rechecking, only after it has the
// lock, whether the identity it was told about is still current. So the ordering inside
// Login decides whether that recheck can be right. Publishing the new generation after
// releasing the lock left an interval in which a revocation for the OLD session, which
// had been blocked on the lock all along, woke up, compared itself against state the
// login had not yet replaced, concluded it was current, and deleted the credential the
// login had just written.
//
// What made it worth a test rather than an argument: every visible signal said it worked.
// Login returned success, `Persisted` was true, the card said "Signed in.", and the
// keychain entry was gone.

// browserOpener completes the callback the way a browser would: it reads the
// authorization URL, pulls the state out of it, and issues the redirect back to the
// loopback listener.
//
// This is what makes a REAL Login drivable in a test — the flow is otherwise parked for
// five minutes waiting for a person. It talks to the actual fixed callback address, so it
// exercises the listener, the state check and the exchange rather than stubbing past them.
type browserOpener struct{ t *testing.T }

func (b browserOpener) Open(ctx context.Context, authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	state := u.Query().Get("state")
	go func() {
		// A short retry: Login binds the listener before opening the browser, but the
		// goroutine scheduling between the two is not something to assume.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			req, rErr := http.NewRequestWithContext(ctx, http.MethodGet,
				RedirectURI()+"?code=auth-code&state="+url.QueryEscape(state), nil)
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

// loginWithPortRetry drives a real Login, retrying while the fixed callback port is held.
//
// The callback address is compiled in — Supabase matches redirect URIs exactly, so it
// cannot be varied per test — and `go test ./...` runs PACKAGES concurrently. Three
// packages now drive real sign-ins (internal/auth, internal/cli, internal/commands), so
// without this they contend for 127.0.0.1:42813 and whichever loses fails with a port
// collision that has nothing to do with what it was testing. Within a package Go
// serializes tests, so this only ever waits on a sibling package.
//
// Only CodeCallbackPortInUse is retried. Every other failure is the thing under test and
// is returned immediately.
func loginWithPortRetry(t *testing.T, m *Manager, progress LoginProgress) (LoginResult, error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := m.Login(context.Background(), true, progress, nil)
		if CodeOf(err) != CodeCallbackPortInUse || time.Now().After(deadline) {
			return res, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A re-login must survive a revocation aimed at the session it replaces.
//
// DETERMINISTIC, via the seam Login exposes right after it releases the credential lock —
// which is precisely the moment a clearer that had been blocked on that lock gets its
// turn. Under the wrong ordering the new generation is not published yet at that point,
// the clearer's staleness check compares against the identity the login replaced, finds
// it current, and deletes the credential the login has just written.
//
// A stress test was tried first and is not good enough: the window is a few nanoseconds
// and a tight loop of revocations never lands in it, so it passes against the bug.
func TestARevocationOfTheOldSessionCannotDeleteANewLogin(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	key := storedFor(t, m, p, store, "refresh-old")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	oldGen, oldToken := m.Generation(), currentToken(m)

	// The clearer takes its turn exactly where a real one would.
	var fired bool
	loginAfterCredentialUnlock = func() {
		fired = true
		// A verdict about the OLD session, arriving late — the shape of every real one:
		// a request that went out before the re-login and answered after it began.
		m.ApplyBackendVerdict(context.Background(), oldGen, oldToken,
			&backend.Error{Code: backend.CodeAuthSessionRevoked})
	}
	t.Cleanup(func() { loginAfterCredentialUnlock = nil })

	res, err := loginWithPortRetry(t, m, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !fired {
		t.Fatal("the seam never ran, so no clearer was exercised and this test proves nothing")
	}
	// Persistence is NOT asserted: it is derived from the storage TIER, and this suite
	// runs on a MemoryStore, where a session saves successfully and still evaporates on
	// exit. The credential's presence below is the fact under test either way — a
	// deletion is a deletion whichever tier it happened on.
	_ = res

	// THE assertion. The login said it stored a credential; one must actually be there.
	got, err := store.Load(context.Background(), key)
	if errors.Is(err, ErrNotFound) {
		t.Fatal("the credential this login persisted was deleted by a revocation aimed at the session it replaced — " +
			"Login must publish its new generation BEFORE releasing the credential lock, or a clearer " +
			"blocked on that lock wakes inside the window and finds the old identity still current")
	}
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken == "refresh-old" {
		t.Error("the stored credential is still the one the login replaced")
	}
	// And the session is usable rather than merely present.
	if st := m.Status(); !st.State.SignedIn() {
		t.Errorf("state = %q after a successful login", st.State)
	}
}

// The generation is visible to anything that takes the credential lock after the login
// has written through it.
//
// This is the invariant the test above depends on, stated directly: a clearer's whole
// correctness rests on the recheck it performs AFTER acquiring the lock, and that recheck
// can only be right if the winning writer published before releasing.
func TestALoginPublishesItsIdentityBeforeReleasingTheCredentialLock(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	key := storedFor(t, m, p, store, "refresh-old")
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	oldGen, oldToken := m.Generation(), currentToken(m)

	if _, err := loginWithPortRetry(t, m, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// A clearer arriving now holds the pre-login identity and must be refused.
	m.clearSessionIfCurrent(context.Background(), oldGen, oldToken)

	if _, err := store.Load(context.Background(), key); err != nil {
		t.Fatalf("a stale clearer deleted the new credential: %v", err)
	}
	if st := m.Status(); !st.State.SignedIn() {
		t.Errorf("a stale clearer signed the new session out: %q", st.State)
	}
}

// The authorization URL never reaches the progress stream.
//
// Progress goes to a structured event stream a caller may log, and to an embedding host's
// NDJSON frames. The URL carries a live authorization request bound to this attempt's
// state and challenge — the one string in this flow that is worth stealing while a login
// is open.
func TestTheAuthorizationURLNeverReachesProgress(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}
	_ = storedFor(t, m, p, store, "refresh-old")

	var mu sync.Mutex
	var seen []string
	progress := func(event, detail string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event+" "+detail)
	}

	if _, err := loginWithPortRetry(t, m, progress); err != nil {
		t.Fatalf("Login: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, line := range seen {
		for _, banned := range []string{"code_challenge", "response_type", "/oauth/authorize", "state="} {
			if strings.Contains(line, banned) {
				t.Errorf("a progress event carried the authorization request (%q): %s", banned, line)
			}
		}
		// Nor the credentials the flow produces.
		for _, banned := range []string{"refresh-", "access-"} {
			if strings.Contains(line, banned) {
				t.Errorf("a progress event carried a token (%q): %s", banned, line)
			}
		}
	}
}

// The CROSS-PROCESS variant, which the same-manager test above cannot see.
//
// A clearer in ANOTHER process holds its own generation and its own cached token, and
// neither moves when this process logs in — nothing local happened over there. So the
// local staleness check says "still current" and the delete destroys a credential
// belonging to a session that began after the verdict being acted on. The window is not
// nanoseconds either: it lasts until that other manager next calls AccessToken and adopts
// the new marker.
//
// The shared revision marker is the only thing that can tell it, which is why the clearer
// consults it after taking the lock rather than trusting its own numbers.
func TestALoginInOneProcessSurvivesAStaleClearerInAnother(t *testing.T) {
	p := newIDP(t)
	// ONE store and ONE state root, because that is what two processes on a machine
	// share: the credential store and the revision file are per user, at the state root.
	store := NewMemoryStore()
	root := t.TempDir()

	newAt := func(opener Opener) *Manager {
		t.Helper()
		m, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: opener})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return m
	}

	// The other process, signed in and holding its identity.
	other := newAt(NoOpener{})
	key := storedFor(t, other, p, store, "refresh-old")
	if _, err := other.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	otherGen, otherToken := other.Generation(), currentToken(other)

	// This process logs in. The clearer over there takes its turn at exactly the moment a
	// blocked one would — after the lock is released and the credential is committed.
	me := newAt(browserOpener{t: t})
	var fired bool
	loginAfterCredentialUnlock = func() {
		fired = true
		other.ApplyBackendVerdict(context.Background(), otherGen, otherToken,
			&backend.Error{Code: backend.CodeAuthSessionRevoked})
	}
	t.Cleanup(func() { loginAfterCredentialUnlock = nil })

	if _, err := loginWithPortRetry(t, me, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !fired {
		t.Fatal("the seam never ran, so no clearer was exercised and this test proves nothing")
	}

	got, err := store.Load(context.Background(), key)
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a stale clearer in ANOTHER process deleted the credential this login just stored — " +
			"a clearer's own generation cannot see a login that happened elsewhere, so the " +
			"post-lock recheck must also consult the shared revision marker")
	}
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken == "refresh-old" {
		t.Error("the stored credential is still the one the login replaced")
	}
	if st := me.Status(); !st.State.SignedIn() {
		t.Errorf("state = %q after a successful login", st.State)
	}
}
