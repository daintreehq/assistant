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
// and unrunnable in a timer, which fires with no turn to inherit from. The handler
// refuses it at fire time. PrepareUnattended's job is to make sure it never gets that
// far: the schedule path runs INSIDE the turn, so the value the fired call will need
// is readable right now and gets written into the stored args.
func TestSpawnPrepareUnattendedSubstitutesTheTurnsWorktree(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{WorktreePin: fakePin("/repo/pinned")})
	if tool.PrepareUnattended == nil {
		t.Fatal("spawn must declare an unattended prepare — it is the tool most often scheduled")
	}
	for name, args := range map[string]string{
		"omitted": `{"title":"T","taskPrompt":"p"}`,
		"empty":   `{"title":"T","taskPrompt":"p","worktreeId":""}`,
		"blank":   `{"title":"T","taskPrompt":"p","worktreeId":"   "}`,
	} {
		repaired, note, refusal := tool.PrepareUnattended(json.RawMessage(args))
		if refusal != "" {
			t.Errorf("%s: a bound pin makes this schedulable, got refusal %q", name, refusal)
			continue
		}
		// The repaired args are what gets STORED and later dispatched, so the pin has
		// to be in them — a note alone would leave the same doomed row behind.
		var got spawnArgs
		if err := json.Unmarshal(repaired, &got); err != nil {
			t.Fatalf("%s: repaired args must be valid spawn args: %v", name, err)
		}
		if got.WorktreeID != "/repo/pinned" {
			t.Errorf("%s: repaired worktreeId = %q, want the pin", name, got.WorktreeID)
		}
		// The rest of the call survives the round trip untouched.
		if got.Title != "T" || got.TaskPrompt != "p" {
			t.Errorf("%s: repair should not disturb the other args, got %+v", name, got)
		}
		// The user has to be able to see WHICH worktree was chosen while the timer is
		// still trivially cancellable, so the note names it.
		if !strings.Contains(note, "/repo/pinned") {
			t.Errorf("%s: note should name the resolved worktree, got %q", name, note)
		}
	}
}

// An UNBOUND pin is the one case nothing can rescue: there is no "here" to capture,
// so the refusal stands and names the argument the model must add.
func TestSpawnPrepareUnattendedRefusesWithNoPin(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	for name, args := range map[string]string{
		"nil pin":   `{"title":"T","taskPrompt":"p"}`,
		"blank pin": `{"title":"T","taskPrompt":"p","worktreeId":"  "}`,
	} {
		repaired, _, refusal := tool.PrepareUnattended(json.RawMessage(args))
		if refusal == "" {
			t.Errorf("%s: with nothing to substitute this cannot be scheduled", name)
		}
		if !strings.Contains(refusal, "worktreeId") {
			t.Errorf("%s: reason should name worktreeId, got %q", name, refusal)
		}
		if repaired != nil {
			t.Errorf("%s: a refusal must not hand back args to store, got %s", name, repaired)
		}
	}
	// A pin bound to whitespace is no pin at all.
	if _, _, refusal := newSpawnForEditsTool(Deps{WorktreePin: fakePin("   ")}).PrepareUnattended(
		json.RawMessage(`{"title":"T","taskPrompt":"p"}`)); refusal == "" {
		t.Error("a whitespace pin should not be substituted")
	}
}

// An explicit worktree is left exactly as written — the hook repairs an OMISSION, it
// does not get an opinion about a value the model chose deliberately.
func TestSpawnPrepareUnattendedLeavesAnExplicitWorktreeAlone(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{WorktreePin: fakePin("/repo/pinned")})
	repaired, note, refusal := tool.PrepareUnattended(
		json.RawMessage(`{"title":"T","taskPrompt":"p","worktreeId":"/repo/elsewhere"}`))
	if refusal != "" || repaired != nil || note != "" {
		t.Errorf("an explicit worktree needs no repair, got (%s, %q, %q)", repaired, note, refusal)
	}
}

// Unparseable args are NOT this hook's business. Decode owns malformed input and
// produces a better message for it; objecting here would replace that with a worse one.
func TestSpawnPrepareIgnoresUnparseableArgs(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{WorktreePin: fakePin("/repo/pinned")})
	if _, _, refusal := tool.PrepareUnattended(json.RawMessage(`{`)); refusal != "" {
		t.Errorf("malformed args belong to Decode, got %q", refusal)
	}
}

// fakePin is a worktree binding that is already resolved — the schedule path only ever
// reads it, so there is nothing else to model.
type fakePin string

func (f fakePin) ID() string { return string(f) }

func (f fakePin) Describe() (string, string, string) { return string(f), string(f), "" }
