package app

import (
	"path/filepath"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
)

// TestCreateWiresPersistedLoginIntoBackendClient proves the resolved login
// credentials actually reach the real backend client: with HOME isolated, a
// persisted /login endpoint becomes the client's BaseURL (the API key rides the
// same AppConfig fields into ClientConfig — the pairing rule itself is locked
// by the config package tests).
func TestCreateWiresPersistedLoginIntoBackendClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAINTREE_BACKEND_URL", "")
	if err := config.SaveCredentials(filepath.Join(home, ".daintree", "credentials.json"),
		config.Credentials{Endpoint: "https://custom.example.com", APIKey: "sk-test-wire"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = a.Shutdown() }()
	if got := a.Backend.BaseURL(); got != "https://custom.example.com" {
		t.Fatalf("backend client BaseURL = %q, want the persisted login endpoint", got)
	}
	if a.Config.BackendAPIKey != "sk-test-wire" {
		t.Fatalf("resolved BackendAPIKey = %q, want the persisted key", a.Config.BackendAPIKey)
	}
}
