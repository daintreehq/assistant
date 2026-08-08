package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

// credentials_test.go locks the login-credential contract: the global file
// round-trips with owner-only permissions, malformed content is distinguishable
// from "never logged in", endpoint validation rejects secret-smuggling URL
// shapes, and LoadConfig's backend resolution keeps DAINTREE_BACKEND_URL as the
// escape hatch that also strips the persisted key (the pairing rule).

func TestSaveLoadCredentialsRoundTripAndPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds-home", ".daintree")
	path := filepath.Join(dir, "credentials.json")
	want := Credentials{Endpoint: "https://backend.example.com", APIKey: "sk-or-v1-abc123"}
	if err := SaveCredentials(path, want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	got, ok, err := LoadCredentials(path)
	if err != nil || !ok {
		t.Fatalf("LoadCredentials = (%+v, %v, %v), want complete round-trip", got, ok, err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 0600 (the key is a secret)", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("credential dir mode = %o, want 0700", perm)
	}
	// Overwrite must replace atomically and leave no temp litter behind.
	next := Credentials{Endpoint: "https://other.example.com", APIKey: "k2"}
	if err := SaveCredentials(path, next); err != nil {
		t.Fatalf("second SaveCredentials: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("credential dir has %d entries after overwrite, want just the file", len(entries))
	}
	got, ok, err = LoadCredentials(path)
	if err != nil || !ok || got != next {
		t.Fatalf("post-overwrite load = (%+v, %v, %v), want %+v", got, ok, err, next)
	}
}

func TestLoadCredentialsMissingIsNotLoggedIn(t *testing.T) {
	got, ok, err := LoadCredentials(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing file is the normal not-logged-in state, got error %v", err)
	}
	if ok || got != (Credentials{}) {
		t.Fatalf("missing file must read as zero/false, got (%+v, %v)", got, ok)
	}
}

func TestLoadCredentialsMalformedIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCredentials(path); err == nil {
		t.Fatal("malformed credentials must error (repairable via login), not read as absent")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "https://assistant.daintree.org", want: "https://assistant.daintree.org"},
		{in: "  https://x.example.com/  ", want: "https://x.example.com"},
		{in: "http://127.0.0.1:8473", want: "http://127.0.0.1:8473"},
		{in: "https://x.example.com/mount/path/", want: "https://x.example.com/mount/path"},
		{in: "", wantErr: true},
		{in: "not a url", wantErr: true},
		{in: "ftp://x.example.com", wantErr: true},
		{in: "https://", wantErr: true},
		{in: "https://user:pw@x.example.com", wantErr: true},
		{in: "https://x.example.com?k=v", wantErr: true},
		{in: "https://x.example.com#frag", wantErr: true},
	}
	for _, c := range cases {
		got, err := NormalizeEndpoint(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeEndpoint(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("NormalizeEndpoint(%q) = (%q, %v), want %q", c.in, got, err, c.want)
		}
	}
}

// loadIsolated resolves config with HOME pointed at an isolated dir (so
// DefaultCredentialsPath reads only what the test wrote) and both backend env
// knobs controlled by the caller.
func loadIsolated(t *testing.T, home string, overrides ConfigOverrides) AppConfig {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	proj := t.TempDir()
	if overrides.ProjectPath == nil {
		overrides.ProjectPath = &proj
	}
	cfg, err := LoadConfig(overrides)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestLoadConfigBackendDefaultsWithoutLogin(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", "")
	cfg := loadIsolated(t, t.TempDir(), ConfigOverrides{})
	if cfg.BackendURL != backend.DefaultBaseURL {
		t.Fatalf("BackendURL = %q, want production default %q", cfg.BackendURL, backend.DefaultBaseURL)
	}
	if cfg.BackendAPIKey != "" {
		t.Fatalf("no login → no key, got %q", cfg.BackendAPIKey)
	}
}

func TestLoadConfigBackendUsesPersistedLogin(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", "")
	home := t.TempDir()
	if err := SaveCredentials(filepath.Join(home, ".daintree", "credentials.json"),
		Credentials{Endpoint: "https://custom.example.com", APIKey: "sk-test-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := loadIsolated(t, home, ConfigOverrides{})
	if cfg.BackendURL != "https://custom.example.com" {
		t.Fatalf("BackendURL = %q, want the persisted endpoint", cfg.BackendURL)
	}
	if cfg.BackendAPIKey != "sk-test-1" {
		t.Fatalf("BackendAPIKey = %q, want the persisted key", cfg.BackendAPIKey)
	}
}

// The pairing rule: an env-overridden endpoint (dev/test/e2e fake server) must
// NEVER receive the persisted key — unless it IS the persisted endpoint.
func TestLoadConfigEnvOverrideStripsMismatchedKey(t *testing.T) {
	home := t.TempDir()
	if err := SaveCredentials(filepath.Join(home, ".daintree", "credentials.json"),
		Credentials{Endpoint: "https://custom.example.com", APIKey: "sk-test-1"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DAINTREE_BACKEND_URL", "http://127.0.0.1:9999")
	cfg := loadIsolated(t, home, ConfigOverrides{})
	if cfg.BackendURL != "http://127.0.0.1:9999" {
		t.Fatalf("BackendURL = %q, env override must win", cfg.BackendURL)
	}
	if cfg.BackendAPIKey != "" {
		t.Fatalf("mismatched env endpoint must strip the key, got %q", cfg.BackendAPIKey)
	}

	// Same endpoint via env (trailing slash included) → the key still travels.
	t.Setenv("DAINTREE_BACKEND_URL", "https://custom.example.com/")
	cfg = loadIsolated(t, home, ConfigOverrides{})
	if cfg.BackendAPIKey != "sk-test-1" {
		t.Fatalf("matching env endpoint must keep the key, got %q", cfg.BackendAPIKey)
	}
}

func TestLoadConfigBackendOverridePointerWins(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", "http://127.0.0.1:9999")
	u := "http://127.0.0.1:8888"
	cfg := loadIsolated(t, t.TempDir(), ConfigOverrides{BackendURL: &u})
	if cfg.BackendURL != u {
		t.Fatalf("BackendURL = %q, explicit override must beat env", cfg.BackendURL)
	}
}

func TestDescribeConfigNeverShowsBackendKey(t *testing.T) {
	cfg := AppConfig{BackendURL: "https://custom.example.com", BackendAPIKey: "sk-super-secret-key"}
	desc := DescribeConfig(cfg)
	if desc["backendUrl"] != "https://custom.example.com" {
		t.Fatalf("backendUrl = %q", desc["backendUrl"])
	}
	if desc["backendApiKey"] != "configured" {
		t.Fatalf("backendApiKey = %q, want presence-only %q", desc["backendApiKey"], "configured")
	}
	for k, v := range desc {
		if strings.Contains(v, "sk-super-secret-key") || strings.Contains(v, "secret") {
			t.Fatalf("DescribeConfig leaks the key via %q = %q", k, v)
		}
	}
	if DescribeConfig(AppConfig{})["backendApiKey"] != "(unset)" {
		t.Fatal("an absent key must read (unset)")
	}
}
