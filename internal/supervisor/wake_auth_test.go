package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/ipc"
)

// manifestServer serves a valid auth manifest on loopback.
//
// HERMETIC ON PURPOSE. An earlier version of this fixture pointed at the production URL,
// so Hydrate performed live discovery: the tests were slow, network-dependent, and — worse
// — a discovery failure resolved to "could not determine the account" rather than the
// signed-out state they meant to assert, which is how they passed for the wrong reason.
func manifestServer(t *testing.T) *httptest.Server {
	t.Helper()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daintree/auth/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"version":1,"environment":"development",
			"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,
			"client_id":"test-client","redirect_uri":%q,"scopes":["openid","email"]}`,
			base+"/auth/v1", base+"/auth/v1/oauth/authorize",
			base+"/auth/v1/oauth/token", base+"/auth/v1/.well-known/jwks.json",
			auth.RedirectURI())
	}))
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// activeSession puts the runtime's manager into a genuinely signed-in state, the way a
// real login does: a stored credential and a descriptor, hydrated, then confirmed by a
// backend request.
//
// MarkIdentityLive alone no longer suffices, and that is deliberate — a confirmation may only
// CONFIRM a session that exists, never revive one that does not, or a success already in
// flight when a logout landed would put the daemon back to work on a closed account.
func activeSession(t *testing.T, r *Runtime) {
	t.Helper()
	man, err := r.auth.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	seedLogin(t, r, man)
	r.auth.Hydrate(context.Background())
	r.auth.MarkIdentityLive(r.auth.Generation())
	if got := r.auth.State(); !got.SignedIn() {
		t.Fatalf("setup: state = %q, want a signed-in session", got)
	}
}

// newAuthRuntime builds a Runtime with a real Manager over an isolated state root and a
// loopback backend.
func newAuthRuntime(t *testing.T) *Runtime {
	r, _ := newAuthRuntimeWithPeer(t)
	return r
}

// newAuthRuntimeWithPeer returns a Runtime and a way to build a SECOND manager over the
// same state root and credential store — which is what "another process" means here.
// Simulating a logout elsewhere by reaching into this manager cannot distinguish the
// marker path from a local state write, and the marker path is the whole point.
func newAuthRuntimeWithPeer(t *testing.T) (*Runtime, func() *auth.Manager) {
	t.Helper()
	srv := manifestServer(t)
	root := t.TempDir()
	store := auth.NewMemoryStore()
	build := func() *auth.Manager {
		mgr, err := auth.NewManager(auth.Options{
			StateRoot:  root,
			BackendURL: srv.URL,
			Store:      store,
			Opener:     auth.NoOpener{},
		})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		return mgr
	}
	r := &Runtime{auth: build()}
	r.auth.Revision().MarkObserved(r.auth.Revision().Current())
	return r, build
}

// The regression the transition rule exists to prevent: an ordinary anonymous install,
// where the manager reaches signed-out ON ITS OWN the first time anything asks it for a
// credential. Blocking on that state alone stopped unattended work for every install
// after its first request.
func TestAnAnonymousInstallIsNotTreatedAsAnEndedSession(t *testing.T) {
	r := newAuthRuntime(t)
	// Exactly what a protected request does: ask for a credential. There is none, and
	// the manager records that.
	if tok, err := r.auth.AccessToken(context.Background()); err != nil || tok != "" {
		t.Fatalf("AccessToken = %q, %v — want the anonymous answer", tok, err)
	}
	if got := r.auth.State(); got != auth.StateSignedOut {
		t.Fatalf("setup: state = %q, want signed_out", got)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorizedToSpendLocked() {
		t.Fatal("an anonymous install was blocked — every install today would stop supervising after one request")
	}
	if r.lastError != "" {
		t.Errorf("a pause was reported for a session that never existed: %q", r.lastError)
	}
}

// An install with no account at all must keep supervising. The backend's open door
// serves anonymous requests, and refusing here would disable unattended work for every
// existing user — a far worse outcome than the one the gate exists to prevent.
func TestADaemonWithNoAccountLayerKeepsWorking(t *testing.T) {
	r := &Runtime{} // auth == nil: a deprecated caller key is set, or no auth dir
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorizedToSpendLocked() {
		t.Fatal("a daemon with no account layer refused to work — every current install would stop supervising")
	}
}

// The ORDINARY anonymous install: a real Manager that has simply never signed in. This is
// what every current user looks like, and it must keep supervising — the backend's open
// door serves anonymous requests.
//
// The nil-Manager test above does not cover this: nil is the configuration-absent case,
// not the never-signed-in one.
func TestAnAnonymousInstallKeepsSupervising(t *testing.T) {
	r := newAuthRuntime(t)
	r.mu.Lock()
	unknown := r.authorizedToSpendLocked()
	r.mu.Unlock()
	if !unknown {
		t.Fatal("a daemon that has not yet determined its account state refused to work")
	}
}

// A marker that moves for a LOGIN must not read as a logout. A test that only bumps and
// asserts "blocked" would pass an implementation that treats every bump as a sign-out.
func TestALoginBumpDoesNotPauseTheDaemon(t *testing.T) {
	r := newAuthRuntime(t)
	// Record a credential and a descriptor, as a real login does.
	man, err := r.auth.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	seedLogin(t, r, man)

	other := auth.NewRevision(r.auth.AuthDirPath())
	if err := other.Bump(context.Background()); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	r.refreshAuthPosture(context.Background())

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorizedToSpendLocked() {
		t.Fatal("a login bump paused the daemon — every bump is being read as a logout")
	}
}

// seedLogin writes a credential and its descriptor, standing in for a completed login.
func seedLogin(t *testing.T, r *Runtime, man *auth.Manifest) {
	t.Helper()
	if err := r.auth.SeedForTest(context.Background(), man, "refresh-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// THE gate. A wake turn is a paid request made with nobody watching, and after a logout
// this process still holds an access token that stays valid until its expiry. Without
// this check it would keep spending on an account the user believes is closed.
func TestASignedOutDaemonRefusesToSpend(t *testing.T) {
	r := newAuthRuntime(t)
	// A session that EXISTS, then ends — which is what the comment above describes and
	// what the gate is for. A manager that merely hydrates to signed-out has never had
	// one, and that is the anonymous install, covered separately.
	activeSession(t, r)
	r.mu.Lock()
	if !r.authorizedToSpendLocked() {
		t.Fatal("setup: an active daemon was blocked")
	}
	r.mu.Unlock()
	// The user signs out. Hydrate off the lock afterwards, exactly as the runtime does.
	if _, err := r.auth.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	r.auth.Hydrate(context.Background())

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.authorizedToSpendLocked() {
		t.Fatal("a signed-out daemon was allowed to make a paid request")
	}
	if !r.authPausedForStatus() {
		t.Fatal("status did not report the pause, so the daemon looks merely idle")
	}
}

// A logout that happened in ANOTHER process must reach this one. That is the entire
// reason the shared marker exists.
func TestALogoutElsewhereStopsTheDaemon(t *testing.T) {
	r, peer := newAuthRuntimeWithPeer(t)
	// Pretend this daemon is happily signed in.
	activeSession(t, r)
	r.mu.Lock()
	ok := r.authorizedToSpendLocked()
	r.mu.Unlock()
	if !ok {
		t.Fatal("an active daemon was blocked")
	}

	// Someone logs out in a terminal. A DIFFERENT process does it: it deletes the
	// credential and bumps the shared marker, and this daemon is told nothing directly.
	if _, err := peer().Logout(context.Background()); err != nil {
		t.Fatalf("logout elsewhere: %v", err)
	}

	// The daemon notices through the same path reactWake uses.
	r.refreshAuthPosture(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.authorizedToSpendLocked() {
		t.Fatal("the daemon kept spending after a logout it did not witness")
	}
}

// refreshAuthPosture does credential-store and possibly network I/O, so holding the
// runtime mutex across it would block the control socket, status replies and handover
// behind a call that can take seconds or prompt for a keychain password.
func TestTheAuthPostureRefreshDoesNotHoldTheRuntimeMutex(t *testing.T) {
	r := newAuthRuntime(t)
	done := make(chan struct{})
	go func() {
		r.refreshAuthPosture(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshAuthPosture did not return — it is holding a lock it should not")
	}
	// The mutex must be free the whole time.
	r.mu.Lock()
	r.mu.Unlock()
}

// The pause must be reported once per transition, not on every 3s tick, or it buries the
// rest of the log.
func TestThePauseIsReportedOncePerTransition(t *testing.T) {
	r := newAuthRuntime(t)
	// A session has to EXIST before it can end. Hydrating a never-signed-in manager
	// straight to signed-out is not a pause and must not report one — that is the
	// anonymous install, which keeps working.
	activeSession(t, r)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.authorizedToSpendLocked() {
		t.Fatal("setup: an active daemon was blocked")
	}
	r.mu.Unlock()
	// End it for real. activeSession stores a credential, so a bare Hydrate would find
	// it and correctly report the session as still live — there would be nothing to
	// pause about.
	if _, err := r.auth.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	r.mu.Lock()

	_ = r.authorizedToSpendLocked()
	first := r.lastError
	r.lastError = ""
	for i := 0; i < 5; i++ {
		_ = r.authorizedToSpendLocked()
	}
	if first == "" {
		t.Fatal("the pause was never reported at all")
	}
	if r.lastError != "" {
		t.Fatalf("the pause was re-reported on every check: %q", r.lastError)
	}
}

// The daemon's status must never carry personal data. Its authorization model is
// filesystem permissions on the state dir — the right boundary for coordination and the
// wrong one for an email address.
func TestTheDaemonStatusCarriesNoPersonalData(t *testing.T) {
	r := newAuthRuntime(t)
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.authStateForStatus()
	rev := r.authRevisionForStatus()
	for _, v := range []string{state, rev} {
		if containsAt(v) {
			t.Errorf("daemon status field %q looks like an email address", v)
		}
	}
	// The revision is a nonce and a counter, never a credential.
	if rev != "" && len(rev) > 64 {
		t.Errorf("the revision field is %d characters — that is not a marker", len(rev))
	}
}

func containsAt(s string) bool {
	for _, r := range s {
		if r == '@' {
			return true
		}
	}
	return false
}

// The daemon frames carry a MARKER, never a token. Adding a credential field to either
// struct is the specific mistake this design exists to avoid, so the check is on the
// SERIALISED shape — the previous version asserted a field equalled itself and would
// have passed with an account token added.
func TestTheDaemonFramesCarryNoAccountToken(t *testing.T) {
	for name, v := range map[string]any{
		"AuthChangedRequest": ipc.AuthChangedRequest{Revision: "abc123:7"},
		"Credentials":        ipc.Credentials{McpURL: "u", McpToken: "t", BackendURL: "b"},
		"StatusReply":        ipc.StatusReply{AuthState: "signed_out", AuthRevision: "abc:1"},
	} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		for k := range fields {
			lower := strings.ToLower(k)
			// mcpToken is the ONE token here and it is not an account credential — it is
			// a per-session Daintree grant the daemon has always carried.
			if lower == "mcptoken" {
				continue
			}
			for _, banned := range []string{"accesstoken", "refreshtoken", "authtoken", "bearer", "credential", "password", "secret"} {
				if strings.Contains(lower, banned) {
					t.Errorf("%s serialises %q — the daemon must never be handed an account credential", name, k)
				}
			}
		}
	}
	// And the marker itself must look like a marker, not a token.
	if rev := (ipc.AuthChangedRequest{Revision: "abc123:7"}).Revision; len(rev) > 64 {
		t.Errorf("revision %q is too long to be a nonce and a counter", rev)
	}
}

// Adding ReqAuthChanged must NOT bump the protocol version.
//
// The server rejects any mismatch outright, so a bump strands a freshly-upgraded CLI
// behind a still-running old daemon — attach fails AND `daemon stop` fails, leaving no
// supported way to recover. The request is best-effort by design (an old daemon answers
// "unknown request type" and stops on its own at the next marker poll), so the cost is
// real and the benefit nil.
func TestAddingAuthChangedDidNotBumpTheProtocolVersion(t *testing.T) {
	if ipc.ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d — bumping it strands a new CLI behind an old daemon it cannot even stop",
			ipc.ProtocolVersion)
	}
}
