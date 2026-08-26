package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// keyref_test.go covers the SECOND half of publishing a session: the non-secret
// descriptor that says which credential was stored.
//
// The credential and the descriptor are one publication, not a write and a courtesy.
// Login used to record the descriptor best-effort under a comment claiming it "only
// enables OFFLINE logout" — and that comment was the bug. The descriptor is also how a
// later process, and this one once its access token expires, discovers a credential
// exists at all: AccessToken short-circuits on its absence, resolveKey falls back to a
// network round trip, and Hydrate reads it before deciding whether an outage is a
// sign-out.
//
// What made the old shape hard to see was the DELAY. A login whose descriptor never
// landed reported success with Persisted: true, and the in-memory access token kept
// working for an hour. Only then did AccessToken conclude "never signed in", return an
// EMPTY bearer, and hand the request to the backend's open door — which serves it
// anonymously. Every visible signal said the user was signed in the whole time.

// --- rig ------------------------------------------------------------------------------

// persistentStore is a MemoryStore that REPORTS the keychain tier.
//
// The tier is what Login derives Persisted from, and it is the whole switch between
// "a failed descriptor write fails the login" and "a failed descriptor write is
// irrelevant". The suite otherwise runs entirely on memory stores, so without this there
// is no way to exercise the persistent branch at all — and that branch is the one that
// rolls a credential back.
type persistentStore struct{ *MemoryStore }

func newPersistentStore() persistentStore { return persistentStore{NewMemoryStore()} }

func (persistentStore) Tier(context.Context) StorageTier { return TierKeychain }

// deleteFailingStore is a persistent store whose Delete always fails, for the rollback's
// own failure path.
type deleteFailingStore struct {
	persistentStore
	err error
}

func (d *deleteFailingStore) Delete(context.Context, CredentialKey) error { return d.err }

// managerAt builds a Manager on a CHOSEN state root, so a second "process" can share one.
func managerAt(t *testing.T, p *idp, store Store, root string, now func() time.Time) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		StateRoot:  root,
		BackendURL: p.srv.URL,
		Store:      store,
		Opener:     browserOpener{t: t},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// blockDescriptorWrite makes saveKeyRef — and ONLY saveKeyRef — fail, by putting a
// directory where the descriptor file belongs. writeAtomic finishes with
// rename(tmp, credential.json), and renaming a regular file over an existing directory
// is refused by the kernel.
//
// The obvious mechanism, chmod 0500 on the auth directory, is WRONG here and quietly so.
// The revision marker lives in that same directory and is written by the same
// temp-and-rename, so an unwritable directory fails the revision bump as well — and the
// bump's rollback predates this change. A test built that way fails the login either way
// and would pass just as green against the best-effort descriptor write it is supposed to
// be catching. Verified: with the checked write reverted, this version fails and the
// chmod version does not.
//
// Nothing is faked. saveKeyRef takes a real error from a real syscall on the real path,
// which is what a read-only state root or a full disk would also produce; only the blast
// radius is narrowed to the one write under test.
func blockDescriptorWrite(t *testing.T, dir string) {
	t.Helper()
	path := keyRefPath(dir)
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("clear descriptor: %v", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("block descriptor path: %v", err)
	}
	// Confirm the block actually blocks, rather than trusting the platform. A silent
	// no-op here would make every assertion below meaningless.
	if err := saveKeyRef(dir, CredentialKey{Issuer: "probe", ClientID: "probe"}); err == nil {
		t.Skip("a descriptor write succeeded over a directory on this filesystem, so it cannot be made to fail")
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
}

// keyFor derives the credential key this manager's endpoint uses, for tests that need to
// name it before any login has recorded a descriptor.
func keyFor(t *testing.T, m *Manager, p *idp) CredentialKey {
	t.Helper()
	man, err := m.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return m.key(man)
}

// signIn drives a real end-to-end login, failing the test on anything but success.
func signIn(t *testing.T, m *Manager) LoginResult {
	t.Helper()
	res, err := loginWithPortRetry(t, m, nil)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return res
}

// descriptorExists reports whether a readable descriptor is on disk.
func descriptorExists(dir string) bool {
	_, ok := loadKeyRef(dir)
	return ok
}

// --- the persistent tier: a descriptor failure fails the login -------------------------

// THE test for row 2. A login that cannot record its descriptor must fail, and must not
// leave the credential it just stored behind.
//
// The rollback matters as much as the failure. Leaving the credential would make "login
// failed" false in the worst way: a later process loads a session this one has just told
// the user it could not create, and after a re-login as a different account the two
// disagree about who is signed in.
func TestALoginWhoseDescriptorCannotBePublishedFailsAndRollsBack(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()
	m := managerAt(t, p, store, root, nil)

	// A FIRST login, so there is no earlier session for the rollback to put back and the
	// credential it stored is simply removed. The re-login case — where a previous session
	// exists and must survive — is its own test below, because the two rollbacks end
	// somewhere deliberately different.
	key := keyFor(t, m, p)
	blockDescriptorWrite(t, m.AuthDirPath())

	res, err := loginWithPortRetry(t, m, nil)
	if err == nil {
		t.Fatal("a login whose descriptor could not be published reported SUCCESS — " +
			"the credential it stored is unaddressable, and once the access token expires " +
			"this session silently becomes an anonymous request the backend's open door accepts")
	}
	if got := CodeOf(err); got != CodeStorageUnavailable {
		t.Errorf("code = %q, want %q", got, CodeStorageUnavailable)
	}
	if res.Persisted {
		t.Error("Persisted = true on a failed login")
	}
	// Rolled back: nothing a later process could resolve is left behind.
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Errorf("the credential survived a failed login: %v — an orphaned keychain entry "+
			"outlives the failure the user was told about", err)
	}
	if m.State().SignedIn() {
		t.Errorf("state = %q after a login that failed", m.State())
	}
}

// A failed RE-login must not take the session it was replacing down with it.
//
// This is the commonest way the rollback is reached, because a user who signs in again
// already had a session. Save has overwritten the previous refresh token by the time the
// descriptor write fails, so a rollback that only DELETED left the machine with no
// credential at all: the sign-in failed, and the working session it was refreshing went
// with it, over a fault that had nothing to do with either credential. Nothing ever
// presented the old token, so putting it back is both possible and correct.
func TestAFailedReLoginKeepsTheSessionItWasReplacing(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()
	m := managerAt(t, p, store, root, nil)

	first := signIn(t, m)
	if !first.Persisted {
		t.Fatalf("Persisted = false on the keychain tier")
	}
	key, ok := loadKeyRef(m.AuthDirPath())
	if !ok {
		t.Fatal("an ordinary login recorded no descriptor")
	}
	before, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load after the first login: %v", err)
	}

	blockDescriptorWrite(t, m.AuthDirPath())
	if _, err := loginWithPortRetry(t, m, nil); err == nil {
		t.Fatal("the re-login reported success despite an unpublishable descriptor")
	}

	after, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("the previous session was destroyed by a failed re-login: %v — the sign-in "+
			"failed, and the session the user already had went with it", err)
	}
	if after.RefreshToken != before.RefreshToken {
		t.Errorf("the stored credential is neither the previous session nor absent")
	}
	// And the process still reports the session it actually holds.
	if !m.State().SignedIn() {
		t.Errorf("state = %q — the previous session is back in the store and usable", m.State())
	}
	if tok, tErr := m.AccessToken(context.Background()); tErr != nil || tok == "" {
		t.Errorf("the restored session could not be spent: tok=%q err=%v", tok, tErr)
	}
}

// The rollback's own failure must not be mistaken for success, and must not replace the
// cause with the cleanup's error.
//
// The cleanup error is deliberately discarded — exactly as the revision-bump rollback
// beside it does — because the failure being REPORTED is the descriptor write, and
// naming the delete instead would send the user after the wrong thing. What is left
// behind in that case (a credential with no descriptor) is genuinely unrecoverable from
// here; the one thing still in this function's gift is that "login failed" stays true.
func TestADescriptorRollbackWhoseDeleteFailsStillFailsTheLogin(t *testing.T) {
	p := newIDP(t)
	store := &deleteFailingStore{persistentStore: newPersistentStore(), err: errors.New("keychain is on fire")}
	root := t.TempDir()
	m := managerAt(t, p, store, root, nil)

	// A first login, so the rollback has nothing to restore and must fall through to the
	// delete that fails.
	blockDescriptorWrite(t, m.AuthDirPath())

	_, err := loginWithPortRetry(t, m, nil)
	if err == nil {
		t.Fatal("a login whose descriptor failed AND whose rollback failed reported success")
	}
	if got := CodeOf(err); got != CodeStorageUnavailable {
		t.Errorf("code = %q, want %q", got, CodeStorageUnavailable)
	}
	if errors.Is(err, store.err) {
		t.Error("the reported cause is the cleanup's error, not the descriptor write that actually failed")
	}
	if m.State().SignedIn() {
		t.Errorf("state = %q after a login that failed", m.State())
	}
}

// A successful persistent login is resolvable by a SECOND PROCESS — which is the whole
// meaning of Persisted: true, and the property the checked write exists to guarantee.
func TestAPersistedLoginIsResolvableByASecondProcess(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()

	first := managerAt(t, p, store, root, nil)
	if res := signIn(t, first); !res.Persisted {
		t.Fatal("Persisted = false on the keychain tier")
	}

	// A different Manager on the same state root and the same credential store: a
	// process restart, or a daemon starting beside the session that signed in. It shares
	// no memory with the first, so the descriptor is the ONLY thing that can tell it a
	// credential exists.
	second := managerAt(t, p, store, root, nil)
	if !second.Hydrate(context.Background()) {
		t.Fatal("a second process could not resolve a login reported as persisted")
	}
	if !second.State().SignedIn() {
		t.Errorf("second process state = %q, want a signed-in state", second.State())
	}
	tok, err := second.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("second process AccessToken: %v", err)
	}
	if tok == "" {
		t.Fatal("a second process sent an EMPTY bearer for a credential the first reported as persisted")
	}
}

// ...and a ROLLED-BACK login leaves that second process nothing to find. The negative of
// the test above, and the reason the rollback is not optional.
func TestARolledBackLoginLeavesNothingForASecondProcess(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()

	first := managerAt(t, p, store, root, nil)
	blockDescriptorWrite(t, first.AuthDirPath())
	if _, err := loginWithPortRetry(t, first, nil); err == nil {
		t.Fatal("the login reported success though its descriptor could not be written")
	}

	second := managerAt(t, p, store, root, nil)
	second.Hydrate(context.Background())
	if second.State().SignedIn() {
		t.Errorf("a second process reports %q after a login that FAILED — "+
			"the credential was not rolled back", second.State())
	}
	// NO BEARER is the whole assertion; the error beside it may legitimately be either
	// of two things, because this rig leaves the machine in a state a second process
	// cannot fully read.
	//
	// blockDescriptorWrite occupies credential.json's path with a DIRECTORY, so the
	// descriptor is not absent — it is unreadable. AccessToken now says so rather than
	// concluding "never signed in", which is the point of that distinction: an
	// unreadable descriptor must never resolve to an anonymous request. CodeNotSignedIn
	// is the answer once the path is genuinely clear. Either way nothing is issued.
	tok, err := second.AccessToken(context.Background())
	switch {
	case err == nil, CodeOf(err) == CodeNotSignedIn, CodeOf(err) == CodeStorageUnavailable:
	default:
		t.Fatalf("second process AccessToken: %v", err)
	}
	if tok != "" {
		t.Error("a second process holds a bearer for a login that failed")
	}
}

// --- the memory tier: honest, and still usable for this process ------------------------

// A descriptor failure on the MEMORY tier must NOT fail the login.
//
// The descriptor's durable purpose is moot when the credential itself is process-only:
// there is nothing for it to address, because the session dies with the process either
// way. Refusing a login over a file it does not need would break sign-in on exactly the
// machines that have the least — a headless box with no session bus, a locked store —
// and it would refuse a session that is about to work perfectly well.
func TestADescriptorFailureOnTheMemoryTierStillSignsIn(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore() // Tier() == TierMemory
	root := t.TempDir()
	m := managerAt(t, p, store, root, nil)

	blockDescriptorWrite(t, filepath.Join(root, authDirName))

	res, err := loginWithPortRetry(t, m, nil)
	if err != nil {
		t.Fatalf("a memory-tier login was refused over a descriptor it does not need: %v", err)
	}
	if res.Persisted {
		t.Error("Persisted = true on the memory tier — the session does not survive exit")
	}
	if res.Tier != TierMemory {
		t.Errorf("Tier = %q, want %q", res.Tier, TierMemory)
	}
	// The honest report: this process only, not a sign-out and not an outage.
	if got := m.State(); got != StateStorageUnavailable {
		t.Errorf("state = %q, want %q so status can say this-process-only", got, StateStorageUnavailable)
	}
	if descriptorExists(m.AuthDirPath()) {
		t.Error("a descriptor was read back from a path the write could not reach")
	}
}

// THE behavioural half of the fix, and the one the old code got wrong.
//
// A memory-tier session has no descriptor by construction. When its access token
// expires, AccessToken re-runs the "has this machine ever signed in?" short-circuit — and
// that used to consult the descriptor alone, find none, rewrite the state to signed-out
// and return an EMPTY bearer with a nil error. The caller sends an anonymous request, the
// backend's open door accepts it, and the turn runs under the wrong principal with
// nothing anywhere reporting a problem.
//
// The remembered key is what makes the answer right: a process that performed the login
// itself knows exactly which credential it holds, and a missing FILE is evidence about
// other processes, never about this one.
func TestAMemoryTierSessionSurvivesItsAccessTokenExpiring(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	root := t.TempDir()

	clock := time.Now()
	m := managerAt(t, p, store, root, func() time.Time { return clock })

	signIn(t, m)
	// No descriptor at all — deleted rather than merely absent, which is also what a
	// `logout` in ANOTHER process leaves behind for this one.
	if err := forgetKeyRef(m.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}

	before, err := m.AccessToken(context.Background())
	if err != nil || before == "" {
		t.Fatalf("AccessToken before expiry = %q, %v", before, err)
	}

	// Past the token's hour, so the cached one is unusable and a refresh is required.
	clock = clock.Add(2 * time.Hour)

	after, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after expiry: %v", err)
	}
	if after == "" {
		t.Fatal("a signed-in process sent an EMPTY bearer once its access token expired — " +
			"the descriptor is missing, but THIS process performed the login and knows which " +
			"credential it holds; the request would be served anonymously under the wrong principal")
	}
	if after == before {
		t.Error("the expired access token was returned unchanged")
	}
	if !m.State().SignedIn() {
		t.Errorf("state = %q after a refresh that succeeded", m.State())
	}
}

// The same claim from the other side: with no descriptor and no login in this process,
// the short-circuit still answers cheaply and makes NO network call. The remembered key
// must not have turned a fast signed-out path into a discovery round trip on the install
// shape that is every install today.
func TestNoLoginInThisProcessStillShortCircuitsWithoutNetwork(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	p.srv.Close() // any network call now fails loudly

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("a signed-out process errored instead of sending no header: %v", err)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty for a process that never signed in", tok)
	}
}

// --- the remembered key belongs to ONE identity ----------------------------------------

// Logout ends the session in this process too. The remembered key must go with it, or it
// keeps telling AccessToken "this process signed in" for a session the user has ended —
// the descriptor's own failure mode, reproduced in memory where no other process could
// correct it.
func TestLogoutForgetsTheKeyRememberedByThisProcess(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	signIn(t, m)
	if _, ok := m.rememberedKey(); !ok {
		t.Fatal("a completed login remembered no key")
	}

	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, ok := m.rememberedKey(); ok {
		t.Fatal("the remembered key outlived the logout that deleted the credential it names")
	}
	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after logout: %v", err)
	}
	if tok != "" {
		t.Error("a bearer was still produced after logout")
	}
	if m.State() != StateSignedOut {
		t.Errorf("state = %q after logout, want %q", m.State(), StateSignedOut)
	}
}

// A revocation is the other identity boundary that deletes the credential, and it must
// forget the key for the same reason.
func TestARevocationForgetsTheKeyRememberedByThisProcess(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	signIn(t, m)
	if _, ok := m.rememberedKey(); !ok {
		t.Fatal("a completed login remembered no key")
	}

	m.ApplyBackendVerdict(context.Background(), m.Generation(), currentToken(m),
		&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthSessionRevoked})

	if m.State() != StateRevoked {
		t.Fatalf("state = %q after a revocation, want %q", m.State(), StateRevoked)
	}
	if _, ok := m.rememberedKey(); ok {
		t.Error("the remembered key outlived the revocation that deleted the credential it names")
	}
}

// A logout in ANOTHER process must reach this one even when its delete failed.
//
// The revision bump is the signal, and Logout emits it whether or not the credential could
// actually be removed — precisely so every other process stops spending when it could not.
// A manager that kept the key it remembered would be the one process on the machine that
// ignored it: past the short-circuit, straight back to the credential the delete could not
// remove, refreshed, and carrying on with the session the user asked to end.
func TestABumpElsewhereRetiresTheKeyRememberedHere(t *testing.T) {
	p := newIDP(t)
	// A store that refuses deletion, so the credential SURVIVES the other process's
	// logout. Without that, this process would settle to signed out through the store's
	// own answer and the test would prove nothing about the marker.
	store := &deleteFailingStore{persistentStore: newPersistentStore(), err: errors.New("keychain is on fire")}
	root := t.TempDir()

	me := managerAt(t, p, store, root, nil)
	signIn(t, me)
	if _, ok := me.rememberedKey(); !ok {
		t.Fatal("a completed login remembered no key")
	}

	// The other process signs the machine out. Its delete fails; its bump does not.
	other, err := NewManager(Options{StateRoot: root, BackendURL: p.srv.URL, Store: store, Opener: NoOpener{}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, lErr := other.Logout(context.Background()); lErr == nil {
		t.Fatal("the rig's delete was supposed to fail, so this test is not testing what it claims")
	}

	tok, err := me.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken after a logout elsewhere: %v", err)
	}
	if tok != "" {
		t.Fatal("this process kept spending after a logout elsewhere — a remembered key must " +
			"not survive an identity change it did not make")
	}
	if _, ok := me.rememberedKey(); ok {
		t.Error("the remembered key outlived the bump that announced the identity had changed")
	}
}

// The store's own "there is nothing here" is the answer the remembered key was deferring
// to, so it retires there.
//
// Holding it past an authoritative absence preserves no session — there is none. It only
// makes every later request skip the cheap short-circuit and pay a lock, a store read and
// a periodic discovery to be told the same thing again.
func TestAnAuthoritativeAbsenceRetiresTheRememberedKey(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()
	m := managerAt(t, p, store, root, nil)

	signIn(t, m)
	key, ok := m.rememberedKey()
	if !ok {
		t.Fatal("a completed login remembered no key")
	}
	// The credential goes, with no bump and no descriptor left behind — a state dir
	// cleared out from under a running process.
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := forgetKeyRef(m.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}
	m.Invalidate(currentToken(m))

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "" {
		t.Fatalf("token = %q for a credential that is not there", tok)
	}
	if _, ok := m.rememberedKey(); ok {
		t.Error("the remembered key outlived the store's own answer that the credential is gone")
	}
	if m.State() != StateSignedOut {
		t.Errorf("state = %q, want %q", m.State(), StateSignedOut)
	}
}

// The remembered key is origin-checked exactly as the descriptor is. A key minted for one
// deployment must never name the credential for another: `auth status` for B would report
// A's session, and `auth logout` for B would DELETE A's credential.
func TestARememberedKeyIsIgnoredForAnotherEndpoint(t *testing.T) {
	p := newIDP(t)
	m := newManager(t, p, NewMemoryStore())
	m.opener = browserOpener{t: t}

	signIn(t, m)
	key, ok := m.rememberedKey()
	if !ok {
		t.Fatal("a completed login remembered no key")
	}

	// The manager is repointed, as a `/backend` switch does. In production the whole
	// manager is rebuilt, which is why this cannot fire today — and exactly why the check
	// is cheap to keep against the day that stops being true.
	m.backendURL = "https://elsewhere.example"
	if got, ok := m.rememberedKey(); ok {
		t.Errorf("a key for %q was offered for a different endpoint: %v", key.BackendOrigin, got)
	}
	// And it does not leak through the resolver either. resolveKey may still resolve a
	// key here — deriving this endpoint's own from discovery is its documented fallback —
	// but it must never be the one the previous endpoint's login remembered.
	got, resolution := m.resolveKey(context.Background())
	if resolution == keyResolved && got == key {
		t.Error("resolveKey handed back the credential belonging to the endpoint the manager was pointed away from")
	}
}

// The remembered key is what lets a memory-tier process sign ITSELF out. Without it,
// Logout has to name the credential through the descriptor or the network — and on this
// tier there may be neither, leaving the process holding a live session it cannot end.
func TestAMemoryTierProcessCanLogItselfOutOffline(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	m.opener = browserOpener{t: t}

	signIn(t, m)
	key, ok := m.rememberedKey()
	if !ok {
		t.Fatal("a completed login remembered no key")
	}
	if err := forgetKeyRef(m.AuthDirPath()); err != nil {
		t.Fatalf("forgetKeyRef: %v", err)
	}
	p.srv.Close()
	m.discoverer.Invalidate()

	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("offline Logout with no descriptor: %v", err)
	}
	if _, err := store.Load(context.Background(), key); !errors.Is(err, ErrNotFound) {
		t.Errorf("the credential survived a logout that could not name it: %v", err)
	}
}

// --- no secret ever reaches the descriptor ---------------------------------------------

// The descriptor is the ADDRESS of a credential, never the credential. A refresh token
// reaching this file would put the one persisted secret on disk in plaintext, outside the
// OS credential store that is the only place it is allowed to live.
func TestTheDescriptorCarriesNoSecret(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	m := managerAt(t, p, store, t.TempDir(), nil)

	signIn(t, m)

	key, ok := loadKeyRef(m.AuthDirPath())
	if !ok {
		t.Fatal("no descriptor was recorded")
	}
	raw, err := os.ReadFile(keyRefPath(m.AuthDirPath()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	stored, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken == "" {
		t.Fatal("the login stored no refresh token, so this test proves nothing")
	}
	for _, secret := range []string{stored.RefreshToken, currentToken(m)} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatal("a token was written into the credential descriptor — the refresh token " +
				"is the one persisted secret and the OS credential store is the only place it may go")
		}
	}
	// 0600 for consistency with the rest of the state root, not because it is sensitive.
	info, err := os.Stat(keyRefPath(m.AuthDirPath()))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("descriptor mode = %o, want 600", perm)
	}
}

// --- an unreadable auth directory is a fault, never a sign-out --------------------------

// The descriptor's absence is a definitive local "no login". Its UNREADABILITY is not,
// and collapsing the two is how a permissions accident silently re-bills every turn.
//
// A fresh process cannot fall back on the remembered key — it never performed the login —
// so it has only the files, and both of them are in the directory that has gone bad. The
// old code read the unreadable descriptor as "never signed in", rewrote the state to
// signed out, and returned an empty bearer with a NIL error. The backend's open door
// accepts that, so the turn succeeded as the anonymous principal and nothing anywhere
// reported a problem. Failing loudly is the only safe answer to "I cannot tell".
func TestAnUnreadableDescriptorFailsRatherThanGoingAnonymous(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()

	first := managerAt(t, p, store, root, nil)
	if _, err := loginWithPortRetry(t, first, nil); err != nil {
		t.Fatalf("setup login: %v", err)
	}

	// A second process, on the same state root, with the descriptor now unreadable.
	second := managerAt(t, p, store, root, nil)
	blockDescriptorWrite(t, second.AuthDirPath())

	tok, err := second.AccessToken(context.Background())
	if tok != "" {
		t.Fatalf("a bearer was issued from an unreadable state root: %q", tok)
	}
	if err == nil {
		t.Fatal("an unreadable descriptor produced an empty bearer and NO error — " +
			"the request would have gone out anonymously under the wrong principal")
	}
	if got := CodeOf(err); got != CodeStorageUnavailable {
		t.Errorf("code = %q, want %q — the fault must name storage, not a sign-out", got, CodeStorageUnavailable)
	}
}

// An unreadable REVISION marker is the same fault by another route.
//
// Current() folded every read failure into the zero Marker, which compares unequal to any
// real observation — so a process that had observed a bump saw an unreadable directory as
// somebody else ending the identity. It dropped the access token AND the remembered key,
// and the unreadable descriptor beside it then answered "no login here". The session this
// process had performed itself evaporated into an anonymous request.
func TestAnUnreadableRevisionMarkerIsNotAnIdentityChange(t *testing.T) {
	p := newIDP(t)
	store := newPersistentStore()
	root := t.TempDir()

	m := managerAt(t, p, store, root, nil)
	if _, err := loginWithPortRetry(t, m, nil); err != nil {
		t.Fatalf("setup login: %v", err)
	}
	if !m.State().SignedIn() {
		t.Fatalf("setup: state = %q", m.State())
	}

	// Make the marker unreadable the way a permissions fault would: replace it with a
	// directory, so it EXISTS and cannot be read.
	marker := m.Revision().Path()
	if err := os.RemoveAll(marker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatalf("block marker: %v", err)
	}
	if _, ok := m.Revision().CurrentReadable(); ok {
		t.Skip("the marker could still be read over a directory on this filesystem")
	}

	// THE GENERATION IS THE ASSERTION, because it is what an identity change moves and
	// what an unreadable file must not.
	//
	// Watching only the bearer would pass either way here: the descriptor is still
	// readable in this test, so even the old code found its way back to the credential
	// after wrongly discarding the session. What it could not hide is the bump — and the
	// bump is the damage, since it retires the remembered key and makes every request
	// already in flight stale, on the strength of a file it merely failed to open.
	before := m.Generation()
	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok == "" {
		t.Fatal("an unreadable marker emptied the bearer — the turn would have gone out anonymously")
	}
	if got := m.Generation(); got != before {
		t.Errorf("generation moved %d -> %d — an unreadable coordination file was read as somebody else ending the session", before, got)
	}
	if !m.State().SignedIn() {
		t.Errorf("state = %q after an unreadable marker, want the session intact", m.State())
	}
}
