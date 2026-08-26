package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	// superseded holds access tokens replaced by a refresh, so an in-flight request
	// still carrying one can have it masked out of an error the backend echoes back.
	superseded []string
	// lastVerifiedAt is when a protected request last succeeded under this session. It
	// is the one thing only the BACKEND can tell us: a stored credential proves a login
	// happened, never that the deployment still honours it.
	//
	// It answers ONLY that question. It is not evidence of a plan, and the state machine
	// must never read it as one — see MarkIdentityLive.
	lastVerifiedAt *time.Time
	// account is what the backend last said about this session — email, plan, billing
	// verdict. MEMORY ONLY and generation-stamped; see accountsnapshot.go.
	account accountSnapshot
	tier    StorageTier
	// generation rises on every LOCAL identity change (login, logout, revocation).
	//
	// It exists because backend verdicts arrive late. A request made with token A can
	// return its answer after the user has logged out and signed back in with token B,
	// and an unconditional "revoked" verdict would then delete B's perfectly good
	// session — or an unconditional MarkIdentityLive would vouch for a session that has
	// been logged out. Callers capture the generation when they start a request and hand
	// it back
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

// Availability reports what this deployment says about accounts. Answerable when
// Manifest is not — see the type — which is the whole reason it is its own call.
func (m *Manager) Availability(ctx context.Context) Availability {
	return m.discoverer.Availability(ctx)
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
		m.rememberSupersededLocked(m.access.AccessToken)
		m.access = TokenSet{} // whatever we cached predates an identity change
		// The IDENTITY changed, somewhere else. Advancing the generation is what makes
		// every request already in flight stale: without it, a 2xx for the old session
		// lands with a generation that still matches and re-confirms a login that has
		// been ended in another process.
		m.generation++
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
		// A state that CLAIMS a session is corrected, not just an unknown one.
		//
		// The descriptor is written at login and removed at logout, so its absence here
		// is definitive: there is no credential on this machine. A long-lived process
		// whose own state still said `signed_in_active` because ANOTHER process signed
		// out went on reporting a spendable session to every surface that renders one,
		// and CanSpend() — the strictest reading of that state — kept answering true for
		// an account the user believes they have signed out of. The token was already
		// gone by then; only the state disagreed.
		//
		// (CanSpend has no production caller today; the supervisor's wake gate branches
		// on StateRevoked/StateSignedOut directly. This comment used to name it as the
		// gate, which sent a reader looking for a coupling that is not there.)
		//
		// NARROW, not every state SignedIn() covers. Three of those carry a diagnosis
		// this branch has no business overwriting:
		//
		//   StateAuthorizing — a login running in THIS process, which has not written
		//     its descriptor yet. Overwriting it reports a sign-in as signed out while
		//     the browser is still open.
		//   StateStorageUnavailable — a memory-only credential, on a box with no
		//     credential service. The descriptor write is best-effort and can fail for
		//     the same reason the store did, so its absence there is a symptom of the
		//     tier rather than evidence of a sign-out, and "signed out" would hide the
		//     one fact the user needs: their session works and will not survive exit.
		//   StateAccessRefused — deliberately retained (see state.go), because a fresh
		//     credential would be refused identically. Downgrading erases the refusal
		//     and invites an endless re-login loop against a deployment that will not
		//     accept this client.
		//
		// What IS corrected is every state asserting an ordinary working session. Those
		// are contradicted by the evidence: no descriptor means no credential on this
		// machine, and this process has no usable token either — the early return above
		// already handled the case where it does.
		switch m.state {
		case StateUnknown, StateSignedInUnverified, StateSignedInActive,
			StateSubscriptionRequired, StateSubscriptionInactive, StateTemporarilyUnavailable:
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
		m.rememberSupersededLocked(m.access.AccessToken)
		m.access = TokenSet{}
	}
	m.mu.Unlock()
}

// Secrets implements backend.TokenScrubber, so a backend that echoes our bearer into an
// error body cannot leave it in terminal scrollback or the debug log.
func (m *Manager) Secrets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.superseded)+1)
	if m.access.AccessToken != "" {
		out = append(out, m.access.AccessToken)
	}
	// Recently-superseded tokens too, and this is not belt-and-braces. Requests overlap:
	// one can be refreshed out from under another that is still in flight, and when that
	// older request's error comes back echoing its own Authorization header, the current
	// token is not the one to mask. The refresh-and-replay ladder makes that overlap
	// routine rather than rare.
	out = append(out, m.superseded...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// rememberSupersededLocked keeps a token maskable after it has been replaced.
//
// Bounded to a handful: the window that matters is one request's lifetime, and an
// unbounded list would grow for the life of a long-running daemon. Oldest is dropped
// first, which is the one least likely to still be attached to anything in flight.
func (m *Manager) rememberSupersededLocked(token string) {
	if token == "" {
		return
	}
	for _, t := range m.superseded {
		if t == token {
			return
		}
	}
	const maxSuperseded = 4
	m.superseded = append(m.superseded, token)
	if len(m.superseded) > maxSuperseded {
		m.superseded = m.superseded[len(m.superseded)-maxSuperseded:]
	}
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
		if CodeOf(err) == CodeAccountsUnavailable {
			// The deployment has TURNED OFF accounts under a machine that still holds a
			// credential for it. There is nothing to refresh against and nothing wrong
			// locally, and the backend serves anonymous requests — so this must read as
			// "send no credential", exactly like never having signed in.
			//
			// recordUnavailable would be doubly wrong here: it reports an outage for a
			// deployment that answered, and the error it returns propagates out of
			// AccessToken, where the client turns it into CodeCredentialUnavailable and
			// ABORTS the request rather than sending it bare. A backend perfectly willing
			// to serve this user would become unreachable until they logged out.
			m.mu.Lock()
			m.access = TokenSet{}
			m.state = StateAccountsUnavailable
			m.mu.Unlock()
			return TokenSet{}, newError(CodeNotSignedIn, "this backend does not offer account sign-in")
		}
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

// loginAfterCredentialUnlock runs immediately after a successful login releases the
// credential lock. Tests only; see the call site for what it is for and why the invariant
// it exposes cannot be reached any other way.
var loginAfterCredentialUnlock func()

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
	priorGen := m.generation
	m.state = StateAuthorizing
	m.mu.Unlock()
	restore := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// ONLY IF THE SESSION IT DESCRIBES IS STILL THERE.
		//
		// Login serializes against other logins and nothing else: a `Logout`, a revoked
		// verdict or a failed refresh can all land while this attempt sits in the
		// browser for up to five minutes, and every one of them ends the prior session
		// and advances the generation. Writing the snapshot back unconditionally then
		// RESURRECTS it — `signed_in_active` restored over a session that was
		// deliberately ended, with no token behind it, and CanSpend() answering true for
		// a credential that no longer exists.
		//
		// The generation is the test because it is what every one of those paths moves.
		// If it has changed, whatever ended the session is a newer statement about it
		// than this snapshot, and a cancelled sign-in has no business overruling it.
		if m.generation != priorGen {
			return
		}
		if hadCredential || priorState.SignedIn() {
			m.state = priorState
		} else {
			m.state = StateSignedOut
		}
	}

	if openBrowser {
		if err := m.opener.Open(ctx, authURL); err != nil {
			restore()
			// The REMEDY is attached here, not only inside SystemOpener, because the
			// remedy is a property of this operation rather than of any one launcher: a
			// browser that will not open leaves exactly one way to sign in on this
			// machine, whichever Opener failed to open it. Attaching it only in the
			// system launcher meant an alternative one — a test double, an embedding
			// host's own — produced a failure with no way out of it, and the surfaces
			// that render HintOf simply showed nothing.
			//
			// An error that already carries a hint keeps its own: the platform cases in
			// SystemOpener are more specific than this, and overwriting them would trade
			// a precise remedy for a generic one.
			if HintOf(err) == "" {
				return LoginResult{}, wrapError(CodeBrowserFailed, "could not open a browser", err).
					withHint(noOpenHint)
			}
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
		// ROLL THE CREDENTIAL BACK before reporting failure, under the lock that is
		// still held.
		//
		// Save has already overwritten whatever was stored, so by this point the
		// PREVIOUS session's refresh token is gone whatever happens next — there is no
		// outcome that preserves it. What is still in this function's gift is whether
		// "login failed" is true. Leaving the new credential behind makes it false in the
		// worst way: a fresh process loads a session this one has just told the user it
		// could not create, and on a re-login as a different account the two disagree
		// about who is signed in. Deleting leaves one consistent story — signed out, the
		// sign-in failed, try again.
		//
		// Best-effort, and its error is discarded on purpose: the failure being reported
		// is the bump, and replacing it with a cleanup error would name the wrong cause.
		_ = m.ensureStore(ctx).Delete(ctx, key)
		if recorded, ok := loadKeyRef(m.authDir); ok && recorded == key {
			_ = forgetKeyRef(m.authDir)
		}
		lock.release()
		restore()
		return LoginResult{}, err
	}

	// PUBLISHED BEFORE THE LOCK IS RELEASED, and that ordering is the whole point.
	//
	// clearSessionIfCurrent — the path a revoked verdict takes — acquires this same
	// credential lock and only THEN rechecks whether the identity it was told about is
	// still current. Releasing here first opened a window between "the new refresh token
	// is stored" and "the new generation is visible", and a revocation for the OLD
	// session that had been blocking on the lock would wake inside it, see the old
	// generation and the old access token, conclude it was current, and delete the
	// credential this login had just persisted. The user would be told "Signed in.",
	// `Persisted` would be true, and the keychain entry would be gone.
	//
	// Publishing first closes it: any clearer that acquires the lock after this point
	// sees the bumped generation and the replaced token, fails its staleness check, and
	// leaves the new credential alone. Nothing below can fail, so there is no path that
	// publishes a session and then abandons it.
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

	lock.release()
	// A SEAM, for the one invariant above that is otherwise unobservable.
	//
	// The ordering this function depends on is a few nanoseconds wide: a clearer blocked
	// on the credential lock has to wake, acquire it, and reach its staleness check
	// before the login publishes. A stress test cannot reliably land in that window —
	// tried, and it does not — so the alternative to this hook is an invariant defended
	// only by a comment, in a function whose failure mode is deleting a credential
	// seconds after telling somebody they signed in.
	//
	// Placed immediately AFTER the release, which is exactly where a competing clearer
	// gets its turn. A test drives one from here and asserts it declines; under the
	// wrong ordering the identity would not yet be published and it would delete.
	// nil in every build that is not a test, and one nil check is the whole cost.
	if hook := loginAfterCredentialUnlock; hook != nil {
		hook()
	}

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
// conditional on a network call means an offline laptop cannot be signed out.
//
// It is LOCAL ONLY, and the return type says so rather than implying otherwise: there is
// no server-side sign-out call, so `revokedRemotely` is always false today. Revoking the
// grant across every device is a separate, website-side action — see `auth disconnect`,
// which prints the page and revokes nothing itself.
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
	key, ok := m.currentKey(ctx)
	if !ok {
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
func (m *Manager) Hydrate(ctx context.Context) (resolved bool) {
	// Sample the generation FIRST. Everything below does network and credential-store
	// work, and a login can complete in that window — at which point writing this
	// hydrate's conclusion would wipe the access token the login just installed. The
	// stored credential would survive, but the live manager would go on believing it is
	// signed out until something re-read the store.
	gen := m.Generation()
	// applyIfCurrent writes hydrate's conclusion only if the identity has not moved
	// underneath it.
	applyIfCurrent := func(fn func()) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.generation != gen {
			return
		}
		fn()
	}

	key, resolution := m.resolveKey(ctx)
	switch resolution {
	case keyAbsent:
		// Definitively no login on this machine. Set UNCONDITIONALLY rather than only
		// downgrading from Unknown — that guard was the bug the daemon exposed: a daemon
		// that had marked itself active kept believing so after a logout elsewhere
		// removed the descriptor, which is exactly the case Hydrate re-reads for.
		applyIfCurrent(func() {
			m.access = TokenSet{}
			m.state = StateSignedOut
		})
		return true
	case keyNotOffered:
		// The backend ANSWERED and says it has no identity provider. There is no
		// credential to find and none to look for, and this is the branch that stops
		// that being reported as an outage: it used to fall into keyUnresolved below,
		// which set StateTemporarilyUnavailable — a state whose SignedIn() is true — so
		// `auth status` on a deployment working exactly as designed said "signed in —
		// could not check just now", with authenticated:true and no session behind it.
		applyIfCurrent(func() {
			m.access = TokenSet{}
			m.state = StateAccountsUnavailable
		})
		return true
	case keyUnresolved:
		// "I could not work out which credential this is" is not "you are signed out".
		// Signing someone out over a backend outage is the failure this branch exists to
		// prevent.
		unresolved := newError(CodeDiscoveryUnavailable, "could not determine which account this backend uses")
		if _, everSignedIn := loadKeyRef(m.authDir); !everSignedIn {
			// No descriptor means no login has EVER completed on this machine, so there
			// is no session for an outage to have interrupted. recordUnavailable would
			// promote Unknown to TemporarilyUnavailable here — a state whose SignedIn()
			// is true — and `auth status` on a fresh install pointed at an unreachable
			// backend would answer "signed in", with authenticated:true and no
			// credential anywhere. Record the reason and leave the state unknown, which
			// is what it is.
			applyIfCurrent(func() { m.lastErr = unresolved })
			return false
		}
		applyIfCurrent(func() {
			m.lastErr = unresolved
			if m.state.SignedIn() || m.state == StateUnknown {
				m.state = StateTemporarilyUnavailable
			}
		})
		return false
	}
	store := m.ensureStore(ctx)
	stored, err := store.Load(ctx, key)
	switch {
	case err == nil && stored.Valid():
		redact.RegisterSecret(stored.RefreshToken)
		applyIfCurrent(func() {
			if !m.state.SignedIn() {
				// A stored credential proves a login exists; it proves nothing about the
				// plan, which only the backend session endpoint can answer.
				m.state = StateSignedInUnverified
			}
		})
	case errors.Is(err, ErrNotFound):
		applyIfCurrent(func() { m.state = StateSignedOut })
	case errors.Is(err, ErrStoreLocked), errors.Is(err, ErrStoreUnavailable):
		// "We could not read it" is not "there is nothing there". Guarded like every
		// other write here: a slow hydrate must not overwrite a login that completed
		// while it was waiting on the credential store.
		applyIfCurrent(func() {
			m.lastErr = err
			if m.state.SignedIn() || m.state == StateUnknown {
				m.state = StateTemporarilyUnavailable
			}
		})
		return false
	case errors.Is(err, ErrStoreCorrupt):
		applyIfCurrent(func() {
			m.lastErr = err
			m.state = StateSignedOut
		})
	}
	return true
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
// token it was made with. Both are checked because verdicts arrive late: a 401 for token
// A can land after the user has already signed back in with token B, and acting on it
// unconditionally would delete a session that is fine. Only RemedyClear deletes anything.
//
// The staleness check is INSIDE the same critical section as the write it guards, which
// it was not before. Checking under the lock, releasing, and then acting is not a guard
// at all — a logout and a fresh login fit in that window, and the verdict for the dead
// generation then lands on the new one. For the destructive case that meant deleting a
// credential the user had just created.
func (m *Manager) ApplyBackendVerdict(ctx context.Context, gen uint64, usedToken string, err error) {
	var be *backend.Error
	if !errors.As(err, &be) {
		return
	}

	// applyIfCurrent runs fn under the lock, but only if the verdict still describes the
	// credential this process holds. Every non-destructive transition goes through it, so
	// the recheck cannot be forgotten by adding a case.
	applyIfCurrent := func(fn func()) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.staleLocked(gen, usedToken) {
			return false
		}
		fn()
		return true
	}

	switch {
	case be.AuthRemedy() == backend.RemedyClear:
		// The destructive one. clearSession takes a cross-process file lock and deletes
		// from the credential store, which cannot happen under m.mu — so it does its own
		// recheck after acquiring that lock. The check here is the cheap early exit.
		m.mu.Lock()
		stale := m.staleLocked(gen, usedToken)
		m.mu.Unlock()
		if !stale {
			m.clearSessionIfCurrent(ctx, gen, usedToken)
		}
	case be.AuthRemedy() == backend.RemedyRefresh, be.AuthRemedy() == backend.RemedyRefreshOrSignIn:
		// The token is stale rather than wrong. Drop it so the next AccessToken call
		// refreshes instead of re-presenting the value the backend just refused —
		// which, for a token with no readable expiry, would otherwise loop forever.
		//
		// The compare-and-clear is the same one Invalidate performs, done here so it
		// shares the generation recheck rather than racing it.
		applyIfCurrent(func() {
			if usedToken != "" && m.access.AccessToken == usedToken {
				m.rememberSupersededLocked(m.access.AccessToken)
				m.access = TokenSet{}
			}
			m.state = StateRefreshing
		})
	case be.AuthRemedy() == backend.RemedySignIn:
		applyIfCurrent(func() {
			m.access = TokenSet{}
			m.state = StateSignedOut
		})
	case be.AuthRemedy() == backend.RemedyReconfigure:
		// A valid token this deployment will not accept. Nothing about the credential is
		// wrong, so it is KEPT — but refreshing would only mint another token rejected in
		// exactly the same way, and recordUnavailable (where this used to go) says a
		// dependency blipped. That is not what happened: the backend answered, and the
		// answer will not change until someone alters the deployment.
		applyIfCurrent(func() {
			m.lastErr = err
			m.state = StateAccessRefused
		})
	case be.Code == backend.CodeSubscriptionRequired:
		applyIfCurrent(func() { m.state = StateSubscriptionRequired })
	case be.Code == backend.CodeSubscriptionInactive:
		applyIfCurrent(func() { m.state = StateSubscriptionInactive })
	case be.IsAccountDependency():
		applyIfCurrent(func() {
			m.lastErr = err
			if m.state.SignedIn() || m.state == StateUnknown {
				m.state = StateTemporarilyUnavailable
			}
		})
	}
}

// staleLocked reports that a verdict describes a credential this process has moved on
// from. Callers must hold m.mu.
//
// Two independent ways to be stale. The GENERATION moves on a logout or a session
// clear, so a mismatch means the identity itself has changed. The TOKEN changes on an
// ordinary refresh, which does NOT move the generation — so without the second check a
// 401 for a token that was replaced seconds ago would be applied to its replacement.
func (m *Manager) staleLocked(gen uint64, usedToken string) bool {
	if m.generation != gen {
		return true
	}
	return usedToken != "" && m.access.AccessToken != "" && m.access.AccessToken != usedToken
}

// clearSession deletes the stored credential and marks the session revoked.
func (m *Manager) clearSession(ctx context.Context) {
	m.clearSessionIfCurrent(ctx, m.Generation(), "")
}

// clearSessionIfCurrent deletes the stored credential, unless the identity moved while
// the cross-process lock was being acquired.
//
// The recheck AFTER the lock is the load-bearing part. Acquiring it can block on another
// process, and a local logout-then-login fits comfortably in that wait — at which point
// deleting would destroy the credential the user has just created and leave them signed
// out with no failure to explain it. The state write happens under the same m.mu
// acquisition as the final check, so nothing can slip between them either.
func (m *Manager) clearSessionIfCurrent(ctx context.Context, gen uint64, usedToken string) {
	key, ok := m.currentKey(ctx)
	if ok {
		lock, lErr := acquireCredentialLock(ctx, m.authDir, key)
		if lErr == nil {
			// THE SHARED MARKER, checked here and not only in staleLocked.
			//
			// staleLocked compares this process's own generation and token, and that is
			// blind to the case this lock exists for. Another PROCESS can complete a
			// whole login while this clearer is blocked: it writes its credential, bumps
			// the marker and releases. This manager's generation has not moved — nothing
			// local happened — so the local check says "still current" and the delete
			// destroys a credential belonging to a session that started after the
			// verdict being acted on.
			//
			// Unlike the local window, this one is not nanoseconds: it lasts until this
			// manager next calls AccessToken and adopts the new marker, which may be a
			// whole idle turn away.
			//
			// Declining on ANY unobserved bump is deliberately blunt. It can skip a
			// genuine revocation — but only defers it, because the same unobserved marker
			// makes the next AccessToken clear the cached credential and advance the
			// generation anyway. Deleting the wrong credential has no equivalent
			// recovery: the refresh token is gone and the user is signed out with
			// nothing to explain it.
			if m.revision.Changed() {
				lock.release()
				return
			}
			m.mu.Lock()
			stale := m.staleLocked(gen, usedToken)
			m.mu.Unlock()
			if stale {
				lock.release()
				return
			}
			_ = m.ensureStore(ctx).Delete(ctx, key)
			// The descriptor is a SINGLE file for the whole state root, while the lock
			// above is per credential key. Removing it unconditionally would let a clear
			// for one endpoint delete the descriptor a login for ANOTHER had just
			// written — leaving that session unresolvable with its credential intact.
			if recorded, ok := loadKeyRef(m.authDir); ok && recorded == key {
				_ = forgetKeyRef(m.authDir)
			}
			_ = m.revision.Bump(ctx)
			lock.release()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.staleLocked(gen, usedToken) {
		return
	}
	m.access = TokenSet{}
	m.state = StateRevoked
	m.generation++
}

// currentKey resolves the credential key, preferring live discovery and falling back to
// the descriptor written at login so this works offline.
// currentKey resolves the credential key.
//
// The RECORDED descriptor is preferred over live discovery, and the order matters: it is
// a local file read against a network round trip, on a path that runs before every
// status check and every daemon decision. Discovery is the fallback for a machine that
// has a credential but no descriptor (an older build, a partially-cleared state dir).
func (m *Manager) currentKey(ctx context.Context) (CredentialKey, bool) {
	key, resolution := m.resolveKey(ctx)
	return key, resolution == keyResolved
}

// keyResolution distinguishes the three answers currentKey used to collapse into a bool.
//
// The collapse was a real bug: "there is definitely no login here" and "I could not work
// out which credential this is" are opposite facts, and Hydrate treated both as a
// definitive sign-out. A transient backend outage on an install whose descriptor predates
// this build would therefore report the user signed out when they are not.
type keyResolution int

const (
	// keyResolved: the credential key is known.
	keyResolved keyResolution = iota
	// keyAbsent: there is definitively no credential on this machine.
	keyAbsent
	// keyUnresolved: there may be one, but which is unknown right now.
	keyUnresolved
	// keyNotOffered: this deployment has no accounts, so there is no key to resolve.
	// Distinct from keyUnresolved because it is an ANSWER rather than a gap, and the
	// two demand opposite handling — one is settled and fine, the other must never be
	// allowed to look like a verdict.
	keyNotOffered
)

// resolveKey works out which credential this manager is responsible for.
func (m *Manager) resolveKey(ctx context.Context) (CredentialKey, keyResolution) {
	recorded, ok := loadKeyRef(m.authDir)
	if ok {
		// The descriptor is a SINGLE file per state root, so it can describe a credential
		// for a different backend than this manager is configured for — after a
		// `/backend` switch, for instance. Using it unchecked would make `auth status`
		// for B report A's session, and `auth logout` for B DELETE A's credential.
		if m.backendOriginMatches(recorded) {
			return recorded, keyResolved
		}
		// It belongs to another endpoint. This one's key can still be derived if
		// discovery works.
		man, err := m.Manifest(ctx)
		if err == nil {
			return m.key(man), keyResolved
		}
		if CodeOf(err) == CodeAccountsUnavailable {
			// A descriptor for ANOTHER endpoint, and this one has no accounts. There is
			// nothing here to resolve, and saying so beats reporting an outage.
			return CredentialKey{}, keyNotOffered
		}
		return CredentialKey{}, keyUnresolved
	}
	man, err := m.Manifest(ctx)
	if err == nil {
		return m.key(man), keyResolved
	}
	if CodeOf(err) == CodeAccountsUnavailable {
		return CredentialKey{}, keyNotOffered
	}
	// No descriptor AND no manifest. A descriptor is written on every successful login,
	// so its absence is strong evidence of no login — but not proof, since persistence is
	// best effort and an older build wrote none. Reporting "unresolved" keeps a
	// transient outage from signing someone out.
	return CredentialKey{}, keyUnresolved
}

// backendOriginMatches reports whether a recorded descriptor belongs to this manager's
// endpoint and state root.
func (m *Manager) backendOriginMatches(k CredentialKey) bool {
	return k.BackendOrigin == strings.TrimRight(strings.TrimSpace(m.backendURL), "/") &&
		k.StateRoot == strings.TrimRight(strings.TrimSpace(m.stateRoot), "/")
}

// MarkIdentityLive records that this deployment answered a protected request under the
// current session — that the CREDENTIAL is still honoured, and nothing more.
//
// It changes no state, and that emptiness is the fix. This was MarkActive, and it
// promoted any signed-in state to StateSignedInActive, which means signed in AND
// ENTITLED and is the sole state CanSpend() is true for. But the only thing the caller
// knows is that some protected route returned 2xx, and nearly every protected route
// answers 2xx without ever consulting billing — /v1/daintree/capabilities does, and it
// runs at boot. So the first call of every session manufactured an entitlement out of a
// request that had never asked about one. That was most visible while entitlement
// enforcement was off or merely observing: the backend served everything, and every
// session therefore reported itself as granted.
//
// Entitlement now has exactly one source: a successfully decoded account-v1 body,
// through ApplyAccountStatus in accountsnapshot.go. This records the OTHER half, the
// half only the backend can supply — a stored credential proves a login happened, never
// that this deployment still honours it — and it lands in lastVerifiedAt, which Status
// renders as its own row beside, never instead of, the entitlement's own timestamp.
//
// The consequence is that a session which never performs an account read stays
// signed_in_unverified for its whole life, because nothing at boot asks. That reads as a
// regression and is not one: "signed in, plan not checked" is precisely what is known,
// and the state that used to be shown there was a guess. The unattended daemon is
// unaffected — its wake gate branches on StateRevoked/StateSignedOut, not on CanSpend.
func (m *Manager) MarkIdentityLive(gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != gen {
		// A confirmation for a session that has since been logged out. Applying it would
		// stamp a verification time onto an identity that is gone, and Status would
		// report the replacement session as freshly checked when it never was.
		return
	}
	// The generation is not enough on its own, and this is the belt to its braces. Not
	// every way a session ends advances it — a logout in ANOTHER process reaches this one
	// as a revision change, and a hydrate that finds the credential gone simply rewrites
	// the state. A success already in flight when that happened would otherwise land here
	// with a generation that still matches and vouch for a session the user has closed.
	if !m.state.SignedIn() {
		return
	}
	// m.lastErr is deliberately NOT cleared, which MarkActive did.
	//
	// Clearing was coherent there only because the same call also moved the state past
	// whatever the error explained. This one leaves the state alone, so clearing would
	// split the pair: a status reading `temporarily_unavailable` or `access_refused`
	// with no code beside it names a problem and then refuses to say which, and the
	// advice every surface renders is chosen from that pair. The error and the state it
	// produced are retired together, by the account read that supersedes both.
	now := m.now()
	m.lastVerifiedAt = &now
}

// Ensure Manager satisfies the backend seams at compile time.
var (
	_ backend.TokenSource   = (*Manager)(nil)
	_ backend.TokenScrubber = (*Manager)(nil)
)

// SeedForTest installs a credential and its descriptor without running a browser flow.
//
// Exported because the supervisor package needs a signed-in Manager to test its spend
// gate, and the alternative — reimplementing the store write and the descriptor format
// over there — would let the two drift apart silently. It is deliberately narrow: it
// cannot mint an access token, so a caller still cannot fake an authorized session.
func (m *Manager) SeedForTest(ctx context.Context, man *Manifest, refreshToken string) error {
	key := m.key(man)
	if err := m.ensureStore(ctx).Save(ctx, key, StoredSession{
		Version:      storedSessionVersion,
		RefreshToken: refreshToken,
		Issuer:       man.Issuer,
		ClientID:     man.ClientID,
		Environment:  man.Environment,
	}); err != nil {
		return err
	}
	return saveKeyRef(m.authDir, key)
}
