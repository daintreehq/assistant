package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// keyref_test.go covers ONE thing: the credential and its non-secret descriptor are a
// single publication, and a login that cannot complete both of them is not a login.
//
// The failure it defends against is silent and delayed, which is why it needs pinning
// rather than reading. A descriptor write that failed used to be discarded, so Login
// returned success with `Persisted: true` while nothing on the machine recorded which
// credential had been stored. The in-memory access token then went on working for its
// full hour, and only when it expired — or when the process restarted, or another process
// asked — did AccessToken's "has this machine ever signed in?" short-circuit answer no,
// return an EMPTY bearer, and hand the backend's open door a turn to run under the
// anonymous principal. Every visible signal said the user was signed in throughout.

// --- rig ------------------------------------------------------------------------------

// keychainStore is a MemoryStore that claims the PERSISTENT tier.
//
// The tier is what Login derives `Persisted` from, and it is the tier — not the store
// implementation — that decides whether a failed descriptor write is fatal. So the tests
// below need a store that saves reliably (a real keychain is not available in a test) and
// reports itself as durable, which is exactly this.
type keychainStore struct{ *MemoryStore }

func (keychainStore) Tier(context.Context) StorageTier { return TierKeychain }

// undeletableStore is a persistent-tier store whose Delete always fails, standing in for
// a keychain that accepts a write and then refuses to give it back.
type undeletableStore struct{ *MemoryStore }

func (undeletableStore) Tier(context.Context) StorageTier { return TierKeychain }
func (undeletableStore) Delete(context.Context, CredentialKey) error {
	return ErrStoreLocked
}

// keyOf resolves the credential key this manager's endpoint would use.
func keyOf(t *testing.T, m *Manager) CredentialKey {
	t.Helper()
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return m.key(man)
}

// blockDescriptorWrite makes exactly ONE write in the auth directory fail: the
// descriptor's. It puts a non-empty DIRECTORY where credential.json belongs, so
// writeAtomic creates its temp file happily and then cannot rename over the obstacle.
//
// No seam, and no mocked filesystem — this is a real condition writeAtomic really fails
// on. It is deliberately narrower than the obvious alternative of chmodding the whole auth
// directory to 0500, and the difference is the entire point of the test. An unwritable
// directory also fails the credential lock and the revision bump, and the bump's rollback
// already covered that case — so a login sealed that way fails whether or not the
// descriptor write is checked, and the test would pass against the bug. Failing only the
// descriptor is what isolates the step under test.
//
// The directory is left non-empty so that a stray forgetKeyRef cannot quietly remove the
// obstacle mid-test: os.Remove succeeds on an empty directory.
func blockDescriptorWrite(t *testing.T, m *Manager) {
	t.Helper()
	obstacle := filepath.Join(m.AuthDirPath(), keyRefFileName)
	if err := os.MkdirAll(filepath.Join(obstacle, "occupied"), 0o700); err != nil {
		t.Fatalf("place the descriptor obstacle: %v", err)
	}
	if err := saveKeyRef(m.AuthDirPath(), CredentialKey{Issuer: "x", ClientID: "y"}); err == nil {
		t.Skip("this filesystem allows a file to be renamed over a non-empty directory, so a " +
			"descriptor-only write failure cannot be simulated")
	}
}

// --- (a) the descriptor write is a checked step ----------------------------------------

// A login that stores a credential it cannot address must FAIL, and must leave nothing
// behind.
//
// The alternative — the old behaviour — is an orphan: a refresh token in the OS credential
// store that no process can name, under a login that reported success. A fresh process
// finds no descriptor, concludes nobody has signed in, and goes anonymous; the entry
// itself survives every logout, because logout deletes by a key it can no longer derive.
func TestALoginFailsWhenTheDescriptorCannotBePublished(t *testing.T) {
	p := newIDP(t)
	store := keychainStore{NewMemoryStore()}
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	key := keyOf(t, m)
	blockDescriptorWrite(t, m)

	res, err := loginWithPortRetry(t, m, nil)
	if err == nil {
		t.Fatal("a login that could not record its credential descriptor reported success — " +
			"the credential it stored would be unaddressable by every later process")
	}
	if got := CodeOf(err); got != CodeStorageUnavailable {
		t.Errorf("code = %q, want %q — a descriptor that cannot be written is a storage failure", got, CodeStorageUnavailable)
	}
	if res.Persisted {
		t.Error("Persisted is true on a failed login")
	}
	// THE rollback. Nothing may be left in the store under a login that failed.
	if _, lErr := store.Load(context.Background(), key); !errors.Is(lErr, ErrNotFound) {
		t.Fatalf("the credential survived a failed login: %v — an orphaned refresh token no logout can reach", lErr)
	}
	if st := m.Status(); st.State.SignedIn() {
		t.Errorf("state = %q after a failed login", st.State)
	}
	// And the process does not go on believing it holds a session.
	tok, tErr := m.AccessToken(context.Background())
	if tok != "" {
		t.Error("a failed login left a spendable access token behind")
	}
	if tErr != nil && CodeOf(tErr) != CodeNotSignedIn {
		t.Errorf("AccessToken after a failed login: %v", tErr)
	}
}

// The rollback is best effort, and its own failure must not rename the fault.
//
// A store that refuses the delete leaves the orphan there regardless — there is nothing
// further this process can do about it. What is still in its gift is what the user is
// told, and telling them "the credential store is locked" would send them to unlock a
// keychain that answered every write perfectly well. The reported cause stays the
// descriptor.
func TestARollbackThatCannotDeleteStillReportsTheRealCause(t *testing.T) {
	p := newIDP(t)
	store := undeletableStore{NewMemoryStore()}
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	blockDescriptorWrite(t, m)

	_, err := loginWithPortRetry(t, m, nil)
	if err == nil {
		t.Fatal("the login reported success despite an unpublishable descriptor")
	}
	if got := CodeOf(err); got != CodeStorageUnavailable {
		t.Errorf("code = %q, want %q", got, CodeStorageUnavailable)
	}
	if st := m.Status(); st.State.SignedIn() {
		t.Errorf("state = %q after a failed login", st.State)
	}
}

// `Persisted: true` is a promise that a NEW process can resolve the credential, so the
// happy path has to actually deliver it.
func TestAPersistedLoginIsResolvableByAnotherProcess(t *testing.T) {
	p := newIDP(t)
	// ONE store and ONE state root: what two processes on a machine share.
	store := keychainStore{NewMemoryStore()}
	root := t.TempDir()
	mk := func(o Opener) *Manager {
		t.Helper()
		mgr, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: o})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return mgr
	}

	me := mk(browserOpener{t: t})
	res, err := loginWithPortRetry(t, me, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !res.Persisted {
		t.Fatalf("Persisted is false on the %q tier", res.Tier)
	}
	if _, ok := loadKeyRef(me.AuthDirPath()); !ok {
		t.Fatal("Persisted was reported true with no descriptor on disk")
	}

	// The second process — a daemon, or the next launch — starts knowing nothing.
	other := mk(NoOpener{})
	if !other.Hydrate(context.Background()) {
		t.Fatal("a fresh process could not resolve the credential a persisted login stored")
	}
	if st := other.Status(); !st.State.SignedIn() {
		t.Fatalf("the second process reports %q for a persisted login", st.State)
	}
	if tok, tErr := other.AccessToken(context.Background()); tErr != nil || tok == "" {
		t.Fatalf("the second process could not spend the session: tok=%q err=%v", tok, tErr)
	}
}

// --- (b) the process remembers the credential it published ------------------------------

// The memory tier, honestly: a login that cannot persist still has to work for the
// lifetime of the process that made it.
//
// The descriptor is removed here to represent what that tier looks like in the general
// case — no credential service, and no guarantee of durable anything beside it. Before
// the in-memory key, the first access-token expiry after such a login turned into an
// empty bearer: the short-circuit read the missing file, answered "never signed in", and
// the backend's open door served the next turn anonymously.
func TestAMemoryTierSessionKeepsRefreshingWithoutADescriptor(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore() // TierMemory
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	res, err := loginWithPortRetry(t, m, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.Persisted {
		t.Fatalf("Persisted is true on the %q tier — the session dies with this process", res.Tier)
	}
	if st := m.Status(); st.State != StateStorageUnavailable {
		t.Fatalf("state = %q, want %q — status must say this-process-only", st.State, StateStorageUnavailable)
	}

	if err := forgetKeyRef(m.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}
	// The access token expires (or is rejected). Everything after this depends on the
	// process knowing which credential it holds.
	m.Invalidate(currentToken(m))

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after expiry on the memory tier: %v", err)
	}
	if tok == "" {
		t.Fatal("a memory-tier session went anonymous after its access token expired — " +
			"the process performed the login and knows which credential it holds; a missing " +
			"descriptor is not evidence that it holds none")
	}
	if st := m.Status(); !st.State.SignedIn() {
		t.Errorf("state = %q after a successful refresh", st.State)
	}
}

// The same guarantee on the persistent tier, for a descriptor that disappears after the
// fact — a partially cleared state dir, a stray delete.
//
// This is the residual window the checked write cannot cover, and it closes it: this
// process performed the login, so it never has to consult the file to know it is signed
// in. A DIFFERENT process legitimately does, which the next test pins.
func TestAnExpiredTokenDoesNotGoAnonymousAfterTheDescriptorDisappears(t *testing.T) {
	p := newIDP(t)
	store := keychainStore{NewMemoryStore()}
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	if _, err := loginWithPortRetry(t, m, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := forgetKeyRef(m.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}
	m.Invalidate(currentToken(m))

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok == "" {
		t.Fatal("the session went anonymous because a file it does not need was missing")
	}
}

// The complement, and the reason the descriptor still has to be written: a process that
// did NOT perform the login has nothing but the file, and correctly reports no session
// when it is gone.
//
// Stated as its own test because it is what makes the one above safe rather than lax —
// the in-memory key is scoped to the process that earned it, and does not soften the
// answer for anybody else.
func TestAnotherProcessStillNeedsTheDescriptor(t *testing.T) {
	p := newIDP(t)
	store := keychainStore{NewMemoryStore()}
	root := t.TempDir()
	mk := func(o Opener) *Manager {
		t.Helper()
		mgr, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: o})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return mgr
	}
	me := mk(browserOpener{t: t})
	if _, err := loginWithPortRetry(t, me, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := forgetKeyRef(me.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}

	other := mk(NoOpener{})
	tok, err := other.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "" {
		t.Fatal("a process that never signed in produced a bearer from a credential it cannot name")
	}
}

// A logout in this process drops the remembered key with the credential.
//
// Kept, it would reproduce the descriptor's own failure mode in memory, where no other
// process could correct it: AccessToken would keep answering "this process signed in" for
// a session the user has deliberately ended.
func TestLogoutForgetsTheRememberedCredential(t *testing.T) {
	p := newIDP(t)
	store := keychainStore{NewMemoryStore()}
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	key := keyOf(t, m)
	if _, err := loginWithPortRetry(t, m, nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, ok := m.rememberedKey(); ok {
		t.Fatal("the remembered credential outlived the logout that deleted it")
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the credential survived logout: %v", err)
	}
	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after logout: %v", err)
	}
	if tok != "" {
		t.Fatal("a logged-out manager still produced a bearer")
	}
}

// The open door is unchanged for an install that has never signed in — which is every
// install today.
//
// The short-circuit exists so that path costs one stat rather than a manifest fetch per
// request, and returns an empty bearer rather than an error, because the BACKEND decides
// whether anonymous access is allowed. Consulting the in-memory key first must not have
// disturbed either half.
func TestANeverSignedInProcessStillSendsNoBearer(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken on a fresh install: %v — the request would abort locally against a "+
			"backend perfectly willing to serve it anonymously", err)
	}
	if tok != "" {
		t.Fatalf("token = %q with no login anywhere", tok)
	}
	if _, err := os.Stat(filepath.Join(m.AuthDirPath(), keyRefFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a descriptor exists with no login: %v", err)
	}
}
