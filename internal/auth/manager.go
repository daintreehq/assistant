package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/redact"
)

// manager.go is the public orchestration: login, refresh, logout, status.
//
// Everything a caller needs goes through a Manager, and there is exactly one per
// process. The invariants it enforces are the ones no individual file below it can:
//
//   - A refresh is serialized IN-PROCESS (singleflight) and ACROSS PROCESSES (the file
//     lock). Supabase refresh tokens rotate and are one-time use, so two concurrent
//     refreshes mean one of them presents a token that has just been consumed.
//   - The stored token is RE-READ after the lock is taken, never before. Another process
//     may have rotated it while this one was waiting, and using the value read earlier
//     is precisely the reuse the lock exists to prevent.
//   - The revision is bumped only AFTER a credential write succeeds. Bumping first would
//     tell every other process to discard a working token in favour of one that then
//     failed to be written.

// Manager owns the account credential for this process.
type Manager struct {
	discoverer *Discoverer
	tokens     *tokenClient
	store      Store
	revision   *Revision
	authDir    string
	stateRoot  string
	backendURL string
	opener     Opener
	now        func() time.Time

	// mu guards everything below it. It is NOT held across network calls — those happen
	// under the cross-process file lock instead, so a slow provider cannot block a
	// status read.
	mu      sync.Mutex
	state   State
	access  TokenSet
	lastErr error
	tier    StorageTier
	// generation rises on every LOCAL identity change (login, logout, revocation).
	//
	// It exists because backend verdicts arrive late. A request made with token A can
	// return its answer after the user has logged out and signed back in with token B,
	// and an unconditional "revoked" verdict would then delete B's perfectly good
	// session — or an unconditional MarkActive would resurrect a signed-in state after a
	// logout. Callers capture the generation when they start a request and hand it back
	// with the verdict, so a stale answer can be recognised and dropped.
	generation uint64

	// storeOnce makes credential-store opening exactly-once.
	storeOnce sync.Once
	// refreshing is the in-process singleflight for refreshes, as a 1-buffered channel so
	// waiting on it can honour a context. The file lock alone would serialize refreshes,
	// but every waiter would then perform its own redundant one on release, burning a
	// rotating token per goroutine.
	refreshing chan struct{}
	// loggingIn makes login single-attempt per process, so two goroutines cannot open
	// competing browser flows against one fixed callback port.
	loggingIn sync.Mutex
	inLogin   bool
}

// Options configure a Manager.
type Options struct {
	// StateRoot is the PER-USER state root — never the per-project state dir. An account
	// is a property of the person, and a project-scoped lock would let two projects
	// rotate the same token concurrently.
	StateRoot string
	// BackendURL is the endpoint whose manifest describes the identity provider.
	BackendURL string
	// Store persists the refresh token. Nil opens the platform store, degrading to
	// memory (with the reason recorded) when there is none.
	Store Store
	// Opener launches the browser. Nil selects the system opener.
	Opener Opener
	// HTTPClient is used for discovery and token calls. Nil selects bounded defaults.
	HTTPClient *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// NewManager builds a Manager. It performs no I/O beyond creating the auth directory:
// discovery and credential reads are deferred, so constructing one on a machine that is
// offline or signed out cannot fail or block a launch.
func NewManager(opts Options) (*Manager, error) {
	dir, err := AuthDir(opts.StateRoot)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	opener := opts.Opener
	if opener == nil {
		opener = SystemOpener()
	}
	m := &Manager{
		discoverer: NewDiscoverer(opts.BackendURL, opts.HTTPClient),
		tokens:     newTokenClient(opts.HTTPClient),
		store:      opts.Store,
		revision:   NewRevision(dir),
		authDir:    dir,
		stateRoot:  opts.StateRoot,
		backendURL: opts.BackendURL,
		opener:     opener,
		now:        now,
		refreshing: make(chan struct{}, 1),
		state:      StateUnknown,
		tier:       TierUnavailable,
	}
	m.tokens.now = now
	return m, nil
}

// ensureStore opens the credential store on first use.
//
// Deferred rather than done in the constructor because opening it can prompt (a locked
// keychain) and can fail, and neither should happen during process startup for a user
// who is not signed in and never asks about their account.
func (m *Manager) ensureStore(ctx context.Context) Store {
	// sync.Once, not a nil check. A nil check lets two concurrent callers each open a
	// store and each write m.store — and when the fallback is a MemoryStore, the loser's
	// instance is the one holding the session just saved into it. The credential then
	// silently vanishes for every later caller.
	m.storeOnce.Do(func() {
		m.mu.Lock()
		preset := m.store
		m.mu.Unlock()
		if preset != nil {
			m.mu.Lock()
			if m.tier == TierUnavailable {
				m.tier = preset.Tier(ctx)
			}
			m.mu.Unlock()
			return
		}
		store, tier, err := OpenStore(ctx)
		m.mu.Lock()
		m.store, m.tier = store, tier
		if err != nil {
			// Recorded, never swallowed: a memory-tier session works and then evaporates,
			// and a user who was not told experiences that as being randomly forgotten.
			m.lastErr = err
			if m.state == StateUnknown {
				m.state = StateStorageUnavailable
			}
		}
		m.mu.Unlock()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store
}

// fallBackToMemory swaps in an in-process store after the real one refused a write.
//
// Without this, a Save that fails with ErrStoreUnavailable leaves the manager holding a
// store that will also fail every later Load — so the "session lasts for this process"
// promise made to the user quietly expires with the access token instead. Swapping keeps
// that promise honestly.
func (m *Manager) fallBackToMemory() Store {
	mem := NewMemoryStore()
	m.mu.Lock()
	m.store, m.tier = mem, TierMemory
	m.state = StateStorageUnavailable
	m.mu.Unlock()
	return mem
}

// State returns the current local account state without performing I/O.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// setState records a state transition.
func (m *Manager) setState(s State) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

// Manifest returns the validated auth manifest for the configured backend.
func (m *Manager) Manifest(ctx context.Context) (*Manifest, error) {
	man, err := m.discoverer.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	return man, nil
}

// key builds the credential key for the current backend and manifest.
func (m *Manager) key(man *Manifest) CredentialKey {
	return KeyFor(m.stateRoot, m.backendURL, man)
}

// AccessToken implements backend.TokenSource.
//
// This is the hot path: every protected backend request calls it. It must be cheap when
// nothing has changed, and it must never hand back a token another process has
// invalidated.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	// The identity marker is the cross-process invalidation signal, and it comes first
	// because it is the only thing that can tell this process a logout happened
	// elsewhere. It is one stat-and-read of a tiny file.
	//
	// The marker is sampled ONCE and that same value is both acted on and adopted.
	// Reading it twice — compare, then adopt whatever Current() says now — would let a
	// bump that landed between the two reads be adopted without ever being acted on, so
	// the logout it represented would never be noticed.
	if marker := m.revision.Current(); marker != m.revision.Observed() {
		m.mu.Lock()
		m.access = TokenSet{} // whatever we cached predates an identity change
		m.mu.Unlock()
		m.revision.MarkObserved(marker)
	}

	m.mu.Lock()
	cur := m.access
	m.mu.Unlock()

	if cur.AccessToken != "" && !cur.NeedsRefresh(m.now()) {
		return cur.AccessToken, nil
	}

	// Has this machine EVER signed in? The descriptor is written at login and removed at
	// logout, so its absence is a definitive local "no" — and answering it here costs one
	// stat instead of a discovery round trip.
	//
	// Without this short-circuit the signed-out path — which is every install today —
	// would try to fetch the auth manifest before every single request, fail (an older
	// backend does not serve one), and abort the call. A user who has never signed in
	// would find the assistant unable to reach a backend that was perfectly willing to
	// serve them anonymously.
	if _, everSignedIn := loadKeyRef(m.authDir); !everSignedIn {
		m.mu.Lock()
		if m.state == StateUnknown {
			m.state = StateSignedOut
		}
		m.mu.Unlock()
		return "", nil
	}

	set, err := m.refresh(ctx)
	if err != nil {
		// NOT SIGNED IN IS NOT AN ERROR HERE. It means "send no Authorization header",
		// which is exactly what the backend's open door expects today and what every
		// existing install does.
		//
		// Returning an error instead would abort the request in setHeaders — so on a
		// machine that has simply never signed in, every protected call would fail
		// locally and never reach the backend at all. The backend is the authority on
		// whether anonymous access is allowed; when it stops allowing it, it answers 401
		// with an account code and the retry ladder handles that. Deciding here would
		// take that call away from it.
		//
		// A real fault — a locked keychain, a refresh that could not complete — still
		// fails loudly, because proceeding anonymously there would silently downgrade a
		// session the user believes they have.
		if CodeOf(err) == CodeNotSignedIn {
			return "", nil
		}
		return "", err
	}
	return set.AccessToken, nil
}

// Invalidate implements backend.TokenSource.
//
// It takes the rejected token rather than clearing unconditionally, because refreshes
// race: two requests can fail on the SAME expired token while a third has already
// refreshed, and a bare reset from the first would discard the good token the third just
// minted.
func (m *Manager) Invalidate(accessToken string) {
	if accessToken == "" {
		return
	}
	m.mu.Lock()
	if m.access.AccessToken == accessToken {
		m.access = TokenSet{}
	}
	m.mu.Unlock()
}

// Secrets implements backend.TokenScrubber, so a backend that echoes our bearer into an
// error body cannot leave it in terminal scrollback or the debug log.
func (m *Manager) Secrets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.access.AccessToken == "" {
		return nil
	}
	return []string{m.access.AccessToken}
}

// refresh obtains a fresh access token, serialized in-process and across processes.
func (m *Manager) refresh(ctx context.Context) (TokenSet, error) {
	// Singleflight. Without it every waiter behind the file lock would perform its own
	// redundant refresh on release, spending one rotating one-time-use refresh token per
	// goroutine and very likely tripping the provider's reuse detection.
	//
	// Acquired through a channel rather than a bare Lock so a cancelled caller can leave.
	// A plain mutex is uninterruptible: with the provider down, every queued caller would
	// serve out its own full 30-second failure in turn, and a caller whose context was
	// cancelled long ago would still be sitting in the queue.
	select {
	case m.refreshing <- struct{}{}:
	case <-ctx.Done():
		return TokenSet{}, ctx.Err()
	}
	defer func() { <-m.refreshing }()

	// Another goroutine may have completed the refresh while this one waited.
	m.mu.Lock()
	cur := m.access
	m.mu.Unlock()
	if cur.AccessToken != "" && !cur.NeedsRefresh(m.now()) {
		return cur, nil
	}

	man, err := m.Manifest(ctx)
	if err != nil {
		m.recordUnavailable(err)
		return TokenSet{}, err
	}
	key := m.key(man)
	store := m.ensureStore(ctx)

	lock, err := acquireCredentialLock(ctx, m.authDir, key)
	if err != nil {
		return TokenSet{}, err
	}
	defer lock.release()

	// RE-READ under the lock. Another process may have rotated the token while this one
	// waited, and presenting the value read before the lock is exactly the reuse the
	// lock exists to prevent.
	stored, err := store.Load(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			m.setState(StateSignedOut)
			return TokenSet{}, newError(CodeNotSignedIn, "not signed in").
				withHint("Run `daintree-assistant auth login`.")
		}
		if errors.Is(err, ErrStoreLocked) {
			m.recordUnavailable(err)
			return TokenSet{}, wrapError(CodeStorageUnavailable, "the credential store is locked", err).
				withHint("Unlock your keychain and try again.")
		}
		m.recordUnavailable(err)
		return TokenSet{}, wrapError(CodeStorageUnavailable, "could not read the stored session", err)
	}

	redact.RegisterSecret(stored.RefreshToken)
	m.setState(StateRefreshing)
	set, err := m.tokens.Refresh(ctx, man, stored.RefreshToken)
	if err != nil {
		// A failed refresh is NOT automatically a revocation. A network blip must leave
		// the credential in place; only the provider explicitly rejecting the grant
		// means the session is gone.
		// CodeGrantRejected and nothing else. A timeout, a DNS failure or a 500 must
		// leave the credential exactly where it is.
		if CodeOf(err) == CodeGrantRejected {
			m.mu.Lock()
			m.access = TokenSet{}
			m.mu.Unlock()
			// Cleanup failures are REPORTED, not swallowed. A failed delete leaves a dead
			// credential that the next launch will try and fail on; a failed bump leaves
			// other processes still using a session that is gone. Both are worth saying
			// out loud, because the user's next action differs.
			// CodeSessionRevoked, never CodeNotSignedIn: AccessToken deliberately
			// swallows the latter as "send no header", and a revocation swallowed that
			// way would silently downgrade the user to anonymous requests instead of
			// telling them their session ended.
			revoked := newError(CodeSessionRevoked, "the session is no longer valid").
				withHint("Sign in again with `daintree-assistant auth login`.")
			_ = forgetKeyRef(m.authDir)
			if delErr := store.Delete(ctx, key); delErr != nil && !errors.Is(delErr, ErrNotFound) {
				revoked = wrapError(CodeStorageUnavailable,
					"the session is no longer valid, and the stored credential could not be removed", delErr).
					withHint("Remove the Daintree Assistant entry from your keychain manually.")
			} else if bumpErr := m.revision.Bump(ctx); bumpErr != nil {
				revoked = wrapError(CodeSessionRevoked,
					"the session is no longer valid, but other Daintree processes could not be notified", bumpErr)
			}
			m.setState(StateRevoked)
			return TokenSet{}, revoked
		}
		m.recordUnavailable(err)
		return TokenSet{}, err
	}

	// Persist BEFORE the new access token is used. If the write fails, the rotation has
	// already happened upstream — the old refresh token is spent — so continuing with an
	// unpersisted one would leave the next process unable to refresh at all.
	if set.RefreshToken != "" && set.RefreshToken != stored.RefreshToken {
		stored.RefreshToken = set.RefreshToken
		if err := store.Save(ctx, key, stored); err != nil {
			m.recordUnavailable(err)
			return TokenSet{}, wrapError(CodeStorageUnavailable, "could not store the rotated session", err)
		}
		// NO revision bump. A rotation is not an identity change: every other process's
		// access token is still valid until its own expiry, and each will re-read this
		// new refresh token under the lock when it next needs one.
		//
		// Bumping here produces a refresh storm. P1 rotates and bumps; P2 sees the bump,
		// discards a perfectly good access token and rotates, bumping again; P1 discards
		// ITS good token and rotates... indefinitely, spending a one-time-use refresh
		// token on every round trip. See revision.go.
	}

	registerSecrets(set)
	m.mu.Lock()
	m.access = set
	m.lastErr = nil
	if m.state == StateRefreshing || m.state == StateUnknown || m.state == StateTemporarilyUnavailable {
		// A refresh proves the credential works; it proves nothing about the plan, which
		// only the backend's session endpoint can answer.
		m.state = StateSignedInUnverified
	}
	m.mu.Unlock()
	return set, nil
}

// registerSecrets teaches the redactor about a live token set.
//
// The JWT access token is already covered by a SHAPE pattern in internal/redact, but a
// Supabase refresh token is an opaque string with no distinctive shape at all — no
// prefix, no length, no separator. Nothing pattern-based can ever match it, so exact
// registration is the only thing that can keep it out of the debug log and the support
// bundle. Registration is additive and permanent by design: a rotated token stays
// registered, because a log line written under it is still on disk.
func registerSecrets(set TokenSet) {
	redact.RegisterSecret(set.AccessToken)
	redact.RegisterSecret(set.RefreshToken)
}

// recordUnavailable records a dependency failure WITHOUT discarding credentials.
//
// This is the single most important error-handling rule in the package: "we could not
// check" must never be rendered as "you are signed out". Deleting a working credential
// because a network call failed turns a transient outage into a re-login for every user
// on the machine.
func (m *Manager) recordUnavailable(err error) {
	m.mu.Lock()
	m.lastErr = err
	if m.state.SignedIn() || m.state == StateUnknown {
		m.state = StateTemporarilyUnavailable
	}
	m.mu.Unlock()
}

// LoginResult is what a completed login reports.
type LoginResult struct {
	Manifest *Manifest
	Tier     StorageTier
	// Persisted is false when the session lives only in this process, which the caller
	// MUST tell the user.
	Persisted bool
}

// LoginProgress is a callback for the JSON event stream and the human spinner. It must
// never receive a live authorization URL — see Login.
type LoginProgress func(event, detail string)

// ManualURLSink receives the authorization URL when the browser is not being opened.
//
// It is a SEPARATE callback from LoginProgress, and that separation is the safety
// property rather than a style choice. The authorization URL carries a live request bound
// to this attempt's PKCE state, and LoginProgress feeds a structured event stream a
// caller may log, forward or persist. Two channels make it impossible to leak the URL
// into that stream by adding one more progress call; the CLI wires this one to stderr
// only, with a warning that the value is temporary and must not be pasted into a bug
// report.
type ManualURLSink func(url string)

// Login performs the whole interactive flow: discovery, PKCE, browser, callback,
// exchange, persist.
//
// The authorization URL is deliberately NOT passed to progress. It carries a live
// authorization request bound to this attempt's state, and progress output goes to a
// structured event stream a caller may log. Only the --no-open path surfaces it, to
// stderr, with a warning.
func (m *Manager) Login(ctx context.Context, openBrowser bool, progress LoginProgress, manualURL ManualURLSink) (LoginResult, error) {
	m.loggingIn.Lock()
	if m.inLogin {
		m.loggingIn.Unlock()
		return LoginResult{}, newError(CodeLoginInProgress, "a sign-in is already in progress in this process")
	}
	m.inLogin = true
	m.loggingIn.Unlock()
	defer func() {
		m.loggingIn.Lock()
		m.inLogin = false
		m.loggingIn.Unlock()
	}()

	report := func(event, detail string) {
		if progress != nil {
			progress(event, detail)
		}
	}

	man, err := m.Manifest(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	report("starting", man.Environment)

	attempt, err := newPKCEAttempt()
	if err != nil {
		return LoginResult{}, err
	}

	// Bind BEFORE opening the browser. The other order races a fast provider redirect
	// against the socket coming up, and the user sees a connection refused for a login
	// that was otherwise fine.
	ln, err := listen(attempt.State)
	if err != nil {
		return LoginResult{}, err
	}
	defer ln.close()

	authURL, err := buildAuthorizeURL(man, attempt)
	if err != nil {
		return LoginResult{}, err
	}

	// The state this process was in BEFORE the attempt. A failed sign-in must restore it
	// rather than forcing signed-out: forcing it would report an unauthenticated account
	// while a perfectly good previous credential was still stored and still in use by
	// every other process — the status and the behaviour would simply disagree.
	m.mu.Lock()
	priorState, hadCredential := m.state, m.access.AccessToken != ""
	m.state = StateAuthorizing
	m.mu.Unlock()
	restore := func() {
		m.mu.Lock()
		if hadCredential || priorState.SignedIn() {
			m.state = priorState
		} else {
			m.state = StateSignedOut
		}
		m.mu.Unlock()
	}

	if openBrowser {
		if err := m.opener.Open(ctx, authURL); err != nil {
			restore()
			return LoginResult{}, err
		}
		// The SAFE account origin, never the authorization URL.
		report("browser_opened", man.AccountURL)
	} else {
		if manualURL == nil {
			restore()
			return LoginResult{}, newError(CodeInteractiveRequired,
				"no browser was opened and there is nowhere to print the sign-in URL").
				withHint("Run sign-in without --no-open, or from a terminal that can display it.")
		}
		// Out of band from the event stream, by construction.
		manualURL(authURL)
		report("manual_url_required", "")
	}
	report("waiting", callbackAddr())

	code, err := ln.wait(ctx)
	if err != nil {
		// Every exit from here restores the prior state. Leaving StateAuthorizing behind
		// on a timeout or a state mismatch would strand the account UI on "finish signing
		// in in your browser" with no browser and no way out.
		restore()
		return LoginResult{}, err
	}

	set, err := m.tokens.Exchange(ctx, man, code, attempt.Verifier)
	if err != nil {
		restore()
		return LoginResult{}, err
	}
	if set.RefreshToken == "" {
		// Without one the session cannot outlive the access token, so the "30-day login"
		// this whole feature exists to provide would silently be a one-hour login.
		restore()
		return LoginResult{}, newError(CodeExchangeFailed, "the identity provider issued no refresh token, so the session could not be kept")
	}

	key := m.key(man)
	store := m.ensureStore(ctx)
	lock, err := acquireCredentialLock(ctx, m.authDir, key)
	if err != nil {
		restore()
		return LoginResult{}, err
	}

	session := StoredSession{
		Version:      storedSessionVersion,
		RefreshToken: set.RefreshToken,
		Issuer:       man.Issuer,
		ClientID:     man.ClientID,
		Environment:  man.Environment,
	}
	saveErr := store.Save(ctx, key, session)
	if saveErr != nil && errors.Is(saveErr, ErrStoreUnavailable) {
		// An explicit degrade, never a silent one — and an ACTUAL one. Reporting
		// "not persisted" while leaving the unavailable store in place would make the
		// promised process-lifetime session expire with the access token instead, since
		// the next refresh would fail on the same store.
		store = m.fallBackToMemory()
		saveErr = store.Save(ctx, key, session)
	}
	if saveErr != nil {
		lock.release()
		restore()
		return LoginResult{}, wrapError(CodeStorageUnavailable, "could not store the session", saveErr)
	}
	_ = saveKeyRef(m.authDir, key) // best effort: it only enables OFFLINE logout

	// Persistence is derived from the TIER, not from whether Save returned an error. A
	// MemoryStore saves successfully and still loses the session on exit, so trusting the
	// error would report an ordinary signed-in state for a login that evaporates.
	m.mu.Lock()
	tier := m.tier
	m.mu.Unlock()
	persisted := tier == TierKeychain

	if err := m.revision.Bump(ctx); err != nil {
		lock.release()
		restore()
		return LoginResult{}, err
	}
	lock.release()

	registerSecrets(set)
	m.mu.Lock()
	m.access = set
	m.lastErr = nil
	m.generation++
	if persisted {
		m.state = StateSignedInUnverified
	} else {
		m.state = StateStorageUnavailable
	}
	m.mu.Unlock()

	// Reported AFTER the credential lock is released. A progress callback is
	// caller-supplied and may re-enter — a UI that reacts to "authenticated" by asking
	// for status, or by logging out — and doing that while this function still held the
	// lock would block it until the 30-second acquisition timeout.
	report("authenticated", "")
	return LoginResult{Manifest: man, Tier: tier, Persisted: persisted}, nil
}

// Logout ends the local session.
//
// The local credential is deleted REGARDLESS of whether the provider could be reached.
// A user must always be able to remove access from their own machine; making that
// conditional on a network call means an offline laptop cannot be signed out. The caller
// is told when server-side revocation could not be confirmed.
func (m *Manager) Logout(ctx context.Context) (revokedRemotely bool, err error) {
	// The key comes from discovery when the backend is reachable, and from the
	// descriptor written at login when it is not.
	//
	// The fallback is what makes offline logout work at all. Deriving the key requires
	// the issuer and client id, which normally arrive in the manifest — so without a
	// recorded copy, a laptop on a plane (or one pointed at a backend that is down)
	// could clear only its own memory while the keychain entry survived and every daemon
	// kept spending. Making the ability to sign out conditional on a network call hands
	// that decision to whoever runs the server.
	var key CredentialKey
	if man, manErr := m.Manifest(ctx); manErr == nil {
		key = m.key(man)
	} else if recorded, ok := loadKeyRef(m.authDir); ok {
		key = recorded
	} else {
		// Nothing was ever stored on this machine under a key we can name. Dropping the
		// in-memory token is all there is to do, and it is enough.
		m.mu.Lock()
		m.access = TokenSet{}
		m.state = StateSignedOut
		m.mu.Unlock()
		return false, nil
	}
	store := m.ensureStore(ctx)

	lock, lerr := acquireCredentialLock(ctx, m.authDir, key)
	if lerr != nil {
		return false, lerr
	}
	defer lock.release()

	delErr := store.Delete(ctx, key)
	_ = forgetKeyRef(m.authDir)

	m.mu.Lock()
	m.access = TokenSet{}
	m.state = StateSignedOut
	m.generation++ // a late verdict from a pre-logout request must not resurrect anything
	m.mu.Unlock()

	// The revision moves even if the delete failed: every other process must stop using
	// its cached token either way, and a failed delete is exactly when that matters most.
	if bumpErr := m.revision.Bump(ctx); bumpErr != nil && delErr == nil {
		delErr = bumpErr
	}
	// revokedRemotely is always false today: there is no server-side sign-out call yet,
	// so the honest answer is that only the local credential is gone. Callers surface
	// that rather than implying the session was killed everywhere.
	return false, delErr
}

// Hydrate loads the stored credential and settles the local state, WITHOUT contacting
// the backend.
//
// It exists because Status is deliberately I/O-free, and a freshly-constructed Manager in
// a new process therefore knows nothing: `auth status` right after a successful login
// would report "unknown". Hydrate is the explicit, bounded read that fills that gap — a
// credential-store lookup and nothing more, so it stays answerable while the network is
// down.
//
// It is separate from AccessToken because it must NOT refresh: someone asking about their
// account should not spend a rotating one-time-use token to be told what it is.
func (m *Manager) Hydrate(ctx context.Context) {
	key, ok := m.currentKey(ctx)
	if !ok {
		m.mu.Lock()
		if m.state == StateUnknown {
			m.state = StateSignedOut
		}
		m.mu.Unlock()
		return
	}
	store := m.ensureStore(ctx)
	stored, err := store.Load(ctx, key)
	switch {
	case err == nil && stored.Valid():
		redact.RegisterSecret(stored.RefreshToken)
		m.mu.Lock()
		if !m.state.SignedIn() {
			// A stored credential proves a login exists; it proves nothing about the
			// plan, which only the backend session endpoint can answer.
			m.state = StateSignedInUnverified
		}
		m.mu.Unlock()
	case errors.Is(err, ErrNotFound):
		m.mu.Lock()
		m.state = StateSignedOut
		m.mu.Unlock()
	case errors.Is(err, ErrStoreLocked), errors.Is(err, ErrStoreUnavailable):
		// "We could not read it" is not "there is nothing there".
		m.recordUnavailable(err)
	case errors.Is(err, ErrStoreCorrupt):
		m.mu.Lock()
		m.lastErr = err
		m.state = StateSignedOut
		m.mu.Unlock()
	}
}

// Revision exposes the shared marker, for the daemon's status and its stop-work check.
func (m *Manager) Revision() *Revision { return m.revision }

// AuthDirPath returns the auth coordination directory (diagnostics only).
func (m *Manager) AuthDirPath() string { return m.authDir }

// LastError returns the most recent recorded failure, for status.
func (m *Manager) LastError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Generation returns the current local identity generation.
//
// A caller captures this BEFORE a backend request and passes it back with the verdict,
// so an answer that arrives after a logout or a re-login can be recognised as stale.
func (m *Manager) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// ApplyBackendVerdict folds a backend account error into the local state.
//
// It is the seam that keeps the two taxonomies in step: the backend decides what is true
// about the account, and this decides what that means for the credential on this machine.
//
// `gen` is the generation captured before the request, and `usedToken` is the access
// token it was made with. Both are checked before anything destructive happens, because
// verdicts arrive late: a 401 for token A can land after the user has already signed back
// in with token B, and acting on it unconditionally would delete a session that is fine.
// Only RemedyClear deletes anything.
func (m *Manager) ApplyBackendVerdict(ctx context.Context, gen uint64, usedToken string, err error) {
	var be *backend.Error
	if !errors.As(err, &be) {
		return
	}
	m.mu.Lock()
	stale := m.generation != gen || (usedToken != "" && m.access.AccessToken != "" && m.access.AccessToken != usedToken)
	m.mu.Unlock()
	if stale {
		// The answer describes a credential this process has already moved on from.
		return
	}

	switch {
	case be.AuthRemedy() == backend.RemedyClear:
		m.clearSession(ctx)
	case be.AuthRemedy() == backend.RemedyRefresh, be.AuthRemedy() == backend.RemedyRefreshOrSignIn:
		// The token is stale rather than wrong. Drop it so the next AccessToken call
		// refreshes instead of re-presenting the value the backend just refused —
		// which, for a token with no readable expiry, would otherwise loop forever.
		m.Invalidate(usedToken)
		m.setState(StateRefreshing)
	case be.AuthRemedy() == backend.RemedySignIn:
		m.mu.Lock()
		m.access = TokenSet{}
		m.state = StateSignedOut
		m.mu.Unlock()
	case be.AuthRemedy() == backend.RemedyReconfigure:
		// A valid token this deployment will not accept. Nothing about the credential is
		// wrong, so it is kept — but nothing can be verified either, and refreshing would
		// only mint another token rejected in exactly the same way.
		m.recordUnavailable(err)
	case be.Code == backend.CodeSubscriptionRequired:
		m.setState(StateSubscriptionRequired)
	case be.Code == backend.CodeSubscriptionInactive:
		m.setState(StateSubscriptionInactive)
	case be.IsAccountDependency():
		m.recordUnavailable(err)
	}
}

// clearSession deletes the stored credential and marks the session revoked.
func (m *Manager) clearSession(ctx context.Context) {
	key, ok := m.currentKey(ctx)
	if ok {
		if lock, lErr := acquireCredentialLock(ctx, m.authDir, key); lErr == nil {
			_ = m.ensureStore(ctx).Delete(ctx, key)
			_ = forgetKeyRef(m.authDir)
			_ = m.revision.Bump(ctx)
			lock.release()
		}
	}
	m.mu.Lock()
	m.access = TokenSet{}
	m.state = StateRevoked
	m.generation++
	m.mu.Unlock()
}

// currentKey resolves the credential key, preferring live discovery and falling back to
// the descriptor written at login so this works offline.
func (m *Manager) currentKey(ctx context.Context) (CredentialKey, bool) {
	if man, err := m.Manifest(ctx); err == nil {
		return m.key(man), true
	}
	return loadKeyRef(m.authDir)
}

// MarkActive records a confirmed, entitled session, if the confirmation is still current.
func (m *Manager) MarkActive(gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != gen {
		// A confirmation for a session that has since been logged out. Applying it would
		// show the user as signed in with no credential behind it.
		return
	}
	m.state = StateSignedInActive
}

// Ensure Manager satisfies the backend seams at compile time.
var (
	_ backend.TokenSource   = (*Manager)(nil)
	_ backend.TokenScrubber = (*Manager)(nil)
)
