package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/config"
)

// accountlayerfault_test.go pins the distinction the account layer used to lose: a
// session with no account manager because none was WANTED, versus one with no account
// manager because it could not be BUILT.
//
// Both used to arrive as a bare nil, so every surface downstream described a broken state
// root — an unwritable directory, a plain file where `auth` belongs, EACCES, ENOSPC — as
// "this deployment does not use accounts". The user then went looking for a backend
// problem while the fault sat on their own disk.

// brokenStateRoot returns a state root whose auth directory can never be created, by
// putting a regular FILE exactly where MkdirAll needs a directory.
//
// Chosen over chmod-ing the root read-only because that proves nothing when the test runs
// as root — which it does in plenty of Linux containers, where a 0500 directory is still
// writable and the "broken" case silently becomes the healthy one. ENOTDIR binds for
// every user there is.
func brokenStateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the blocking file: %v", err)
	}
	return root
}

// A state root the auth directory cannot be created under yields no manager AND a fault
// naming the cause.
func TestAccountLayerFaultReportsAnUnbuildableStateRoot(t *testing.T) {
	cfg := config.AppConfig{StateRoot: brokenStateRoot(t), BackendURL: "https://assistant.daintree.org"}

	if mgr := NewAccountManager(cfg); mgr != nil {
		t.Fatal("a state root with a file where the auth directory belongs produced a manager")
	}
	fault := accountLayerFault(cfg)
	if fault == nil {
		t.Fatal("no fault reported for an unbuildable auth state root — this is the whole defect")
	}
	if errors.Is(fault, ErrAccountLayerUnbuilt) {
		t.Fatalf("fell back to the sentinel instead of naming the cause: %v", fault)
	}
	// The code is what a caller branches on; the message is what a human reads. Both have
	// to survive, or the surfaces above cannot tell this apart from a sign-in failure.
	if code := auth.CodeOf(fault); code == "" {
		t.Errorf("fault carries no auth code, so no caller can classify it: %v", fault)
	}
}

// A caller key is a CHOICE, not a fault, and must stay silent. Reporting one would tell an
// operator who configured exactly this that their machine is broken.
func TestAccountLayerFaultStaysSilentForACallerKey(t *testing.T) {
	// Deliberately combined with a root that WOULD fault: the caller-key branch has to
	// win, because no auth directory is needed when no managed sign-in is in play.
	cfg := config.AppConfig{
		StateRoot:  brokenStateRoot(t),
		BackendURL: "https://assistant.daintree.org",
		APIKey:     "fake-caller-key-for-tests",
	}
	if mgr := NewAccountManager(cfg); mgr != nil {
		t.Fatal("a caller key must leave App.Auth nil — two credentials, one winner")
	}
	if fault := accountLayerFault(cfg); fault != nil {
		t.Fatalf("a deliberate caller key was reported as a fault: %v", fault)
	}
}

// A healthy root builds — and the config-only helper, which is only ever consulted once
// a manager is already known to be missing, falls back to the sentinel rather than to the
// directory error. Anything else would attribute a cause it has no evidence for.
func TestAccountLayerFaultOnAHealthyRootReportsNoDirectoryCause(t *testing.T) {
	cfg := config.AppConfig{StateRoot: t.TempDir(), BackendURL: "https://assistant.daintree.org"}
	if mgr := NewAccountManager(cfg); mgr == nil {
		t.Fatal("a writable state root failed to produce a manager")
	}
	fault := accountLayerFault(cfg)
	if !errors.Is(fault, ErrAccountLayerUnbuilt) {
		t.Fatalf("healthy root reported %v, want the sentinel", fault)
	}
}

// The App accessor answers from the LIVE pair, so a session that has a manager reports
// nothing even when the config alone would fault — the manager already exists and the
// directory it needs was created when it was built.
func TestAppAccountLayerFaultIsSilentWhileAManagerExists(t *testing.T) {
	cfg := config.AppConfig{StateRoot: t.TempDir(), BackendURL: "https://assistant.daintree.org"}
	mgr := NewAccountManager(cfg)
	if mgr == nil {
		t.Fatal("a writable state root failed to produce a manager")
	}
	a := &App{Config: cfg, Auth: mgr}
	if fault := a.AccountLayerFault(); fault != nil {
		t.Fatalf("a session holding a manager reported a fault: %v", fault)
	}
}

// The sentinel exists so the third branch is never silent: no manager, no caller key, and
// a root that now builds fine. It is the repaired-mid-session case, and the one thing it
// must not do is fall through to copy about the deployment.
func TestAppAccountLayerFaultFallsBackToTheSentinel(t *testing.T) {
	a := &App{Config: config.AppConfig{StateRoot: t.TempDir(), BackendURL: "https://assistant.daintree.org"}}
	fault := a.AccountLayerFault()
	if !errors.Is(fault, ErrAccountLayerUnbuilt) {
		t.Fatalf("missing manager with a healthy root reported %v, want the sentinel", fault)
	}
}

// fetchAccount is where the refresh path used to erase the distinction: mgr == nil meant
// Skipped unconditionally, so a broken state root rendered as "nothing to check here".
func TestFetchAccountReportsAConstructionFaultAsAnError(t *testing.T) {
	cfg := config.AppConfig{StateRoot: brokenStateRoot(t), BackendURL: "https://assistant.daintree.org"}
	got := fetchAccount(context.Background(), cfg, nil, AccountRefreshOptions{})
	if got.Skipped {
		t.Fatal("a broken auth state root was skipped — it reads as a deployment with no accounts")
	}
	if got.Err == nil {
		t.Fatal("a broken auth state root produced no error")
	}
}

// The caller-bearer case keeps its old behaviour, tested separately from the broken root
// on purpose: it is the one nil manager that genuinely has nothing to report, and folding
// the two together is what produced the defect in the first place.
func TestFetchAccountStillSkipsForACallerKey(t *testing.T) {
	cfg := config.AppConfig{
		StateRoot:  t.TempDir(),
		BackendURL: "https://assistant.daintree.org",
		APIKey:     "fake-caller-key-for-tests",
	}
	got := fetchAccount(context.Background(), cfg, nil, AccountRefreshOptions{})
	if !got.Skipped {
		t.Fatal("a caller key must skip the account read — there is no session to refresh")
	}
	if got.Err != nil {
		t.Fatalf("a caller key produced an error: %v", got.Err)
	}
}

// RefreshAccount folds that error through to the value every surface renders, as Err
// rather than Skipped — refreshNote is silent on Skipped by design, so the wrong one here
// means the fault reaches the user as nothing at all.
func TestRefreshAccountSurfacesAConstructionFault(t *testing.T) {
	a := &App{Config: config.AppConfig{StateRoot: brokenStateRoot(t), BackendURL: "https://assistant.daintree.org"}}
	res := a.RefreshAccount(context.Background(), AccountRefreshOptions{})
	if res.Skipped {
		t.Fatal("reported as skipped, which renders as no note at all")
	}
	if res.Err == nil {
		t.Fatal("no error reported for a session whose account manager could not be built")
	}
	if res.Applied() {
		t.Fatal("reported as applied — nothing was read")
	}
}
