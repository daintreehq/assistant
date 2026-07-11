package app

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// parallel_mutation_test.go pins the ParallelMutationSafe authorization bar: a
// homogeneous-mutation cohort may form ONLY when every member is already fully
// authorized — interactive main actor, tier allows the risk, auto-approve on. Any
// leg failing keeps the spawn fan-out on the serial path (where the confirmation
// prompt / grant machinery runs one call at a time, as it must).
func TestParallelMutationSafeRequiresFullPreAuthorization(t *testing.T) {
	a := newOfflineApp(t) // tier operator (allows RiskProject), auto-approve off, main actor
	t.Cleanup(func() { _ = a.Shutdown() })
	runner := newToolRunner(a)

	const spawn = "agentTask.spawnForEdits"

	// Auto-approve off: the call would raise a confirmation prompt → serial.
	if runner.ParallelMutationSafe(spawn) {
		t.Fatal("without auto-approve, a confirming mutation must never join a cohort")
	}

	// Auto-approve on + main actor + allowing tier: the full bar is met.
	a.cfgMu.Lock()
	a.Config.AutoApprove = true
	a.cfgMu.Unlock()
	if !runner.ParallelMutationSafe(spawn) {
		t.Fatal("a pre-authorized spawn (main + auto-approve + allowed tier) must qualify")
	}

	// The opt-in is per-tool: an ordinary mutating tool (no ParallelHomogeneous)
	// and a read tool never qualify, whatever the authorization state.
	if runner.ParallelMutationSafe("timer.create") {
		t.Fatal("a mutating tool without the ParallelHomogeneous opt-in must not qualify")
	}
	if runner.ParallelMutationSafe("terminal.extract") {
		t.Fatal("a read tool belongs to the ParallelSafe grouping, never the mutation cohort")
	}

	// A tier that denies the risk closes the gate even with auto-approve on.
	a.SetTier(domain.TierSupervisor)
	if runner.ParallelMutationSafe(spawn) {
		t.Fatal("a tier-denied tool must never join a cohort")
	}
	a.SetTier(domain.TierOperator)

	// A non-interactive actor (the daemon's wake turns) authorizes via consumable
	// grants, whose consumption order must stay deterministic → serial.
	a.dispatchActor = domain.ActorWake
	if runner.ParallelMutationSafe(spawn) {
		t.Fatal("a non-main actor must never form a mutation cohort")
	}
	a.dispatchActor = domain.ActorMain

	// The conflict-key seam: unknown tools refuse cohorts; the spawn tool's real
	// classifier is wired through (an explore spawn carries its launch-name
	// identity dimension and nothing else).
	if _, ok := runner.ParallelConflictKey("no.such.tool", nil); ok {
		t.Fatal("an unknown tool must refuse cohort membership")
	}
	keys, ok := runner.ParallelConflictKey(spawn, json.RawMessage(`{"mode":"explore","title":"t","taskPrompt":"p"}`))
	if !ok || len(keys) != 1 || keys[0] != "name:Claude: t" {
		t.Fatalf("explore spawn keys = (%v, %v), want ([name:Claude: t], true)", keys, ok)
	}
}
