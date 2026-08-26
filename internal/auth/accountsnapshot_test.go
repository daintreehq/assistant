package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/backend/accountfixture"
)

// accountsnapshot_test.go covers the memory-only account view: what the backend said,
// how it lands on the local state, and — the half that matters most — every way it must
// STOP being said.
//
// The tests drive a real backend.Client against a real Manager wherever the seam is the
// point, because the failure this whole layer exists to prevent is a state machine whose
// transitions are all tested and never reached.

// activeAccountBody is THE canonical granted response, not a copy of it. Four packages
// decode this contract, and four hand-written examples of one response is how both ends
// of it came to be green against documents that did not match.
var activeAccountBody = accountfixture.String(accountfixture.GrantedStandard)

// statusFor builds an AccountStatus the way the client would hand one over.
func statusFor(t *testing.T, access, plan, source string) backend.AccountStatus {
	t.Helper()
	// CONTRACT-VALID, not merely field-complete. ApplyAccountStatus takes an
	// already-decoded value and does not re-validate, so a helper is free to build a
	// response no backend could send — and then every test resting on it is exercising a
	// shape that will never arrive. The freshness pointer is the one that matters: a
	// checked verdict must SAY whether its answer is stale, and a nil here would model
	// the body the decoder now refuses.
	fresh := false
	st := backend.AccountStatus{
		Version:            backend.AccountStatusVersion,
		Email:              accountfixture.Email,
		SubjectHash:        "0123456789abcdef",
		Access:             access,
		PlanID:             plan,
		SubscriptionStatus: "active",
		EntitlementSource:  source,
		EntitlementStale:   &fresh,
		CheckedAt:          accountfixture.CheckedAt,
	}
	parsed, err := time.Parse(time.RFC3339, st.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	st.CheckedAtTime = parsed
	return st
}

// markStale flags a decoded status as served from an aged cache.
//
// A helper rather than an inline address-of, because EntitlementStale is a POINTER on the
// wire type: the contract distinguishes "we checked and it is fresh" from "we never
// asked", and only a pointer can carry that difference. Saying it once here keeps every
// call site reading as the intent — stale — rather than as pointer mechanics.
func markStale(st backend.AccountStatus) backend.AccountStatus {
	stale := true
	st.EntitlementStale = &stale
	return st
}

// signedInManager returns a Manager already holding a hydrated session.
func signedInManager(t *testing.T) *Manager {
	t.Helper()
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	if !m.Hydrate(context.Background()) {
		t.Fatal("the seeded session did not hydrate")
	}
	return m
}

// Each access verdict lands on its own local state, and each carries the display fields
// through to the status a human and Daintree both read.
func TestAccountStatusProjectsEveryVerdict(t *testing.T) {
	cases := []struct {
		access, plan, source string
		want                 State
	}{
		{backend.AccessGranted, backend.PlanStandard, backend.EntitlementSourcePolar, StateSignedInActive},
		{backend.AccessSubscriptionRequired, "", "", StateSubscriptionRequired},
		{backend.AccessSubscriptionInactive, backend.PlanPro, backend.EntitlementSourcePolar, StateSubscriptionInactive},
		{backend.AccessUnverified, "", "", StateSignedInUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.access, func(t *testing.T) {
			m := signedInManager(t)
			m.ApplyAccountStatus(m.Generation(), statusFor(t, tc.access, tc.plan, tc.source))

			st := m.Status()
			if st.State != tc.want {
				t.Errorf("state = %q, want %q", st.State, tc.want)
			}
			if st.Email != accountfixture.Email {
				t.Errorf("email = %q", st.Email)
			}
			if st.SubjectHash != "0123456789abcdef" {
				t.Errorf("subjectHash = %q", st.SubjectHash)
			}
			if st.Plan != tc.plan {
				t.Errorf("plan = %q, want %q", st.Plan, tc.plan)
			}
			if st.EntitlementSource != tc.source {
				t.Errorf("entitlementSource = %q, want %q", st.EntitlementSource, tc.source)
			}
			if st.LastVerifiedAt == nil {
				t.Error("a live answer left lastVerifiedAt unset")
			}
			// The subscription verdicts are LOGINS. Reporting them as unauthenticated
			// would send someone through a browser flow to reach the identical 402.
			if !st.Authenticated {
				t.Errorf("state %q reported as not authenticated", st.State)
			}
		})
	}
}

// A verdict this build has no name for is "signed in, not verified" — the identity is
// good (the request was authenticated) and the entitlement is a word we cannot read.
// Anything more definite would either grant access on an unknown or revoke it on one.
func TestAnUnrecognisedAccessVerdictIsUnverifiedNotRefused(t *testing.T) {
	if got := StateForAccess("some_future_verdict"); got != StateSignedInUnverified {
		t.Errorf("StateForAccess(unknown) = %q, want %q", got, StateSignedInUnverified)
	}
	if got := StateForAccess(""); got != StateSignedInUnverified {
		t.Errorf("StateForAccess(\"\") = %q, want %q", got, StateSignedInUnverified)
	}
}

// A stale answer is marked stale. It is a materially different claim from a fresh one —
// "we believe you are subscribed, as of some minutes ago" — and a user deciding whether
// to trust it needs to be told which they are looking at.
func TestAStaleEntitlementIsReportedAsStale(t *testing.T) {
	m := signedInManager(t)
	st := statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourceCache)
	st = markStale(st)
	m.ApplyAccountStatus(m.Generation(), st)

	got := m.Status()
	if !got.EntitlementStale {
		t.Error("a cached answer was reported as fresh")
	}
	if got.EntitlementSource != backend.EntitlementSourceCache {
		t.Errorf("entitlementSource = %q", got.EntitlementSource)
	}
}

// A verdict for a session that has ended must not land. This is the same guard
// ApplyBackendVerdict carries, and it is here because a status read can outlive a logout
// — at which point applying it would show an account, an email and a plan on a machine
// that holds no credential at all.
func TestAStaleGenerationIsIgnoredAfterLogout(t *testing.T) {
	m := signedInManager(t)
	gen := m.Generation()

	if _, err := m.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	m.ApplyAccountStatus(gen, statusFor(t, backend.AccessGranted, backend.PlanStandard, backend.EntitlementSourcePolar))

	st := m.Status()
	if st.State != StateSignedOut {
		t.Errorf("state = %q, want %q — a pre-logout verdict was applied", st.State, StateSignedOut)
	}
	if st.Email != "" || st.Plan != "" {
		t.Errorf("the previous account survived a logout: email=%q plan=%q", st.Email, st.Plan)
	}
}

// The snapshot must not survive a session ending, whichever way it ends.
//
// Every case drives a REAL lifecycle path rather than incrementing the generation by
// hand. A test that bumps the counter itself proves only that the comparison works — it
// would pass unchanged against a build in which no production path ever bumped it, which
// is exactly the bug worth catching.
func TestASnapshotDoesNotSurviveASessionEnding(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, m *Manager)
	}{
		{"logout", func(t *testing.T, m *Manager) {
			if _, err := m.Logout(context.Background()); err != nil {
				t.Fatalf("Logout: %v", err)
			}
		}},
		{"revocation", func(t *testing.T, m *Manager) {
			m.ApplyBackendVerdict(context.Background(), m.Generation(), "",
				&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthSessionRevoked, Message: "gone"})
		}},
		{"the backend asks for a fresh sign-in", func(t *testing.T, m *Manager) {
			m.ApplyBackendVerdict(context.Background(), m.Generation(), "",
				&backend.Error{HTTPStatus: 401, Code: backend.CodeAuthRequired, Message: "none"})
		}},
		{"the credential disappears from the store", func(t *testing.T, m *Manager) {
			m.mu.Lock()
			store := m.store
			m.mu.Unlock()
			key, ok := m.currentKey(context.Background())
			if !ok {
				t.Fatal("could not resolve the credential key")
			}
			if err := store.Delete(context.Background(), key); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			_ = forgetKeyRef(m.AuthDirPath())
			m.Hydrate(context.Background())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := signedInManager(t)
			m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))
			if m.Status().Plan != backend.PlanPro {
				t.Fatal("the snapshot did not apply in the first place")
			}

			tc.end(t, m)

			st := m.Status()
			if st.Plan != "" || st.Email != "" || st.SubjectHash != "" ||
				st.EntitlementSource != "" || st.EntitlementStale || st.EntitlementCheckedAt != nil {
				t.Errorf("a snapshot outlived its session: %+v", st)
			}
		})
	}
}

// Another process swapping the ACCOUNT is the case the generation counter cannot see on
// its own: this manager's counter never moves, and a plain Hydrate finds a perfectly
// valid credential and settles a signed-in state. Without the revision check, the
// previous account's email and plan would render under the new one's session.
func TestASnapshotDoesNotSurviveAnAccountSwapInAnotherProcess(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	m.Hydrate(context.Background())
	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))
	if m.Status().Plan != backend.PlanPro {
		t.Fatal("the snapshot did not apply in the first place")
	}

	// What another process's logout-and-login looks like from here: the shared marker
	// moves, and a credential is still present. Nothing local changes.
	if err := m.Revision().Bump(context.Background()); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	m.Hydrate(context.Background())

	st := m.Status()
	if !st.State.SignedIn() {
		t.Fatalf("the swap left no session at all, so this proves nothing: %q", st.State)
	}
	if st.Plan != "" || st.Email != "" || st.SubjectHash != "" {
		t.Errorf("the previous account rendered under a swapped session: %+v", st)
	}
}

// A confirmation can only ever CONFIRM. It promotes a session that still exists and
// never revives one that does not — otherwise a status read racing a logout puts the
// user back to signed-in with no credential behind it.
func TestAnAccountStatusCannotResurrectASignedOutSession(t *testing.T) {
	m := signedInManager(t)
	m.mu.Lock()
	m.state = StateSignedOut
	gen := m.generation
	m.mu.Unlock()

	m.ApplyAccountStatus(gen, statusFor(t, backend.AccessGranted, backend.PlanStandard, backend.EntitlementSourcePolar))
	if got := m.State(); got != StateSignedOut {
		t.Errorf("state = %q, want %q", got, StateSignedOut)
	}
}

// A live answer supersedes whatever last went wrong. Without this, a status that has
// just been confirmed still carries the error code from the outage that preceded it,
// which reads as an account still in trouble.
func TestAFreshAccountAnswerClearsTheLastError(t *testing.T) {
	m := signedInManager(t)
	m.ApplyBackendVerdict(context.Background(), m.Generation(), "",
		&backend.Error{HTTPStatus: 503, Code: backend.CodeEntitlementUnavailable, Message: "billing down"})
	if m.Status().LastErrorCode == "" {
		t.Fatal("the dependency failure was not recorded")
	}

	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanStandard, backend.EntitlementSourcePolar))
	if code := m.Status().LastErrorCode; code != "" {
		t.Errorf("lastErrorCode = %q after a successful check", code)
	}
}

// A later outage keeps the display fields but must not keep claiming they are current.
// Blanking the plan here would tell someone their subscription went away because their
// network did; the honest reading is "this is what we last knew, and here is when".
func TestASnapshotIsRetainedThroughADependencyOutage(t *testing.T) {
	m := signedInManager(t)
	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))

	m.ApplyBackendVerdict(context.Background(), m.Generation(), "",
		&backend.Error{HTTPStatus: 503, Code: backend.CodeEntitlementUnavailable, Message: "billing down"})

	st := m.Status()
	if st.State != StateTemporarilyUnavailable {
		t.Errorf("state = %q, want %q", st.State, StateTemporarilyUnavailable)
	}
	if st.Plan != backend.PlanPro || st.Email == "" {
		t.Errorf("the outage discarded what was last known: %+v", st)
	}
	if st.LastVerifiedAt == nil {
		t.Error("lastVerifiedAt was dropped, so nothing says how old the plan is")
	}
	// An outage is never a statement about the subscription.
	if st.State.NeedsPlan() || st.State.NeedsLogin() {
		t.Error("a dependency outage was rendered as a plan or login problem")
	}
}

// ForgetAccountStatus drops the view without touching the credential — for a caller with
// positive reason to distrust what it holds.
func TestForgetAccountStatusKeepsTheCredential(t *testing.T) {
	m := signedInManager(t)
	stale := statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourceCache)
	stale = markStale(stale)
	m.ApplyAccountStatus(m.Generation(), stale)
	m.ForgetAccountStatus()

	st := m.Status()
	if st.Plan != "" || st.Email != "" || st.SubjectHash != "" || st.EntitlementSource != "" ||
		st.EntitlementStale || st.EntitlementCheckedAt != nil {
		t.Errorf("the snapshot survived: %+v", st)
	}
	if !st.State.SignedIn() {
		t.Errorf("forgetting the account signed the user out: %q", st.State)
	}
}

// A deployment that says it has no accounts overrides everything local — including any
// account fields left from when it did have them. An email beside "this backend has no
// accounts" is not a partial truth; it is two statements that cannot both hold.
func TestAccountFieldsAreDroppedWhenTheDeploymentHasNoAccounts(t *testing.T) {
	m := signedInManager(t)
	// A STALE cached grant, so every projected field starts non-zero. Starting from
	// `entitlementStale: false` would let a WithAvailability that forgot that one field
	// pass unnoticed.
	stale := statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourceCache)
	stale = markStale(stale)
	m.ApplyAccountStatus(m.Generation(), stale)
	if before := m.Status(); before.Email == "" || before.Plan == "" || before.SubjectHash == "" ||
		before.EntitlementSource == "" || !before.EntitlementStale || before.EntitlementCheckedAt == nil {
		t.Fatalf("the priming snapshot left a field zero, so clearing it proves nothing: %+v", before)
	}

	st := m.Status().WithAvailability(Availability{Known: true, Configured: false})
	if st.State != StateAccountsUnavailable {
		t.Errorf("state = %q, want %q", st.State, StateAccountsUnavailable)
	}
	if st.Email != "" || st.Plan != "" || st.SubjectHash != "" || st.EntitlementSource != "" ||
		st.EntitlementStale || st.EntitlementCheckedAt != nil {
		t.Errorf("account fields survived a deployment with no accounts: %+v", st)
	}
}

// An availability we could not establish writes nothing — including nothing that would
// discard a good snapshot on the strength of a network failure.
func TestAnUnknownAvailabilityLeavesTheSnapshotAlone(t *testing.T) {
	m := signedInManager(t)
	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))

	st := m.Status().WithAvailability(Availability{Known: false})
	if st.Plan != backend.PlanPro {
		t.Errorf("an unknown availability discarded the plan: %+v", st)
	}
}

// No account DETAIL reaches disk — no email, no plan, no entitlement verdict. A plan on
// disk is a plan that can be wrong, and a fresh process must ask again, which is the whole
// point of `--refresh`. The credential itself is a different matter and does persist: the
// refresh token in the OS keychain, plus a non-secret descriptor naming which credential
// it is.
func TestAnAccountSnapshotIsNeverPersisted(t *testing.T) {
	p := newIDP(t)
	store := NewMemoryStore()
	m := newManager(t, p, store)
	storedFor(t, m, p, store, "refresh-seed")
	m.Hydrate(context.Background())
	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))

	// A second Manager over the SAME state root and the SAME store: everything durable
	// is visible to it, and the account must not be.
	fresh, err := NewManager(Options{StateRoot: m.stateRoot, BackendURL: p.srv.URL, Store: store, Opener: NoOpener{}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	fresh.Hydrate(context.Background())

	st := fresh.Status()
	if st.Plan != "" || st.Email != "" || st.SubjectHash != "" {
		t.Fatalf("account details survived into a new process: %+v", st)
	}
	// The credential DID survive — otherwise this test would pass for the wrong reason.
	if !st.State.SignedIn() {
		t.Fatalf("the credential itself did not survive: %q", st.State)
	}
}

// End to end through a real client: the status read reaches the backend, the answer
// lands, and the endpoint's own 200 does not confirm anything on its own.
func TestAccountReadThroughARealClientLandsOnTheManager(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())
	d.scriptAccount(accountBody(activeAccountBody))

	gen := m.Generation()
	st, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	m.ApplyAccountStatus(gen, st)

	got := m.Status()
	if got.State != StateSignedInActive {
		t.Errorf("state = %q, want %q", got.State, StateSignedInActive)
	}
	if got.Plan != backend.PlanStandard || got.Email != accountfixture.Email {
		t.Errorf("the account did not reach the manager: %+v", got)
	}
	if n := d.accountCalls.Load(); n != 1 {
		t.Errorf("account requests = %d, want exactly 1", n)
	}
}

// A 200 carrying `subscription_required` must leave the user needing a PLAN, not a
// login — and must not pass through "active" on the way, which is what the transport's
// usual confirm-on-2xx would have produced.
func TestAPlanlessAccountReadNeverReportsAnActiveSession(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())
	d.scriptAccount(accountBody(
		`{"version":1,"email":"` + accountfixture.Email + `","subject_hash":"0123456789abcdef",` +
			`"access":"subscription_required","subscription_status":"none",` +
			`"entitlement_source":"polar","entitlement_stale":false,` +
			`"checked_at":"` + accountfixture.CheckedAt + `"}`))

	gen := m.Generation()
	st, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	// The transport observed nothing, so before the verdict is applied the state is
	// still whatever hydration settled on — never "active".
	if got := m.State(); got == StateSignedInActive {
		t.Fatal("the status read confirmed an active session on its own 200")
	}
	m.ApplyAccountStatus(gen, st)

	got := m.Status()
	if got.State != StateSubscriptionRequired {
		t.Errorf("state = %q, want %q", got.State, StateSubscriptionRequired)
	}
	if !got.State.NeedsPlan() || got.State.NeedsLogin() {
		t.Error("a missing plan was rendered as a login problem")
	}
	// The credential is untouched: a plan problem is not a credential problem.
	if !got.State.SignedIn() {
		t.Error("a missing plan discarded the session")
	}
	// And the daemon must not spend against it.
	if got.State.CanSpend() {
		t.Error("an unsubscribed account was cleared to spend")
	}
}

// A malformed body preserves the credential and reports nothing new. It must never be
// reinterpreted as signed out or unsubscribed.
func TestAMalformedAccountBodyPreservesTheSession(t *testing.T) {
	d := newDeployment(t)
	m, c, key := signedIn(t, d, NewMemoryStore())
	d.scriptAccount(accountBody(`{"version":9,"access":"granted","plan_id":"pro"}`))

	// Prime the session first. Obtaining an access token is itself a state transition
	// (unknown → signed_in_unverified), so measuring from before it would attribute
	// that move to the malformed body and pass or fail for the wrong reason.
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	// Prime a GOOD snapshot first. Without one the test proves only that nothing
	// changed from nothing — it would pass against an implementation that wiped every
	// field it touched.
	gen := m.Generation()
	m.ApplyAccountStatus(gen, statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))
	primedStatus := m.Status()
	if primedStatus.EntitlementCheckedAt == nil {
		t.Fatal("the priming snapshot did not apply")
	}
	primed := *primedStatus.EntitlementCheckedAt
	before := m.State()
	if before != StateSignedInActive {
		t.Fatalf("the session did not settle before the read: %q", before)
	}
	st, err := c.Account(context.Background())
	if err == nil {
		t.Fatal("a malformed body was accepted")
	}
	// The caller applies the typed error, not the empty status — exactly as the command
	// will. Neither may move the session.
	m.ApplyBackendVerdict(context.Background(), gen, "", err)
	// The command applies the (empty) status on the error path too. It must be inert:
	// mapping a zero value to "unverified" would record a verification that never
	// happened and drop a plan that was correctly known a moment ago.
	m.ApplyAccountStatus(gen, st)

	got := m.Status()
	if got.State != before {
		t.Errorf("state moved from %q to %q on a malformed body", before, got.State)
	}
	if got.State.NeedsLogin() || got.State.NeedsPlan() {
		t.Error("a backend contract failure was rendered as a login or plan problem")
	}
	if got.Plan != backend.PlanPro || got.Email == "" {
		t.Errorf("a malformed body erased what was correctly known: %+v", got)
	}
	if got.EntitlementCheckedAt == nil || !got.EntitlementCheckedAt.Equal(primed) {
		t.Errorf("a malformed body restamped the entitlement check time: %v", got.EntitlementCheckedAt)
	}
	if !storedCredentialExists(t, m, key) {
		t.Error("a malformed body deleted the stored credential")
	}
}

// A revocation on the status route still clears the credential. Suppressing the SUCCESS
// observation must not have suppressed the failure one — that would leave a dead
// credential on disk and a daemon believing in a session the backend has ended.
func TestARevocationOnTheStatusRouteStillClearsTheCredential(t *testing.T) {
	d := newDeployment(t)
	store := NewMemoryStore()
	m, c, key := signedIn(t, d, store)
	d.scriptAccount(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","code":"auth_session_revoked","message":"gone"}}`))
	})

	gen := m.Generation()
	_, err := c.Account(context.Background())
	if err == nil {
		t.Fatal("a revoked session reported an account")
	}
	m.ApplyBackendVerdict(context.Background(), gen, "", err)

	if got := m.State(); got != StateRevoked {
		t.Errorf("state = %q, want %q", got, StateRevoked)
	}
	if storedCredentialExists(t, m, key) {
		t.Error("a revoked session left its credential on disk")
	}
}

// storedCredentialExists reports whether the credential is still in the store.
func storedCredentialExists(t *testing.T, m *Manager, key CredentialKey) bool {
	t.Helper()
	_, err := m.ensureStore(context.Background()).Load(context.Background(), key)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrNotFound) {
		return false
	}
	t.Fatalf("Load: %v", err)
	return false
}

// The regression that matters most: an ordinary protected 2xx must not promote a session
// the account endpoint has just reported as unsubscribed.
//
// The sequence is routine, not contrived. /v1/daintree/capabilities answers 200 whatever
// the plan says, and it runs at boot — so before this was guarded, a user with no
// subscription went from a correct `signed_in_subscription_required` to `signed_in_active`
// on the next thing the CLI did, taking NeedsPlan() and CanSpend() with it.
func TestAnUnrelatedSuccessCannotEraseAPlanVerdict(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())
	d.scriptAccount(accountBody(
		`{"version":1,"email":"` + accountfixture.Email + `","subject_hash":"0123456789abcdef",` +
			`"access":"subscription_required","subscription_status":"none",` +
			`"entitlement_source":"polar","entitlement_stale":false,` +
			`"checked_at":"` + accountfixture.CheckedAt + `"}`))

	gen := m.Generation()
	st, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	m.ApplyAccountStatus(gen, st)
	if got := m.State(); got != StateSubscriptionRequired {
		t.Fatalf("state = %q before the second call, want %q", got, StateSubscriptionRequired)
	}

	// Anything else protected. Capabilities is the real one that runs at boot.
	d.script(okJSON)
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	got := m.Status()
	if got.State != StateSubscriptionRequired {
		t.Errorf("state = %q after an unrelated success, want %q", got.State, StateSubscriptionRequired)
	}
	if !got.State.NeedsPlan() {
		t.Error("the account stopped needing a plan because an unrelated endpoint said 200")
	}
	if got.State.CanSpend() {
		t.Error("an unsubscribed account was cleared to spend by an unrelated 200")
	}
	// The success is still worth something: it confirms the credential, so the session
	// liveness time moves even though the plan verdict does not.
	if got.LastVerifiedAt == nil {
		t.Error("a successful protected request recorded no verification at all")
	}
}

// A success DOES still promote an ordinary unverified session — the guard above is
// narrow, and widening it to every state would strand a perfectly good login at
// "unverified" forever.
func TestAnUnrelatedSuccessStillConfirmsAnUnverifiedSession(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got := m.State(); got != StateSignedInUnverified {
		t.Fatalf("state = %q before the call", got)
	}

	d.script(okJSON)
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got := m.State(); got != StateSignedInActive {
		t.Errorf("state = %q, want %q — a confirmed session stayed unverified", got, StateSignedInActive)
	}
}

// The entitlement's own timestamp is the BACKEND's, and an unrelated success must not
// make an old plan look freshly checked. The two times answer different questions and
// drift apart in exactly the direction that misleads.
func TestTheEntitlementTimeIsTheBackendsAndDoesNotDrift(t *testing.T) {
	d := newDeployment(t)
	m, c, _ := signedIn(t, d, NewMemoryStore())
	d.scriptAccount(accountBody(activeAccountBody))

	gen := m.Generation()
	st, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	m.ApplyAccountStatus(gen, st)

	want, err := time.Parse(time.RFC3339, accountfixture.CheckedAt)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Status()
	if got.EntitlementCheckedAt == nil || !got.EntitlementCheckedAt.Equal(want) {
		t.Fatalf("entitlementCheckedAt = %v, want the backend's %v", got.EntitlementCheckedAt, want)
	}

	// A later protected success moves session liveness and nothing else.
	d.script(okJSON)
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	after := m.Status()
	if after.EntitlementCheckedAt == nil || !after.EntitlementCheckedAt.Equal(want) {
		t.Errorf("an unrelated success restamped the entitlement: %v", after.EntitlementCheckedAt)
	}
}

// The snapshot survives an ordinary token rotation. A refresh is the SAME OAuth session,
// so treating it as an identity change would blank the plan roughly once an hour.
func TestASnapshotSurvivesAnOrdinaryTokenRefresh(t *testing.T) {
	m := signedInManager(t)
	m.ApplyAccountStatus(m.Generation(), statusFor(t, backend.AccessGranted, backend.PlanPro, backend.EntitlementSourcePolar))

	tok, err := m.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	m.Invalidate(tok)
	if _, err := m.AccessToken(context.Background()); err != nil {
		t.Fatalf("AccessToken after invalidate: %v", err)
	}

	if got := m.Status(); got.Plan != backend.PlanPro {
		t.Errorf("a token rotation discarded the plan: %+v", got)
	}
}
