package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
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
