package agenttaskx

import (
	"encoding/json"
	"reflect"
	"testing"
)

func keysOf(t *testing.T, args string) []string {
	t.Helper()
	return keysOfPinned(t, args, "")
}

// keysOfPinned classifies against a bound turn worktree. The pin is what the handler
// substitutes for an omitted worktreeId, so the classifier has to see the same value or
// it would refuse cohorts the handler is about to send to one known worktree.
func keysOfPinned(t *testing.T, args, pinned string) []string {
	t.Helper()
	keys, ok := spawnParallelConflictKeys(json.RawMessage(args), pinned)
	if !ok {
		t.Fatalf("spawnParallelConflictKeys(%s, %q) refused cohort membership; want keys", args, pinned)
	}
	return keys
}

// spawnParallelConflictKeys is the spawn tool's cohort-independence classifier. It
// must key on the NORMALIZED launch identity — never the raw spelling — so that
// alias twins conflict (or stay serial) instead of slipping into one cohort.
func TestSpawnParallelConflictKeys(t *testing.T) {
	// Cohort-refusing shapes: unknown/unresolvable targets stay serial.
	for name, args := range map[string]string{
		"edit into the active worktree":          `{"mode":"edit","title":"t","taskPrompt":"p"}`,
		"default mode is edit":                   `{"title":"t","taskPrompt":"p"}`,
		"whitespace worktree treated as omitted": `{"worktreeId":"  ","title":"t","taskPrompt":"p"}`,
		"branch alias needs an MCP resolve":      `{"mode":"edit","worktreeId":"main","title":"t","taskPrompt":"p"}`,
		"alias on explore also refuses":          `{"mode":"explore","worktreeId":"main","title":"t","taskPrompt":"p"}`,
		"unparseable args":                       `{"mode":`,
	} {
		if keys, ok := spawnParallelConflictKeys(json.RawMessage(args), ""); ok {
			t.Errorf("%s: got keys %v, want cohort refusal (ok=false)", name, keys)
		}
	}

	// Every accepted call carries its launch-NAME dimension (the reconciliation +
	// idempotency identity), computed with defaults applied: omitted agentId and an
	// explicit "claude" are the SAME identity and must collide.
	implicitAgent := keysOf(t, `{"mode":"explore","title":"same task","taskPrompt":"p1"}`)
	explicitAgent := keysOf(t, `{"mode":"explore","agentId":"claude","title":"same task","taskPrompt":"p2"}`)
	if !reflect.DeepEqual(implicitAgent, explicitAgent) {
		t.Errorf("normalized twins must share conflict keys: %v vs %v", implicitAgent, explicitAgent)
	}
	if len(implicitAgent) != 1 || implicitAgent[0] != "name:Claude: same task" {
		t.Errorf("explore keys = %v, want exactly [name:Claude: same task]", implicitAgent)
	}

	// A self-prefixed title ("Claude: same task") normalizes to the same launch name, so
	// it must land on the same key — reconciliation matches a terminal by that name
	// EXACTLY, so two concurrent same-named launches can cross-bind each other's
	// terminal when one goes ambiguous. (Their idempotency keys still differ here: the
	// composed prompt is part of the key and these carry different taskPrompts. Name
	// collision alone is what forces them serial.) They only ran concurrently before
	// because the redundant prefix accidentally produced a different, doubled label.
	selfPrefixed := keysOf(t, `{"mode":"explore","title":"Claude: same task","taskPrompt":"p3"}`)
	if !reflect.DeepEqual(selfPrefixed, implicitAgent) {
		t.Errorf("self-prefixed title must share conflict keys: %v vs %v", selfPrefixed, implicitAgent)
	}

	// Distinct titles → distinct name keys → freely concurrent explores.
	other := keysOf(t, `{"mode":"explore","title":"other task","taskPrompt":"p"}`)
	if reflect.DeepEqual(implicitAgent, other) {
		t.Errorf("distinct titles must not conflict: %v vs %v", implicitAgent, other)
	}

	// Edit mode adds the worktree dimension, path-CLEANED so trailing-slash /
	// dot-segment spellings of one path collide instead of overlapping.
	editA := keysOf(t, `{"mode":"edit","worktreeId":"/w/app/","title":"a","taskPrompt":"p"}`)
	editB := keysOf(t, `{"mode":"edit","worktreeId":"/w/app","title":"b","taskPrompt":"p"}`)
	wantWt := "worktree:/w/app"
	if editA[1] != wantWt || editB[1] != wantWt {
		t.Errorf("cleaned worktree keys = %q / %q, want both %q", editA[1], editB[1], wantWt)
	}
	// Distinct worktrees keep distinct keys (the parallelizable edit case).
	editC := keysOf(t, `{"mode":"edit","worktreeId":"/w/other","title":"c","taskPrompt":"p"}`)
	if editC[1] == wantWt {
		t.Errorf("distinct worktrees must not share a key: %q", editC[1])
	}
}

// A bound turn worktree is the value the HANDLER launches with when worktreeId is
// omitted, so the classifier must key on it too. Without this an edit fan-out into the
// turn's own worktree serialized on a target that was never actually unknown.
func TestSpawnParallelConflictKeysUseTheTurnWorktree(t *testing.T) {
	// Omitted + pinned resolves the edit worktree dimension instead of refusing.
	pinned := keysOfPinned(t, `{"mode":"edit","title":"a","taskPrompt":"p"}`, "/w/app/")
	if len(pinned) != 2 || pinned[1] != "worktree:/w/app" {
		t.Errorf("pinned edit keys = %v, want a cleaned worktree:/w/app dimension", pinned)
	}

	// An explicit id still WINS over the pin — naming a worktree is how the model sends
	// an agent somewhere other than where the turn began, and the key must follow the
	// launch, not the binding.
	explicit := keysOfPinned(t, `{"mode":"edit","worktreeId":"/w/other","title":"a","taskPrompt":"p"}`, "/w/app")
	if explicit[1] != "worktree:/w/other" {
		t.Errorf("explicit worktree key = %q, want worktree:/w/other", explicit[1])
	}

	// With NO pin bound the omitted case is still genuinely unknown, so an edit spawn
	// keeps refusing cohort membership rather than guessing it shares a target.
	if keys, ok := spawnParallelConflictKeys(json.RawMessage(`{"mode":"edit","title":"a","taskPrompt":"p"}`), ""); ok {
		t.Errorf("unpinned edit spawn got keys %v, want cohort refusal", keys)
	}

	// A branch-shaped pin cannot be proven to name a distinct worktree without an MCP
	// read this classifier must never make, so it refuses exactly like a branch-shaped
	// argument does.
	if keys, ok := spawnParallelConflictKeys(json.RawMessage(`{"mode":"edit","title":"a","taskPrompt":"p"}`), "main"); ok {
		t.Errorf("branch-shaped pin got keys %v, want cohort refusal", keys)
	}
}

// The spawn tool must carry the homogeneous-mutation opt-in (with its conflict
// classifier) — this is the registration parallel dispatch keys off.
func TestSpawnToolOptsIntoHomogeneousParallel(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	if !tool.ParallelHomogeneous {
		t.Fatal("agentTask.spawnForEdits must opt into homogeneous-mutation parallel dispatch")
	}
	if tool.ParallelConflictKey == nil {
		t.Fatal("agentTask.spawnForEdits must declare a ParallelConflictKey classifier")
	}
}
