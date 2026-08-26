package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// accountfault_test.go pins what /login, /logout and /account say when this session has no
// account manager — three causes that used to collapse into two sentences.
//
// The one that was missing is a state root the auth directory could not be created under.
// It rendered as "Accounts are not available in this session.", which is a claim about the
// DEPLOYMENT: the reader goes looking for a backend with no identity provider while the
// fault is a file on their own disk.

// brokenStateRoot puts a regular FILE where the auth directory has to be created, which
// fails for every user including root — unlike a read-only directory, which proves
// nothing in a container running as uid 0.
func brokenStateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the blocking file: %v", err)
	}
	return root
}

func TestNoAccountManagerTextNamesAnUnbuildableStateRoot(t *testing.T) {
	a := &app.App{Config: config.AppConfig{
		StateRoot:  brokenStateRoot(t),
		BackendURL: "https://assistant.daintree.org",
	}}

	got := noAccountManagerText(a)
	if got == "Accounts are not available in this session." {
		t.Fatal("a broken auth state root still renders as a deployment with no accounts")
	}
	// The three things this copy owes the reader: what happened, that it is local, and
	// what to do next.
	for _, want := range []string{"auth state directory", "not on the backend", "doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("copy is missing %q:\n%s", want, got)
		}
	}
	// It must not promise that turns keep working. They do on an open deployment and they
	// do not on one that requires an account, and this session has no credential either
	// way — a reassurance that is false half the time reads as "ignore this".
	if strings.Contains(got, "turns still work") {
		t.Errorf("the card promises working turns on a deployment that may require an account:\n%s", got)
	}
	// The local error CODE is deliberately not rendered: creating the directory is
	// wrapped as `auth_exchange_failed`, and no token exchange was attempted.
	if strings.Contains(got, "auth_exchange_failed") {
		t.Errorf("the raw error code leaked into the card:\n%s", got)
	}
	// The state root is a doctor detail, not a turn's prose.
	if strings.Contains(got, a.Config.StateRoot) {
		t.Errorf("the state-root path leaked into a card rendered in the conversation:\n%s", got)
	}
}

// The caller-bearer case is tested SEPARATELY from the broken root, because keeping them
// apart is the entire point: a deliberate override must never be reported as a fault.
func TestNoAccountManagerTextKeepsTheCallerKeyBranch(t *testing.T) {
	a := &app.App{Config: config.AppConfig{
		StateRoot:  brokenStateRoot(t),
		BackendURL: "https://assistant.daintree.org",
		APIKey:     "fake-caller-key-for-tests",
	}}

	got := noAccountManagerText(a)
	if !strings.Contains(got, "DAINTREE_API_KEY") {
		t.Fatalf("the caller-key branch stopped winning:\n%s", got)
	}
	// Even with a root that would fault, an operator who configured this must not be told
	// their machine is broken.
	if strings.Contains(got, "auth state directory") {
		t.Errorf("a deliberate caller key was reported as a local fault:\n%s", got)
	}
}

// No App at all is not a machine fault and must keep the plain sentence — the host asks
// these commands before a session exists.
func TestNoAccountManagerTextWithoutAnAppStaysGeneric(t *testing.T) {
	if got := noAccountManagerText(nil); got != "Accounts are not available in this session." {
		t.Fatalf("a nil App produced %q", got)
	}
}

// A construction fault reaching the REFRESH path renders as a fault, not as a transient
// re-check failure. It arrives here only through the narrow `/backend` race — the manager
// is replaced between a command's own nil check and the read — and the generic branch
// would dress it up as "could not be re-checked just now (auth_exchange_failed: …)", which
// invites a retry that cannot succeed and names an exchange nothing attempted.
func TestRefreshNoteRendersAConstructionFaultAsALocalFault(t *testing.T) {
	fault := &app.AccountLayerFaultError{Cause: app.ErrAccountLayerUnbuilt}
	got := refreshNote(app.AccountRefresh{Err: fault}, auth.Status{})
	if strings.Contains(got, "could not be re-checked") {
		t.Errorf("a construction fault rendered as a transient re-check failure:\n%s", got)
	}
	if !strings.Contains(got, "not on the backend") {
		t.Errorf("the note does not place the fault on this machine:\n%s", got)
	}
}

// An ordinary refresh failure keeps the transient wording — the point of the branch above
// is that it is NARROW, and swallowing every error into "fix your machine" would misreport
// a billing outage as a local fault.
func TestRefreshNoteKeepsTheTransientWordingForAnOrdinaryFailure(t *testing.T) {
	got := refreshNote(app.AccountRefresh{Err: errors.New("upstream timed out")}, auth.Status{})
	if !strings.Contains(got, "could not be re-checked") {
		t.Errorf("an ordinary read failure lost its transient wording:\n%s", got)
	}
}

// A settled refusal is not a failed check, and the note must not invite the retry that
// wording implies.
//
// The 403 the backend returns for a valid identity a private deployment has not approved
// is an ANSWER. "Could not be re-checked just now" says the opposite — that the question
// is still open and worth asking again — when in fact nothing changes until a person
// alters the deployment.
func TestRefreshNoteSeparatesASettledRefusalFromAFailedCheck(t *testing.T) {
	err := &backend.Error{
		Code: backend.CodeAuthPermissionDenied, Type: "authentication_error",
		Message: "this identity is not on the private-staging allowlist; email ops@vendor.example",
	}
	got := refreshNote(app.AccountRefresh{Err: err}, auth.Status{State: auth.StateAccessRefused})

	if strings.Contains(got, "could not be re-checked") {
		t.Errorf("a settled refusal was rendered as a transient failure:\n%s", got)
	}
	if !strings.Contains(got, "refused") {
		t.Errorf("the note does not say the account was refused:\n%s", got)
	}
	if !strings.Contains(got, backend.CodeAuthPermissionDenied) {
		t.Errorf("the note dropped the stable code, which is the searchable half:\n%s", got)
	}
}

// BACKEND-AUTHORED PROSE NEVER REACHES A CARD.
//
// `*backend.Error.Error()` writes the server's `Message` verbatim, and this note renders
// into the conversation, the host's NDJSON stream and every transcript that gets pasted
// into an issue. The message can name a vendor, quote a provider's copy, or carry
// whatever a proxy in between substituted; the code is ours and is what a reader can
// actually search for.
func TestRefreshNoteNeverEchoesBackendAuthoredProse(t *testing.T) {
	const prose = "polar says: contact billing@vendor.example about card ending 4242"
	for _, tc := range []struct {
		name string
		err  *backend.Error
	}{
		{"a settled refusal", &backend.Error{Code: backend.CodeAuthPermissionDenied, Type: "authentication_error", Message: prose}},
		{"a dependency outage", &backend.Error{Code: backend.CodeEntitlementUnavailable, Type: "api_error", Message: prose}},
		{"an unknown code", &backend.Error{Code: "something_new", Type: "api_error", Message: prose}},
		// The hole a code-preferring helper leaves open if it stops there: the decoder
		// accepts an envelope carrying a message and NO code, and a fallback to the
		// error's own text hands the whole leak straight back.
		{"an envelope with no code at all", &backend.Error{HTTPStatus: 403, Type: "authentication_error", Message: prose}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := refreshNote(app.AccountRefresh{Err: tc.err}, auth.Status{State: auth.StateSignedInUnverified})
			if strings.Contains(got, "vendor.example") || strings.Contains(got, "4242") {
				t.Errorf("the backend's own prose reached the card:\n%s", got)
			}
			// Something local and specific stands in its place — the code where there is
			// one, the HTTP status where there is not. A note that named nothing at all
			// would be its own failure.
			want := tc.err.Code
			if want == "" {
				want = "http 403"
			}
			if !strings.Contains(got, want) {
				t.Errorf("the note names nothing the reader can act on (wanted %q):\n%s", want, got)
			}
		})
	}
}

// A local failure keeps its own words. The rule above is about the SERVER's prose — an
// error raised on this machine (a locked keychain, a lock that could not be taken) carries
// the only description of what went wrong, and replacing it with a code would leave the
// reader with nothing.
func TestRefreshNoteKeepsTheTextOfALocalFailure(t *testing.T) {
	got := refreshNote(app.AccountRefresh{Err: errors.New("keychain is locked")}, auth.Status{})
	if !strings.Contains(got, "keychain is locked") {
		t.Errorf("a local failure lost the only description it had:\n%s", got)
	}
}

// The card must SAY SOMETHING to a refused account.
//
// accountNextStep had no branch for it and fell through to the empty string, so the card
// read "access refused" and then stopped — leaving a reader with a valid sign-in and no
// action reaching for the two that cannot help: another login, which returns the identical
// refusal, and a checkout, which buys a plan for an account the deployment has not
// admitted.
func TestTheAccountCardGivesARefusedAccountAnAction(t *testing.T) {
	got := accountNextStep(auth.Status{
		State: auth.StateAccessRefused,
		// Links are present, which is the trap: with them in hand the easy branch is a
		// billing URL, and this is not a billing problem.
		Links: auth.StatusLinks{Subscribe: "https://daintree.org/subscribe", Account: "https://daintree.org/account"},
	})
	if got == "" {
		t.Fatal("a refused account still gets no next step at all")
	}
	// The remedy is a person approving this account on the deployment.
	if !strings.Contains(got, "approve") {
		t.Errorf("the next step does not name operator approval:\n%s", got)
	}
	// Neither of the two answers that cannot work may lead.
	for _, banned := range []string{"Choose a plan", "subscribe", "Check billing"} {
		if strings.Contains(got, banned) {
			t.Errorf("a refusal was answered with %q:\n%s", banned, got)
		}
	}
	// A fresh sign-in is at most secondary, and must never be offered as the fix.
	if strings.HasPrefix(got, "Run /login") {
		t.Errorf("the card leads with a login that returns the same refusal:\n%s", got)
	}
}

// The two 403s land in ONE state and need TWO sentences.
//
// `access_refused` covers an account the deployment has not approved AND an OAuth client
// it does not accept, which is why the backend keeps them as separate codes under a shared
// remedy. Telling someone whose CLIENT is refused to try a different account sends them
// through a browser flow that cannot change the answer.
func TestTheRefusedAccountAdviceDistinguishesTheAccountFromTheClient(t *testing.T) {
	account := accountNextStep(auth.Status{
		State: auth.StateAccessRefused, LastErrorCode: backend.CodeAuthPermissionDenied,
	})
	client := accountNextStep(auth.Status{
		State: auth.StateAccessRefused, LastErrorCode: backend.CodeAuthClientNotAllowed,
	})

	if account == client {
		t.Fatalf("both 403s produce the identical advice, so one of them is wrong:\n%s", account)
	}
	if !strings.Contains(client, "client") {
		t.Errorf("a refused client is not told what was refused:\n%s", client)
	}
	// The remedy that cannot work for a refused client.
	if strings.Contains(client, "/logout") || strings.Contains(client, "different account") {
		t.Errorf("a refused client is offered a different account, which changes nothing:\n%s", client)
	}
}
