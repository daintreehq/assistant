package app

import (
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Finding 5: App.Create must validate every loaded skill's requiredTools against
// the live registry at boot, so a skill declaring a tool the registry lacks is
// surfaced LOUDLY (debug log + the Log hook) instead of silently vanishing from
// OpenAITools when that skill is loaded. Here we wire an EMPTY tool registry, so
// the real embedded skills' requiredTools are all missing — the Log hook must
// receive the warning.
func TestBootValidatesSkillRequiredTools(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	dir := t.TempDir()

	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
		Hooks: AppHooks{
			Log: func(msg string) {
				mu.Lock()
				logs = append(logs, msg)
				mu.Unlock()
			},
		},
		// Register no tools — every embedded skill's requiredTools is now missing.
		BuildTools: func(_ *App) ([]*tools.Tool, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, m := range logs {
		if strings.Contains(m, "requiredTools missing") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a 'requiredTools missing' boot diagnostic; got logs: %v", logs)
	}
}

// The happy path (the real full tool set) must NOT emit the diagnostic — every
// embedded skill's requiredTools is satisfied by the registered families.
func TestBootCleanWithFullToolSet(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	dir := t.TempDir()

	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
		Hooks: AppHooks{
			Log: func(msg string) {
				mu.Lock()
				logs = append(logs, msg)
				mu.Unlock()
			},
		},
		// nil BuildTools ⇒ DefaultToolBuilder (the full wired set).
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	mu.Lock()
	defer mu.Unlock()
	for _, m := range logs {
		if strings.Contains(m, "requiredTools missing") {
			t.Fatalf("full tool set should satisfy every skill's requiredTools; unexpected: %q", m)
		}
	}
}
