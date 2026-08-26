package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/config"
)

// doctoraccount_test.go pins the account row against the one condition it used to report
// as healthy.
//
// The row reads configuration only, on purpose — doctor is run when things are broken and
// a row needing the network goes unknown exactly when it matters. That is also how it
// missed this: it never asked whether the account layer had actually been built, so a
// state root whose auth directory could not be created printed "ok  account · account
// sign-in", and a support bundle from a machine that cannot sign in at all showed no
// problem.

// brokenAuthStateRoot blocks the auth directory with a regular file, which defeats
// MkdirAll for every user — including root, where a read-only directory would not.
func brokenAuthStateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the blocking file: %v", err)
	}
	return root
}

func TestAccountDoctorCheckFailsOnAnUnbuildableAuthStateRoot(t *testing.T) {
	root := brokenAuthStateRoot(t)
	c := accountDoctorCheck(&app.App{Config: config.AppConfig{
		StateRoot:  root,
		BackendURL: "https://assistant.daintree.org",
	}})

	if c.Status != StatusFail {
		t.Fatalf("status %q for a state root the account layer cannot be built under, want fail", c.Status)
	}
	if c.Hint == "" {
		t.Error("a failing check with no hint is a support ticket by construction")
	}
	// The PATH belongs in a doctor row: the fault is unactionable without knowing which
	// directory could not be created, and this output already carries state-dir paths.
	if !strings.Contains(c.Hint, root) {
		t.Errorf("hint does not name the state root, so there is nothing to fix: %q", c.Hint)
	}
	if c.Data["stateRoot"] != root {
		t.Errorf("data carries stateRoot %v, want %q", c.Data["stateRoot"], root)
	}
	if code, _ := c.Data["code"].(string); code == "" {
		t.Error("no stable auth code in the JSON form, so a script cannot branch on it")
	}
}

// A healthy root keeps the ok row. The failure this guards against is a check that fires
// on every install — the account layer builds fine on a normal machine, and a doctor that
// cries fail for all of them stops being read.
func TestAccountDoctorCheckStaysOKOnAHealthyStateRoot(t *testing.T) {
	root := t.TempDir()
	a := &app.App{Config: config.AppConfig{StateRoot: root, BackendURL: "https://assistant.daintree.org"}}
	a.Auth = app.NewAccountManager(a.Config)
	if a.Auth == nil {
		t.Fatal("a writable state root failed to produce an account manager")
	}

	c := accountDoctorCheck(a)
	if c.Status != StatusOK {
		t.Fatalf("status %q on a healthy install, want ok (detail: %s)", c.Status, c.Detail)
	}
}

// The deprecated caller key keeps its warn row and wins outright — tested apart from the
// broken root, because a fault reported for a deliberate override sends an operator to
// fix something they chose.
func TestAccountDoctorCheckKeepsTheCallerKeyWarning(t *testing.T) {
	c := accountDoctorCheck(&app.App{Config: config.AppConfig{
		StateRoot:        brokenAuthStateRoot(t),
		BackendURL:       "https://assistant.daintree.org",
		APIKey:           "fake-caller-key-for-tests",
		APIKeyDeprecated: true,
	}})

	if c.Status != StatusWarn {
		t.Fatalf("status %q for a caller-supplied key, want warn", c.Status)
	}
	if c.Data["deprecatedApiKey"] != true {
		t.Errorf("the deprecation flag went missing from the JSON form: %v", c.Data)
	}
}
