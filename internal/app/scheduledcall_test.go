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
func scheduleTestApp(t *testing.T) *App { return scheduleTestAppWithPin(t, nil) }

// scheduleTestAppWithPin builds the same registry with a worktree binding in place, so
// the repair path can be exercised against the REAL tool rather than a stub of it.
func scheduleTestAppWithPin(t *testing.T, pin agenttaskx.WorktreePin) *App {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range agenttaskx.Tools(agenttaskx.Deps{WorktreePin: pin}) {
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
		prepared := a.prepareScheduledCall(written, good)
		if prepared.Refusal != "" {
			t.Errorf("%s should schedule, got refusal %q", name, prepared.Refusal)
			continue
		}
		// The whole point: what comes back must be a name the registry can find,
		// because that is the lookup fire-time dispatch performs.
		if a.Registry.Get(prepared.ToolName) == nil {
			t.Errorf("%s resolved to %q, which dispatch could not look up", name, prepared.ToolName)
		}
	}
}

func TestPrepareScheduledCallRefusesWhatCouldNeverRun(t *testing.T) {
	a := scheduleTestApp(t)
	for name, tc := range map[string]struct{ tool, args, wantIn string }{
		// Used to schedule cleanly: a tool nobody could find raised no objection.
		"unknown tool": {"agentTask.spawnForEditz", `{"title":"T","taskPrompt":"p","worktreeId":"/w"}`, "registered"},
		// Only unrescuable because this app has no worktree bound; with a pin it is
		// repaired instead (see TestPrepareScheduledCallFillsInTheTurnsWorktree).
		"no worktree":        {"agentTask.spawnForEdits", `{"title":"T","taskPrompt":"p"}`, "worktreeId"},
		"missing taskPrompt": {"agentTask.spawnForEdits", `{"title":"T","worktreeId":"/w"}`, "not valid"},
		"bad mode enum":      {"agentTask.spawnForEdits", `{"title":"T","taskPrompt":"p","worktreeId":"/w","mode":"refactor"}`, "not valid"},
	} {
		prepared := a.prepareScheduledCall(tc.tool, json.RawMessage(tc.args))
		if prepared.Refusal == "" {
			t.Errorf("%s should be refused at schedule time", name)
			continue
		}
		if !strings.Contains(prepared.Refusal, tc.wantIn) {
			t.Errorf("%s: refusal should say why (%q), got %q", name, tc.wantIn, prepared.Refusal)
		}
		if prepared.ToolName != "" || prepared.Args != nil {
			t.Errorf("%s: a refusal must not hand back anything to store, got %+v", name, prepared)
		}
	}
}

// No registry (a stripped context) means no opinion — never a refusal. Blocking every
// schedule because the checker is absent would be a worse failure than the one it
// guards against.
func TestPrepareScheduledCallIsSilentWithoutARegistry(t *testing.T) {
	a := &App{}
	prepared := a.prepareScheduledCall("anything.at.all", json.RawMessage(`{}`))
	if prepared.Refusal != "" || prepared.ToolName != "anything.at.all" {
		t.Fatalf("expected a pass-through, got %+v", prepared)
	}
}

// The repair, end to end against the real registry and the real tool.
//
// A spawn that omits worktreeId is the single most common scheduled call, and the
// omission is the DOCUMENTED way to say "here". Refusing it spent a round trip telling
// the model to go and read a value this process was already holding; the pin is read
// on the spot instead and written into the args that get stored.
func TestPrepareScheduledCallFillsInTheTurnsWorktree(t *testing.T) {
	a := scheduleTestAppWithPin(t, fakePin("/repo/pinned"))

	prepared := a.prepareScheduledCall("agentTask.spawnForEdits",
		json.RawMessage(`{"title":"T","taskPrompt":"p"}`))
	if prepared.Refusal != "" {
		t.Fatalf("a bound pin makes this schedulable, got %q", prepared.Refusal)
	}
	if !strings.Contains(string(prepared.Args), `"worktreeId":"/repo/pinned"`) {
		t.Fatalf("stored args should carry the pin, got %s", prepared.Args)
	}
	if !strings.Contains(prepared.Note, "/repo/pinned") {
		t.Fatalf("the repair should be disclosable, got note %q", prepared.Note)
	}
	// The args that come back must still satisfy the tool that will run them — the
	// check that catches a repair which "fixed" one field and broke the payload.
	if _, err := a.Registry.Get("agentTask.spawnForEdits").Decode(prepared.Args); err != nil {
		t.Fatalf("repaired args must still decode for dispatch: %v", err)
	}
}

type fakePin string

func (f fakePin) ID() string { return string(f) }

func (f fakePin) Describe() (string, string, string) { return string(f), string(f), "" }
