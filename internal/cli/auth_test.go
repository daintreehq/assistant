package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/config"
)

func TestParseAuthActionAcceptsTheDocumentedWords(t *testing.T) {
	for word, want := range map[string]AuthAction{
		"login": AuthLogin, "signin": AuthLogin, "sign-in": AuthLogin, "LOGIN": AuthLogin,
		"status": AuthStatus, "account": AuthStatus,
		"logout": AuthLogout, "signout": AuthLogout, "sign-out": AuthLogout,
		"disconnect": AuthDisconnect,
	} {
		got, ok := ParseAuthAction(word)
		if !ok || got != want {
			t.Errorf("ParseAuthAction(%q) = %q,%v, want %q", word, got, ok, want)
		}
	}
	for _, bad := range []string{"", "  ", "revoke", "auth", "log in", "--json"} {
		if _, ok := ParseAuthAction(bad); ok {
			t.Errorf("ParseAuthAction(%q) was accepted", bad)
		}
	}
}

// THE contract for Daintree: stdout carries ONLY parseable events under --json. A stray
// human sentence there breaks a login in the desktop app, which reads the stream line by
// line.
func TestJSONModeKeepsHumanTextOffStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	w := authWriter{json: true, out: &out, err: &errBuf}

	w.event(authEvent{Type: "auth:starting", Env: "staging"})
	w.human("Signing in to Daintree…")
	w.human("Warning: no system credential store is available")
	w.event(authEvent{Type: "auth:authenticated"})

	for i, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e authEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("stdout line %d is not JSON: %q", i, line)
		}
		if e.V != authEventVersion {
			t.Errorf("line %d has version %d, want %d", i, e.V, authEventVersion)
		}
	}
	if !strings.Contains(errBuf.String(), "Signing in") {
		t.Error("human text did not reach stderr")
	}
	if strings.Contains(out.String(), "Signing in") {
		t.Error("human text reached stdout and would break the event stream")
	}
}

// In human mode the ordinary output belongs on stdout, or piping it anywhere is useless.
func TestHumanModeWritesToStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	w := authWriter{json: false, out: &out, err: &errBuf}
	w.event(authEvent{Type: "auth:starting"}) // must emit nothing at all
	w.human("Signed in.")

	if strings.Contains(out.String(), "auth:starting") {
		t.Error("an NDJSON event leaked into human output")
	}
	if !strings.Contains(out.String(), "Signed in.") {
		t.Error("human output did not reach stdout")
	}
}

// The event type has nowhere to put a credential, and specifically no field for the
// authorization URL — which carries a live PKCE-bound request and is exactly what a
// caller is most likely to log.
func TestTheEventTypeCannotCarryACredential(t *testing.T) {
	body, err := json.Marshal(authEvent{
		V: 1, Type: "auth:authenticated", Env: "staging",
		URL: "https://staging.daintree.org/account",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{
		"access_token", "accessToken", "refresh_token", "refreshToken",
		"token", "authorization_url", "authorizationUrl", "state", "code_verifier",
	} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("authEvent carries %q", forbidden)
		}
	}
}

func TestAuthUsageListsEveryAction(t *testing.T) {
	usage := AuthUsage()
	for _, action := range []string{"login", "status", "logout", "disconnect"} {
		if !strings.Contains(usage, action) {
			t.Errorf("usage does not mention %q", action)
		}
	}
}

// A user who signs in with no credential store must be TOLD their session will not
// survive exit — otherwise it reads as the assistant randomly forgetting them.
func TestStatusSurfacesANonPersistentSession(t *testing.T) {
	var out bytes.Buffer
	w := authWriter{json: false, out: &out, err: &out}
	renderAuthStatus(w, auth.Status{
		State:       auth.StateStorageUnavailable,
		BackendURL:  "https://assistant.daintree.org",
		StorageTier: auth.TierMemory,
	}, config.AppConfig{})

	body := out.String()
	if !strings.Contains(body, "will not persist") {
		t.Errorf("a memory-tier session did not warn the user:\n%s", body)
	}
	if !strings.Contains(body, "no system credential store") {
		t.Errorf("the storage tier was not explained:\n%s", body)
	}
}

// A plan problem must point at the plans page, not at a login the user already has.
func TestStatusPointsAPlanProblemAtBilling(t *testing.T) {
	var out bytes.Buffer
	w := authWriter{json: false, out: &out, err: &out}
	renderAuthStatus(w, auth.Status{
		State:       auth.StateSubscriptionRequired,
		BackendURL:  "https://assistant.daintree.org",
		StorageTier: auth.TierKeychain,
		Links:       auth.StatusLinks{Subscribe: "https://staging.daintree.org/subscribe"},
	}, config.AppConfig{})

	body := out.String()
	if !strings.Contains(body, "subscribe") {
		t.Errorf("a subscription-required state did not link the plans page:\n%s", body)
	}
	if strings.Contains(body, "auth login") {
		t.Errorf("a subscription-required state told the user to sign in, which they already are:\n%s", body)
	}
}

func TestStatusTellsASignedOutUserWhatToRun(t *testing.T) {
	var out bytes.Buffer
	w := authWriter{json: false, out: &out, err: &out}
	renderAuthStatus(w, auth.Status{State: auth.StateSignedOut, StorageTier: auth.TierKeychain}, config.AppConfig{})
	if !strings.Contains(out.String(), "auth login") {
		t.Errorf("a signed-out status did not name the command to run:\n%s", out.String())
	}
}

// A set DAINTREE_API_KEY silently overrides account identity, so an install with both is
// one where the key wins. Anyone reading status has to know that.
func TestStatusFlagsTheDeprecatedKeyOnlyWhenSet(t *testing.T) {
	var withKey bytes.Buffer
	w := authWriter{json: false, out: &withKey, err: &withKey}
	renderAuthStatus(w, auth.Status{State: auth.StateSignedInActive, StorageTier: auth.TierKeychain},
		config.AppConfig{APIKey: "sk-test-fake-value-for-tests"})
	if !strings.Contains(withKey.String(), "DAINTREE_API_KEY") {
		t.Errorf("a set deprecated key was not flagged:\n%s", withKey.String())
	}
	if !strings.Contains(withKey.String(), "deprecated") {
		t.Errorf("the key was mentioned without saying it is deprecated:\n%s", withKey.String())
	}
	// ...and it must NOT be advertised to the overwhelming majority who do not set it.
	var without bytes.Buffer
	w2 := authWriter{json: false, out: &without, err: &without}
	renderAuthStatus(w2, auth.Status{State: auth.StateSignedInActive, StorageTier: auth.TierKeychain},
		config.AppConfig{})
	if strings.Contains(without.String(), "DAINTREE_API_KEY") {
		t.Errorf("a path being retired was advertised to an install that does not use it:\n%s", without.String())
	}
}

// The value must never be printed, only its presence.
func TestStatusNeverPrintsTheDeprecatedKeyItself(t *testing.T) {
	const key = "sk-test-fake-not-a-real-credential"
	var out bytes.Buffer
	w := authWriter{json: false, out: &out, err: &out}
	renderAuthStatus(w, auth.Status{State: auth.StateSignedInActive, StorageTier: auth.TierKeychain},
		config.AppConfig{APIKey: key})
	if strings.Contains(out.String(), key) {
		t.Fatalf("the key value was printed:\n%s", out.String())
	}
}

func TestEveryStateHasAHumanLabel(t *testing.T) {
	for _, s := range []auth.State{
		auth.StateUnknown, auth.StateSignedOut, auth.StateAuthorizing,
		auth.StateSignedInUnverified, auth.StateSignedInActive,
		auth.StateSubscriptionRequired, auth.StateSubscriptionInactive,
		auth.StateRefreshing, auth.StateTemporarilyUnavailable,
		auth.StateRevoked, auth.StateStorageUnavailable,
		auth.StateAccountsUnavailable, auth.StateAccessRefused,
	} {
		label := authStateLabel(s)
		if label == "" {
			t.Errorf("%s has no label", s)
		}
		// A raw enum value leaking into human output means a state was added without a
		// sentence being written for it.
		if label == string(s) && s != auth.StateUnknown {
			t.Errorf("%s renders as its raw enum value — a state was added without a sentence for it", s)
		}
	}
}

// A relative duration is what the user wants; the token behind it is never shown.
func TestStatusRendersExpiryAsARelativeTime(t *testing.T) {
	exp := time.Now().Add(42 * time.Minute)
	var out bytes.Buffer
	w := authWriter{json: false, out: &out, err: &out}
	renderAuthStatus(w, auth.Status{
		State: auth.StateSignedInActive, StorageTier: auth.TierKeychain,
		AccessExpiresAt: &exp,
	}, config.AppConfig{})
	body := out.String()
	if !strings.Contains(body, "renews in") {
		t.Errorf("the session expiry was not rendered:\n%s", body)
	}
	if strings.Contains(body, exp.Format(time.RFC3339)) {
		t.Errorf("an absolute timestamp was rendered where a relative one was intended:\n%s", body)
	}
}
