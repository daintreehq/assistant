package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/credentials"
)

// writeSignIn stores a sign-in at the path the given config would resolve to.
func writeSignIn(t *testing.T, dir, baseURL, key string) {
	t.Helper()
	if err := credentials.Save(credentials.Path(dir), credentials.Credentials{BaseURL: baseURL, APIKey: key}); err != nil {
		t.Fatal(err)
	}
}

// With nothing stored and nothing exported, a fresh install points at the deployed
// backend and reports itself signed out — the state the login gate keys on.
func TestSignIn_FreshInstallDefaultsToTheDeployedEndpoint(t *testing.T) {
	isolatedHome(t)
	cfg := mustLoad(t, ConfigOverrides{})

	if cfg.BackendURL != backend.DefaultBaseURL {
		t.Errorf("backendURL = %q, want %q", cfg.BackendURL, backend.DefaultBaseURL)
	}
	if cfg.APIKey != "" {
		t.Errorf("a fresh install must be signed out, got key %q", cfg.APIKey)
	}
}

// The sign-in is PER-USER: stored at the state root so one login serves every project,
// rather than in the per-project subdir where each project would need its own.
func TestSignIn_CredentialsLiveAtTheStateRootAcrossProjects(t *testing.T) {
	home := isolatedHome(t)
	root := filepath.Join(home, ".daintree", "assistant-cli")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSignIn(t, root, "https://assistant.daintree.org", "sk-or-v1-shared0123456789")

	t.Setenv("DAINTREE_PROJECT_ID", "proj-alpha")
	alpha := mustLoad(t, ConfigOverrides{})
	if alpha.APIKey != "sk-or-v1-shared0123456789" {
		t.Fatalf("project alpha did not see the shared sign-in: %q", alpha.APIKey)
	}

	t.Setenv("DAINTREE_PROJECT_ID", "proj-beta")
	beta := mustLoad(t, ConfigOverrides{})
	if beta.APIKey != alpha.APIKey {
		t.Fatalf("project beta key = %q, want the same shared sign-in", beta.APIKey)
	}
	// …while the per-project state dirs stay distinct, so this is genuinely a shared
	// credential rather than two projects colliding on one state dir.
	if alpha.StateDir == beta.StateDir {
		t.Fatal("per-project state dirs must remain distinct")
	}
	if alpha.CredentialsPath != beta.CredentialsPath {
		t.Fatalf("credentials path must be shared: %q vs %q", alpha.CredentialsPath, beta.CredentialsPath)
	}
}

// An explicit state dir is how tests, benchmarks and db-reset isolate themselves; the
// credentials must follow it, or those runs would read (and a login would overwrite)
// the developer's real sign-in.
func TestSignIn_ExplicitStateDirIsolatesTheCredentials(t *testing.T) {
	home := isolatedHome(t)
	root := filepath.Join(home, ".daintree", "assistant-cli")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSignIn(t, root, "https://assistant.daintree.org", "sk-or-v1-real0123456789")

	isolated := t.TempDir()
	cfg := mustLoad(t, ConfigOverrides{StateDir: strptr(isolated)})

	if cfg.CredentialsPath != credentials.Path(isolated) {
		t.Fatalf("credentialsPath = %q, want it inside the overridden state dir", cfg.CredentialsPath)
	}
	if cfg.APIKey != "" {
		t.Fatalf("an isolated state dir must not see the real sign-in, got %q", cfg.APIKey)
	}
}

// The main development loop: sign in once against the deployed backend, then point
// DAINTREE_BACKEND_URL at a local one to test a backend change. The URL is overridden;
// the KEY must survive, because it is the caller's own provider credential and is
// equally valid against either endpoint.
func TestSignIn_BackendURLOverrideKeepsTheStoredKey(t *testing.T) {
	home := isolatedHome(t)
	root := filepath.Join(home, ".daintree", "assistant-cli")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSignIn(t, root, backend.DefaultBaseURL, "sk-or-v1-stored0123456789")

	t.Setenv("DAINTREE_BACKEND_URL", backend.LocalBaseURL)
	cfg := mustLoad(t, ConfigOverrides{})

	if cfg.BackendURL != backend.LocalBaseURL {
		t.Errorf("backendURL = %q, want the env override %q", cfg.BackendURL, backend.LocalBaseURL)
	}
	if cfg.APIKey != "sk-or-v1-stored0123456789" {
		t.Errorf("stored key must survive an endpoint override, got %q", cfg.APIKey)
	}
}

func TestSignIn_EnvKeyBeatsTheStoredOne(t *testing.T) {
	home := isolatedHome(t)
	root := filepath.Join(home, ".daintree", "assistant-cli")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSignIn(t, root, backend.DefaultBaseURL, "sk-or-v1-stored0123456789")

	t.Setenv("DAINTREE_API_KEY", "sk-or-v1-env0123456789")
	cfg := mustLoad(t, ConfigOverrides{})
	if cfg.APIKey != "sk-or-v1-env0123456789" {
		t.Errorf("apiKey = %q, want the env value", cfg.APIKey)
	}
}

// SECURITY: the API key is spendable (it funds the upstream model calls) and the URL
// decides where it is SENT. A bound project's .env is arbitrary attacker-controlled
// content, so it must be able to supply neither — otherwise cloning a repo would be
// enough to redirect a stranger's key to a collection endpoint.
func TestSignIn_ProjectDotEnvCannotSupplyEndpointOrKey(t *testing.T) {
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

// DescribeConfig feeds /status, which users paste into bug reports.
func TestSignIn_DescribeConfigRedactsTheKey(t *testing.T) {
	isolatedHome(t)
	const key = "sk-or-v1-verysecret0123456789"
	t.Setenv("DAINTREE_API_KEY", key)
	cfg := mustLoad(t, ConfigOverrides{})

	desc := DescribeConfig(cfg)
	if desc["apiKey"] == key {
		t.Fatal("DescribeConfig leaked the raw API key")
	}
	if desc["apiKey"] != credentials.Redact(key) {
		t.Fatalf("apiKey = %q, want the redacted form", desc["apiKey"])
	}
	if desc["backendUrl"] != cfg.BackendURL {
		t.Fatalf("backendUrl = %q, want %q", desc["backendUrl"], cfg.BackendURL)
	}
}

// A malformed credentials file must NOT be fatal. Both commands that exist to repair it
// — login and logout — resolve config first, so erroring here would refuse every
// supported recovery path and leave "delete the file yourself" as the only way out.
func TestSignIn_MalformedCredentialsResolveAsSignedOut(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(credentials.Path(dir), []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(ConfigOverrides{StateDir: strptr(dir)})
	if err != nil {
		t.Fatalf("a malformed credentials file must not be fatal: %v", err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("a malformed file must read as signed out, got key %q", cfg.APIKey)
	}
	// …and the login flow must still be able to overwrite it.
	if err := credentials.Save(cfg.CredentialsPath, credentials.Credentials{
		BaseURL: backend.DefaultBaseURL, APIKey: "sk-or-v1-repaired0123456789",
	}); err != nil {
		t.Fatalf("login must be able to repair a malformed file: %v", err)
	}
}

// SECURITY: with no home directory and no explicit override, stateRoot would resolve
// RELATIVE to the working directory — i.e. inside the bound project — writing a
// spendable API key into the user's repository where 0600 will not save it from a
// stray `git add`. Fail loudly instead.
func TestSignIn_NoHomeDirectoryIsFatalWithoutAnExplicitStateDir(t *testing.T) {
	isolatedHome(t)
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	if _, err := LoadConfig(ConfigOverrides{}); err == nil {
		t.Fatal("an unresolvable home directory must fail rather than store the key in the project")
	}

	// An explicit state dir is a deliberate choice and stays supported.
	dir := t.TempDir()
	if _, err := LoadConfig(ConfigOverrides{StateDir: strptr(dir)}); err != nil {
		t.Fatalf("an explicit state dir must still work without a home: %v", err)
	}
}
