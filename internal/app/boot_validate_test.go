package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
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

// Skill requiredTools validation was REMOVED: skills are server-owned (the backend's
// selector picks and injects runbooks), so the CLI no longer loads a local skill
// catalog to validate at boot. The two tests that exercised that validation
// (TestBootValidatesSkillRequiredTools / TestBootCleanWithFullToolSet) were deleted.

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

// infoCapture is an agent.EventSink that records Info messages (set as the live
// AgentEvents hook so the session's eventProxy forwards to it).
type infoCapture struct {
	agent.NoopEventSink
	mu   sync.Mutex
	msgs []string
}

func (c *infoCapture) Info(m string) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *infoCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

// Issue #213, part 2 end-to-end: App.Create must thread RehydrateResult.DroppedRows
// into the session so the first resumed turn emits the "rows dropped" info note.
// This is the only test that exercises the full wiring (seed corrupt DB row →
// Create → first turn → info event); the unit tests stub the count directly.
func TestCreatePropagatesRehydrateDropsToFirstTurn(t *testing.T) {
	dir := t.TempDir()
	const sid = "ses_drop_wire"

	// Boot a fresh session (no client-side control prefix is persisted anymore), then
	// append a corrupt assistant row (malformed tool-call JSON) at seq 3 — rehydration
	// drops it regardless of the preceding seqs.
	a1, err := Create(CreateOptions{
		SessionID: sid,
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
	})
	if err != nil {
		t.Fatalf("Create (seed): %v", err)
	}
	bad := "{not json"
	if _, err := a1.Store.InsertMessage(domain.ConversationMessageRecord{
		SessionID: sid, Seq: 3, Role: "assistant", Content: "text", ToolCallsJson: &bad,
	}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	if err := a1.Shutdown(); err != nil {
		t.Fatalf("Shutdown (seed): %v", err)
	}

	// Re-create with the SAME session id: rehydration must count the corrupt row. Inject a
	// fake backend so the first turn runs without the real client (which targets the
	// hardcoded dev endpoint); the rehydrate-drop info note is emitted before the model
	// stream regardless of the reply.
	a2, err := Create(CreateOptions{
		SessionID: sid,
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
		BackendOverride: &fakeBackend{},
	})
	if err != nil {
		t.Fatalf("Create (resume): %v", err)
	}
	t.Cleanup(func() { _ = a2.Shutdown() })

	capture := &infoCapture{}
	a2.SetHooks(AppHooks{AgentEvents: capture})

	// The info note is emitted at the start of the first turn (before the model stream),
	// so the turn's reply is irrelevant here.
	if _, err := a2.Session.Send(context.Background(), "hello", agent.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	found := false
	for _, m := range capture.all() {
		if strings.Contains(m, "dropped from saved history") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a rehydrate-drop info note on the first resumed turn; got %v", capture.all())
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
