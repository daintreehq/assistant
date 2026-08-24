package tools

import "testing"

// Finding 3: List/OpenAITools previously ranged over the tools map, so the
// projected tool-spec order (and the read-only/filter projection derived from
// List) reordered across runs — churning the prompt cache, a stability invariant.
// Registration order must now be the deterministic projection order.
func TestRegistrationOrderIsStable(t *testing.T) {
	names := []string{"fs.read", "fs.list", "context.snapshot", "memory.recall", "runbook.find"}
	build := func() *Registry {
		r := NewRegistry()
		for _, n := range names {
			if err := r.Register(noopTool(n)); err != nil {
				t.Fatalf("register %s: %v", n, err)
			}
		}
		return r
	}

	// List preserves registration order, identically every time.
	for run := 0; run < 50; run++ {
		got := build().List()
		if len(got) != len(names) {
			t.Fatalf("List len = %d, want %d", len(got), len(names))
		}
		for i, tool := range got {
			if tool.Name != names[i] {
				t.Fatalf("run %d: List[%d] = %q, want %q", run, i, tool.Name, names[i])
			}
		}
	}

	// OpenAITools (unfiltered) projects in registration order.
	specs, err := build().OpenAITools(nil)
	if err != nil {
		t.Fatalf("OpenAITools: %v", err)
	}
	for i, sp := range specs {
		want := toWireName(names[i])
		if sp.Function.Name != want {
			t.Fatalf("OpenAITools[%d] = %q, want %q", i, sp.Function.Name, want)
		}
	}
}

// A narrowed (filtered) projection must follow filterNames order — coreTools then
// each runbook's requiredTools — de-duped, skipping unregistered names. That keeps
// the per-turn toolset byte-stable across runs for the same loaded-runbook set.
func TestFilteredProjectionFollowsFilterOrder(t *testing.T) {
	r := NewRegistry()
	// Register in one order...
	for _, n := range []string{"a", "b", "c", "d"} {
		_ = r.Register(noopTool(n))
	}
	// ...but request a different, explicit filter order with a dup + an unknown name.
	filter := []string{"c", "a", "c", "zzz_unknown", "b"}
	specs, err := r.OpenAITools(filter)
	if err != nil {
		t.Fatalf("OpenAITools: %v", err)
	}
	want := []string{"c", "a", "b"}
	if len(specs) != len(want) {
		t.Fatalf("got %d specs, want %d", len(specs), len(want))
	}
	for i, sp := range specs {
		if sp.Function.Name != want[i] {
			t.Fatalf("spec[%d] = %q, want %q", i, sp.Function.Name, want[i])
		}
	}
}
