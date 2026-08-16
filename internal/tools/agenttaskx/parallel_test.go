package agenttaskx

import (
	"encoding/json"
	"reflect"
	"testing"
)

func keysOf(t *testing.T, args string) []string {
	t.Helper()
	keys, ok := spawnParallelConflictKeys(json.RawMessage(args))
	if !ok {
		t.Fatalf("spawnParallelConflictKeys(%s) refused cohort membership; want keys", args)
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
		if keys, ok := spawnParallelConflictKeys(json.RawMessage(args)); ok {
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
