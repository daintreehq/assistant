package skills

import (
	"slices"
	"strings"
	"testing"
)

// Issue #239: any built-in skill that drives the IN-TURN re-await loop (it tells the
// model to re-await the awaitAll `stillWorking` ids) MUST bound that loop and define a
// graceful escape — otherwise a genuinely hung agent holds the turn open indefinitely,
// the very gap this contract guards. The first cut of the fix updated three skills but
// silently missed a fourth (daintree.recipe.run-or-create) that had the identical
// unbounded pattern; this test makes that class of omission a hard failure instead of a
// review catch. The predicate is deliberately behavioural — "mentions stillWorking AND
// re-await" — so a new orchestration runbook can't reintroduce the loop without the bound.
func TestInTurnReawaitLoopIsBounded(t *testing.T) {
	reg, err := BuiltinRegistry()
	if err != nil {
		t.Fatalf("builtin registry: %v", err)
	}

	matched := 0
	for _, sk := range reg.List() {
		body := sk.Body
		lower := strings.ToLower(body)
		// Only the skills that actually drive the stillWorking re-await loop. A skill that
		// merely names terminal.awaitAll (or re-awaits a single misread "finished" terminal
		// in the self-heal path) without the stillWorking-budget loop is out of scope.
		if !strings.Contains(body, "stillWorking") || !strings.Contains(lower, "re-await") {
			continue
		}
		matched++

		// The defined budget: re-await at most twice (three awaitAll calls total).
		if !strings.Contains(lower, "at most twice") {
			t.Errorf("%s drives the stillWorking re-await loop but defines no re-await budget "+
				"(expected the literal %q so the bound can't be improvised)", sk.ID, "at most twice")
		}
		// The defined escape: publish to the attention queue so a hung agent surfaces.
		if !strings.Contains(body, "queue.publish") {
			t.Errorf("%s bounds the re-await loop but defines no escalation (expected a queue.publish escape)", sk.ID)
		}
		// The escape tool must be declared so the boot-time requiredTools registry
		// cross-check (TestBootCleanWithFullToolSet) stays satisfied.
		if !slices.Contains(sk.RequiredTools, "queue.publish") {
			t.Errorf("%s uses queue.publish in its body but omits it from requiredTools", sk.ID)
		}
	}

	// Guard the guard: if a refactor renames stillWorking or the predicate stops matching,
	// the test would vacuously pass — so assert it actually covered the known runbooks.
	if matched < 4 {
		t.Fatalf("expected the re-await bound to cover >=4 orchestration skills, only matched %d", matched)
	}
}
