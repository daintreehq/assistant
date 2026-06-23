package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// coreToolStubs builds a no-op read-risk tool for each core tool name so a test
// registry satisfies the boot-time core-tool drift assertion
// (Registry.AssertRegistered) without pulling in the full wired tool set. The
// stubs are never dispatched at boot, so a trivial Ok handler suffices.
func coreToolStubs() []*tools.Tool {
	names := agent.CoreToolNames()
	stubs := make([]*tools.Tool, 0, len(names))
	for _, name := range names {
		stubs = append(stubs, &tools.Tool{
			Name: name,
			Risk: domain.RiskRead,
			Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
				return tools.Ok("ok", nil)
			},
		})
	}
	return stubs
}

// Finding 5: App.Create must validate every loaded skill's requiredTools against
// the live registry at boot, so a skill declaring a tool the registry lacks is
// surfaced LOUDLY (debug log + the Log hook) instead of silently vanishing from
// OpenAITools when that skill is loaded. Here we wire ONLY the core tools (so the
// boot-time core-tool drift assertion passes), leaving the embedded skills'
// non-core requiredTools all missing — the Log hook must receive the warning.
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
		// Register only the core tools — the assertion passes, but every embedded
		// skill's NON-core requiredTools is still missing.
		BuildTools: func(_ *App) ([]*tools.Tool, error) { return coreToolStubs(), nil },
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

// Issue #213: agent.coreToolNames is hand-maintained and must stay in lockstep
// with the registry. App.Create must HARD-FAIL boot if a core tool name is not
// registered (a rename/removal would otherwise starve the model silently).
func TestBootFailsWhenCoreToolMissing(t *testing.T) {
	dir := t.TempDir()

	// Register every core tool EXCEPT one — simulating a drift where a core name no
	// longer matches the registry.
	stubs := coreToolStubs()
	dropped := stubs[len(stubs)-1].Name
	stubs = stubs[:len(stubs)-1]

	_, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
		BuildTools: func(_ *App) ([]*tools.Tool, error) { return stubs, nil },
	})
	if err == nil {
		t.Fatal("Create must hard-fail when a core tool name is missing from the registry")
	}
	if !strings.Contains(err.Error(), "core tools") || !strings.Contains(err.Error(), dropped) {
		t.Fatalf("error should name the core-tool drift and the missing tool %q; got %q", dropped, err.Error())
	}
}

// The full wired tool set must satisfy the core-tool assertion — boot succeeds.
func TestBootCoreToolsAllRegisteredWithFullSet(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
		// nil BuildTools ⇒ the full wired set, which must include every core tool.
	})
	if err != nil {
		t.Fatalf("full tool set must satisfy the core-tool assertion: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
}
