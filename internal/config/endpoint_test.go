package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// The whole point of dropping the sign-in: a fresh install resolves to a WORKING config
// with no key anywhere. Nothing is stored, nothing is prompted for, and the endpoint
// falls straight through to the deployed backend.
func TestEndpoint_FreshInstallNeedsNothing(t *testing.T) {
	isolatedHome(t)
	cfg := mustLoad(t, ConfigOverrides{})

	if cfg.BackendURL != backend.DefaultBaseURL {
		t.Errorf("backendURL = %q, want %q", cfg.BackendURL, backend.DefaultBaseURL)
	}
	if cfg.APIKey != "" {
		t.Errorf("a fresh install must carry no credential, got %q", cfg.APIKey)
	}
}

// The dev loop, and now the only endpoint mechanism there is.
func TestEndpoint_TrustedEnvOverridesTheDefault(t *testing.T) {
	isolatedHome(t)
	t.Setenv("DAINTREE_BACKEND_URL", backend.LocalBaseURL)

	cfg := mustLoad(t, ConfigOverrides{})
	if cfg.BackendURL != backend.LocalBaseURL {
		t.Errorf("backendURL = %q, want the local override", cfg.BackendURL)
	}
	if cfg.APIKey != "" {
		t.Errorf("pointing at a local backend must not conjure a key, got %q", cfg.APIKey)
	}
}

// SECURITY, and the reason both stay trusted-env. The URL decides where a turn — its
// prose, tool arguments and results — is SENT, and a key, when one exists at all, is
// spendable. A bound project's .env is arbitrary attacker-controlled content, so cloning
// a repo must not be enough to redirect a stranger's session or fund a turn on their
// behalf.
func TestEndpoint_ProjectDotEnvCanSupplyNeither(t *testing.T) {
	isolatedHome(t)
	project := t.TempDir()
	envBody := "DAINTREE_BACKEND_URL=http://evil.test\nDAINTREE_API_KEY=sk-attacker-0123456789\n"
	if err := os.WriteFile(filepath.Join(project, ".env"), []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := mustLoad(t, ConfigOverrides{ProjectPath: strptr(project)})

	if cfg.BackendURL == "http://evil.test" {
		t.Fatal("a project .env must not be able to redirect the backend endpoint")
	}
	if cfg.APIKey != "" {
		t.Fatalf("a project .env must not be able to inject an API key, got %q", cfg.APIKey)
	}
}

// The seam kept alive for account sign-in: an exported key still resolves and still
// rides as the bearer, overriding the backend's own credential for this session.
func TestEndpoint_TrustedEnvKeyStillResolves(t *testing.T) {
	isolatedHome(t)
	const key = "sk-test-0123456789"
	t.Setenv("DAINTREE_API_KEY", key)

	if got := mustLoad(t, ConfigOverrides{}).APIKey; got != key {
		t.Errorf("apiKey = %q, want the exported key to survive resolution", got)
	}
}

// Nobody is prompted for this value any more, so a mangled one arrives from the shell.
// Failing at load names the variable; letting it through fails inside net/http on every
// single turn with a message that names nothing.
func TestEndpoint_MalformedKeyFailsAtLoadNamingTheVariable(t *testing.T) {
	isolatedHome(t)
	t.Setenv("DAINTREE_API_KEY", "sk-test with a space")

	_, err := LoadConfig(ConfigOverrides{ProjectPath: strptr(t.TempDir())})
	if err == nil {
		t.Fatal("a bearer that cannot ride an HTTP header must fail at load")
	}
	if !strings.Contains(err.Error(), "DAINTREE_API_KEY") {
		t.Errorf("the error must name the variable to fix: %v", err)
	}
}

// DescribeConfig feeds /status, which users paste into bug reports.
func TestEndpoint_DescribeConfigNeverLeaksTheKey(t *testing.T) {
	isolatedHome(t)
	const key = "sk-test-verysecret0123456789"
	t.Setenv("DAINTREE_API_KEY", key)
	cfg := mustLoad(t, ConfigOverrides{})

	desc := DescribeConfig(cfg)
	if strings.Contains(desc["apiKey"], key) {
		t.Fatalf("DescribeConfig leaked the raw API key: %q", desc["apiKey"])
	}
	if desc["apiKey"] == "" || desc["apiKey"] == "(unset)" {
		t.Fatalf("a set key must still read as present: %q", desc["apiKey"])
	}
	if desc["backendUrl"] != cfg.BackendURL {
		t.Fatalf("backendUrl = %q, want %q", desc["backendUrl"], cfg.BackendURL)
	}
}

// The stored preference is the one endpoint source a user cannot fix from the command
// line, so when it is refused the reason has to be carried rather than swallowed —
// otherwise the session silently runs on the deployed default and nothing can say why.
// DescribeConfig is the redacted view of a resolved config (`/backend` is what actually
// renders this rejection to a human, in app.DescribeBackendChoices), and it is checked
// here as the boundary it is: everything it emits is quotable into a bug report, so a
// refused endpoint's userinfo or query token must not survive the trip into it.
func TestEndpoint_DescribeConfigReportsARefusedPreferenceWithoutLeakingIt(t *testing.T) {
	isolatedHome(t)
	stateDir := t.TempDir()
	if err := SaveBackendURL(EndpointPath(stateDir), "https://user:verysecret0123@stored.example"); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr(stateDir)})

	desc := DescribeConfig(cfg)
	if desc["endpointShapeRejected"] == "" {
		t.Fatal("a refused stored preference must be reported, or the session silently runs on an endpoint nobody chose")
	}
	for key, val := range desc {
		if strings.Contains(val, "verysecret0123") {
			t.Fatalf("DescribeConfig[%q] leaked the refused endpoint's credential: %q", key, val)
		}
	}
	if desc["backendUrl"] != backend.DefaultBaseURL {
		t.Errorf("backendUrl = %q, want the default fallback", desc["backendUrl"])
	}
}
