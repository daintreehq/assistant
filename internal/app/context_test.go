package app

import "testing"

// TestReadOnlyToolNamesMemoizedDefensiveCopy: the read-only-turn set is derived from
// the (append-only-after-startup) registry, computed once, and returned as a
// defensive copy so a caller mutating the result can't corrupt the memoized cache.
func TestReadOnlyToolNamesMemoizedDefensiveCopy(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	tr := newToolRunner(a)

	first := tr.ReadOnlyToolNames()
	if len(first) == 0 {
		t.Fatal("expected a non-empty read-only tool set")
	}
	// Mutate the returned slice; the cached value must be unaffected.
	first[0] = "__mutated__"

	second := tr.ReadOnlyToolNames()
	if len(second) != len(first) {
		t.Fatalf("read-only set length changed across calls: %d then %d", len(first), len(second))
	}
	if second[0] == "__mutated__" {
		t.Fatal("ReadOnlyToolNames leaked an aliased slice — caller mutation corrupted the cache")
	}
	// The read-only set must exclude the skill-context-mutating tools.
	for _, n := range second {
		if n == "skill.find" || n == "skill.load" {
			t.Fatalf("read-only set must exclude skill-context-mutating tool %q", n)
		}
	}
}
