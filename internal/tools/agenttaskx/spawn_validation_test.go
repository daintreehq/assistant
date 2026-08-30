package agenttaskx

import (
	"encoding/json"
	"strings"
	"testing"
)

// Finding 1: agentTask.spawnForEdits advertises required title/taskPrompt and a
// mode enum (edit|explore) that strict decoding didn't enforce — an empty title
// or taskPrompt would reach spawn with a blank prompt, and an arbitrary mode would
// silently become edit-mode. Validate() must reject all of these as INVALID_ARGS.
func TestSpawnForEditsRejectsRequiredAndEnumGaps(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	for name, bad := range map[string]string{
		"missing title":      `{"taskPrompt":"do the thing"}`,
		"empty title":        `{"title":"","taskPrompt":"do the thing"}`,
		"blank title":        `{"title":"   ","taskPrompt":"do the thing"}`,
		"missing taskPrompt": `{"title":"T"}`,
		"empty taskPrompt":   `{"title":"T","taskPrompt":""}`,
		"blank taskPrompt":   `{"title":"T","taskPrompt":"  "}`,
		"bad mode":           `{"title":"T","taskPrompt":"p","mode":"refactor"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("%s should be rejected: %s", name, bad)
		}
	}
	// Valid edit/explore/default-mode calls still decode.
	for _, good := range []string{
		`{"title":"T","taskPrompt":"p"}`,
		`{"title":"T","taskPrompt":"p","mode":"edit"}`,
		`{"title":"T","taskPrompt":"p","mode":"explore"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(good)); err != nil {
			t.Errorf("valid spawn args should decode: %s — %v", good, err)
		}
	}
}

// The schedule-time half of the worktree rule.
//
// Omitting worktreeId means "the worktree this turn started in" — correct in a turn,
// and unrunnable in a timer, which fires with no turn to inherit from. The handler has
// always refused it at fire time; the preflight is what moves that refusal to the
// moment the model writes the call, while it can still fix it.
func TestSpawnPreflightUnattendedNeedsAnExplicitWorktree(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	if tool.PreflightUnattended == nil {
		t.Fatal("spawn must declare an unattended preflight — it is the tool most often scheduled")
	}
	for name, args := range map[string]string{
		"omitted": `{"title":"T","taskPrompt":"p"}`,
		"empty":   `{"title":"T","taskPrompt":"p","worktreeId":""}`,
		"blank":   `{"title":"T","taskPrompt":"p","worktreeId":"   "}`,
	} {
		why := tool.PreflightUnattended(json.RawMessage(args))
		if why == "" {
			t.Errorf("%s worktreeId should be refused for an unattended dispatch: %s", name, args)
		}
		// The reason names the argument to add, since the model's next move is to
		// rewrite this exact call.
		if why != "" && !strings.Contains(why, "worktreeId") {
			t.Errorf("%s: reason should name worktreeId, got %q", name, why)
		}
	}

	if why := tool.PreflightUnattended(
		json.RawMessage(`{"title":"T","taskPrompt":"p","worktreeId":"/repo/wt"}`)); why != "" {
		t.Errorf("an explicit worktree is schedulable, got %q", why)
	}
}

// Unparseable args are NOT the preflight's business. Decode owns malformed input and
// produces a better message for it; objecting here would replace that with a worse one.
func TestSpawnPreflightIgnoresUnparseableArgs(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	if why := tool.PreflightUnattended(json.RawMessage(`{`)); why != "" {
		t.Errorf("malformed args belong to Decode, got %q", why)
	}
}
