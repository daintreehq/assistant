package backend

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// declaredTaskConstants parses a source file and returns every `Task*` string
// constant it declares, as name → value.
//
// The AST scan (rather than a hand-written list) is the whole point: a manifest
// maintained by hand is exactly the thing that silently falls behind, which is how
// the 2026-07-07 id drift went unnoticed on both sides.
func declaredTaskConstants(t *testing.T, path string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Task") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[name.Name] = val
			}
		}
	}
	return out
}

// Every Task* constant MUST appear in the manifest CheckTasks verifies. Adding a
// new task id without listing it would silently reintroduce the original hole: the
// CLI would depend on a task nothing ever checked for, and drift on it would again
// surface only as a runtime 404 mid-turn.
//
// Two ParseFile calls rather than a directory walk: this also fails loudly if a
// task constant is ever moved to a third file, which is precisely what we want.
func TestEveryTaskConstantIsInTheManifest(t *testing.T) {
	declared := map[string]string{}
	for name, val := range declaredTaskConstants(t, "tasks.go") {
		declared[name] = val
	}
	for name, val := range declaredTaskConstants(t, "workflowtasks.go") {
		declared[name] = val
	}
	if len(declared) == 0 {
		t.Fatal("AST scan found no Task* constants — the scan itself is broken")
	}

	manifest := map[string]bool{}
	for _, id := range append(CoreTaskIDs(), WorkflowTaskIDs()...) {
		manifest[id] = true
	}

	var missing []string
	for name, val := range declared {
		if !manifest[val] {
			missing = append(missing, name+" ("+val+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("Task constants declared but absent from coreTaskIDs/workflowTaskIDs: %s\n"+
			"Add them to the manifest in tasks.go or workflowtasks.go so CheckTasks verifies them.",
			strings.Join(missing, ", "))
	}

	// And the converse: the manifest must not name an id no constant declares.
	values := map[string]bool{}
	for _, v := range declared {
		values[v] = true
	}
	for id := range manifest {
		if !values[id] {
			t.Errorf("manifest lists %q but no Task* constant declares it", id)
		}
	}
}

func capsWith(ids ...string) Capabilities { return Capabilities{Tasks: ids} }

func TestCheckTasks(t *testing.T) {
	all := append(CoreTaskIDs(), WorkflowTaskIDs()...)

	t.Run("every core task present", func(t *testing.T) {
		av := CheckTasks(capsWith(CoreTaskIDs()...), false)
		if !av.Reported || !av.OK() || len(av.Missing) != 0 {
			t.Fatalf("expected a clean verdict, got %+v", av)
		}
		if av.Required != len(CoreTaskIDs()) {
			t.Errorf("Required = %d, want %d", av.Required, len(CoreTaskIDs()))
		}
	})

	t.Run("the 2026-07-07 de-versioning incident is caught", func(t *testing.T) {
		// The backend served every id with a `.v1` suffix stripped — the COUNT was
		// unchanged, which is exactly why both sides' count-only assertions passed.
		var versioned []string
		for _, id := range CoreTaskIDs() {
			versioned = append(versioned, id+".v1")
		}
		av := CheckTasks(capsWith(versioned...), false)
		if !av.Reported {
			t.Fatal("a full-but-wrong list must still count as reported")
		}
		if av.OK() {
			t.Fatal("a wholesale id rename must NOT be OK — this is the incident")
		}
		if len(av.Missing) != len(CoreTaskIDs()) {
			t.Fatalf("expected all %d ids missing, got %d: %v", len(CoreTaskIDs()), len(av.Missing), av.Missing)
		}
		if len(versioned) != av.Required {
			t.Error("count-only comparison would have passed here — that is the bug being closed")
		}
	})

	t.Run("one missing id is reported by name, sorted", func(t *testing.T) {
		av := CheckTasks(capsWith(CoreTaskIDs()[1:]...), false)
		if av.OK() {
			t.Fatal("a missing required task must not be OK")
		}
		if len(av.Missing) != 1 || av.Missing[0] != CoreTaskIDs()[0] {
			t.Fatalf("Missing = %v, want [%s]", av.Missing, CoreTaskIDs()[0])
		}
		if !sort.StringsAreSorted(av.Missing) {
			t.Error("Missing must be sorted for a stable diagnostic")
		}
	})

	t.Run("extra server tasks are forward compatibility, not drift", func(t *testing.T) {
		av := CheckTasks(capsWith(append(all, "some_future_task")...), true)
		if !av.OK() {
			t.Fatalf("a backend exposing MORE tasks must be OK, got %+v", av)
		}
	})

	t.Run("workflow tasks are required only when the flag is on", func(t *testing.T) {
		core := capsWith(CoreTaskIDs()...) // a backend predating workflow intelligence
		if av := CheckTasks(core, false); !av.OK() {
			t.Fatalf("flag off: a backend without workflow tasks must be OK, got %+v", av)
		}
		av := CheckTasks(core, true)
		if av.OK() {
			t.Fatal("flag on: missing workflow tasks must be reported")
		}
		if len(av.Missing) != len(WorkflowTaskIDs()) {
			t.Fatalf("expected the %d workflow ids missing, got %v", len(WorkflowTaskIDs()), av.Missing)
		}
	})

	t.Run("an empty inventory is unverifiable, never drifted", func(t *testing.T) {
		// /v1/daintree/capabilities sits behind require_auth AND require_ready, so a
		// live-but-still-warming backend legitimately reports nothing. Calling that
		// "drift" would fail /doctor against a healthy server.
		av := CheckTasks(Capabilities{}, false)
		if av.Reported {
			t.Error("an empty list must not count as reported")
		}
		if !av.OK() {
			t.Error("cannot-verify must not be a failure")
		}
		if len(av.Missing) != 0 {
			t.Errorf("Missing must stay empty when unreported, got %v", av.Missing)
		}
	})

	t.Run("CoreTaskIDs returns a copy", func(t *testing.T) {
		got := CoreTaskIDs()
		got[0] = "mutated"
		if CoreTaskIDs()[0] == "mutated" {
			t.Fatal("CoreTaskIDs leaked the package-level manifest")
		}
		wf := WorkflowTaskIDs()
		wf[0] = "mutated"
		if WorkflowTaskIDs()[0] == "mutated" {
			t.Fatal("WorkflowTaskIDs leaked the package-level manifest")
		}
	})
}
