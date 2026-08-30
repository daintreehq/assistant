package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/tools/agenttaskx"
)

// prepareScheduledCall against the REAL registry.
//
// The timer package's own tests inject a stub for this seam, so they prove the seam is
// consulted and cannot prove it agrees with the registry. This one does: the failure it
// exists to catch is a name that schedule-time can resolve and fire-time cannot, which
// is invisible to any test that does not use the same lookup dispatch uses.
func scheduleTestApp(t *testing.T) *App {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range agenttaskx.Tools(agenttaskx.Deps{}) {
		t := tl
		if err := reg.Register(&t); err != nil {
			return nil
		}
	}
	// Build the projection before resolving anything. The wire-name alias map is
	// rebuilt by OpenAITools and is EMPTY until then — and reversing a wire name by
	// string substitution is explicitly forbidden (collisions are possible), so the map
	// is the only legal resolver. In production this is free: a turn cannot schedule a
	// timer without the model having been handed the projected toolset first.
	if _, err := reg.OpenAITools(nil); err != nil {
		t.Fatalf("project tools: %v", err)
	}
	return &App{Registry: reg}
}

func TestPrepareScheduledCallResolvesNamesDispatchCanLookUp(t *testing.T) {
	a := scheduleTestApp(t)
	good := json.RawMessage(`{"title":"T","taskPrompt":"p","worktreeId":"/w"}`)

	for name, written := range map[string]string{
		"internal spelling":  "agentTask.spawnForEdits",
		"wire spelling":      "agentTask__spawnForEdits",
		"padded with spaces": "  agentTask.spawnForEdits  ",
	} {
		canonical, refusal := a.prepareScheduledCall(written, good)
		if refusal != "" {
			t.Errorf("%s should schedule, got refusal %q", name, refusal)
			continue
		}
		// The whole point: what comes back must be a name the registry can find,
		// because that is the lookup fire-time dispatch performs.
		if a.Registry.Get(canonical) == nil {
			t.Errorf("%s resolved to %q, which dispatch could not look up", name, canonical)
		}
	}
}

func TestPrepareScheduledCallRefusesWhatCouldNeverRun(t *testing.T) {
	a := scheduleTestApp(t)
	for name, tc := range map[string]struct{ tool, args, wantIn string }{
		// Used to schedule cleanly: a tool nobody could find raised no objection.
		"unknown tool":       {"agentTask.spawnForEditz", `{"title":"T","taskPrompt":"p","worktreeId":"/w"}`, "registered"},
		"no worktree":        {"agentTask.spawnForEdits", `{"title":"T","taskPrompt":"p"}`, "worktreeId"},
		"missing taskPrompt": {"agentTask.spawnForEdits", `{"title":"T","worktreeId":"/w"}`, "not valid"},
		"bad mode enum":      {"agentTask.spawnForEdits", `{"title":"T","taskPrompt":"p","worktreeId":"/w","mode":"refactor"}`, "not valid"},
	} {
		canonical, refusal := a.prepareScheduledCall(tc.tool, json.RawMessage(tc.args))
		if refusal == "" {
			t.Errorf("%s should be refused at schedule time", name)
			continue
		}
		if !strings.Contains(refusal, tc.wantIn) {
			t.Errorf("%s: refusal should say why (%q), got %q", name, tc.wantIn, refusal)
		}
		if canonical != "" {
			t.Errorf("%s: a refusal must not hand back a name to store, got %q", name, canonical)
		}
	}
}

// No registry (a stripped context) means no opinion — never a refusal. Blocking every
// schedule because the checker is absent would be a worse failure than the one it
// guards against.
func TestPrepareScheduledCallIsSilentWithoutARegistry(t *testing.T) {
	a := &App{}
	canonical, refusal := a.prepareScheduledCall("anything.at.all", json.RawMessage(`{}`))
	if refusal != "" || canonical != "anything.at.all" {
		t.Fatalf("expected a pass-through, got (%q, %q)", canonical, refusal)
	}
}
