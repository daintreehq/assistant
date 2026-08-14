package app

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// The exported inventory must be the projection a real turn sends — not a plausible
// re-derivation of it. Pin that by comparing against a live App's own registry
// projection, so a future change to how tools are wired can't quietly leave the
// generator producing a stale or differently-shaped payload while still succeeding.
func TestBuildToolInventoryMatchesALiveRegistry(t *testing.T) {
	// newOfflineApp forces workflow intelligence OFF, matching the default options —
	// otherwise an ambient DAINTREE_WORKFLOW_INTELLIGENCE=1 would fail this comparison
	// over a difference that is correct on both sides.
	a := newOfflineApp(t)
	defer a.Shutdown()

	projected, err := a.Registry.OpenAITools(nil)
	if err != nil {
		t.Fatalf("OpenAITools: %v", err)
	}

	inv, err := BuildToolInventory(ToolInventoryOptions{})
	if err != nil {
		t.Fatalf("BuildToolInventory: %v", err)
	}
	if len(inv) != len(projected) {
		t.Fatalf("inventory has %d tools, the live registry projects %d", len(inv), len(projected))
	}
	for i := range inv {
		if inv[i].Function.Name != projected[i].Function.Name {
			t.Fatalf("tool %d is %q in the inventory, %q in the live projection — order or content drifted",
				i, inv[i].Function.Name, projected[i].Function.Name)
		}
		if inv[i].Function.Description != projected[i].Function.Description {
			t.Errorf("%s: description differs between the inventory and the live projection", inv[i].Function.Name)
		}
		if !bytes.Equal(inv[i].Function.Parameters, projected[i].Function.Parameters) {
			t.Errorf("%s: parameters differ between the inventory and the live projection", inv[i].Function.Name)
		}
	}
}

// The whole point of a committed generator is that the backend can regenerate and fail
// on a diff. That only works if two runs of an unchanged registry produce identical
// bytes — otherwise every refresh is a spurious diff and the gate gets switched off.
//
// Both flag states are covered, on full rendered bytes rather than names: the flagged
// projection is the one nothing else in the suite serializes, so a description or schema
// that varied per run in a graph tool alone would otherwise go unnoticed.
func TestRenderToolInventoryIsDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ToolInventoryOptions
	}{
		{"default", ToolInventoryOptions{}},
		{"workflow-intelligence", ToolInventoryOptions{WorkflowIntelligence: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := buildInventory(t, tc.opts)
			second := buildInventory(t, tc.opts)

			a := render(t, first)
			b := render(t, second)
			if !bytes.Equal(a, b) {
				t.Error("two runs produced different bytes — the export is not deterministic")
			}

			// Wire fidelity, stated as an equality rather than a guess about escaping
			// rules: compacting the rendered bytes must reproduce json.Marshal — the
			// exact call internal/backend/client.go makes to build a request.
			// Indentation and the trailing newline are the ONLY permitted differences.
			// This is what stops a well-meant SetEscapeHTML(false) from making the
			// fixture prettier and wrong.
			wire, err := json.Marshal(first)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var compacted bytes.Buffer
			if err := json.Compact(&compacted, a); err != nil {
				t.Fatalf("json.Compact: %v", err)
			}
			if !bytes.Equal(compacted.Bytes(), wire) {
				t.Error("compacting the rendered inventory does not reproduce json.Marshal — the fixture differs from the bytes sent on the wire")
			}

			// Indented and newline-terminated: what makes a refresh a reviewable diff
			// rather than one changed 100 KB line.
			if !bytes.HasSuffix(a, []byte("\n")) {
				t.Error("output does not end in a newline")
			}
			if !bytes.Contains(a, []byte("\n  {")) {
				t.Error("output is not indented — a refresh would diff as a single line")
			}
		})
	}
}

// The consumer does not read Go structs, it reads BYTES — so the assertions that matter
// are on the serialized document, not on the values that produced it. A json.Marshal
// round-trip cannot catch a renamed or dropped JSON tag (both sides change together),
// which is exactly how a fixture consumer breaks: the generator keeps succeeding while
// the thing it emits stops being what anyone can parse.
//
// The pinned contract is the one the backend actually relies on: a bare array of
// {"type":"function","function":{"name","description","parameters"}}, nothing else at
// either level, `type` literally "function", a non-empty description, and `parameters`
// a JSON Schema OBJECT (not null, not a string, not an array).
func TestToolInventorySerializesTheConsumerContract(t *testing.T) {
	inv := buildInventory(t, ToolInventoryOptions{})
	data := render(t, inv)

	// Decode into raw maps so a renamed or extra key is visible rather than discarded,
	// which a typed struct would silently do.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("output is not a JSON array of objects: %v", err)
	}
	if len(raw) != len(inv) {
		t.Fatalf("decoded %d tools, built %d", len(raw), len(inv))
	}
	if len(raw) == 0 {
		t.Fatal("the inventory is empty — the generator is not reaching the real registry")
	}

	seen := make(map[string]bool, len(raw))
	for i, entry := range raw {
		assertExactKeys(t, entry, "tool "+strconv.Itoa(i), "type", "function")

		var typ string
		if err := json.Unmarshal(entry["type"], &typ); err != nil || typ != "function" {
			t.Errorf("tool %d: type is %s, want \"function\"", i, entry["type"])
		}

		var fn map[string]json.RawMessage
		if err := json.Unmarshal(entry["function"], &fn); err != nil {
			t.Errorf("tool %d: function is not an object: %v", i, err)
			continue
		}
		assertExactKeys(t, fn, "tool "+strconv.Itoa(i)+" function", "name", "description", "parameters")

		var name string
		if err := json.Unmarshal(fn["name"], &name); err != nil {
			t.Errorf("tool %d: name is not a string", i)
			continue
		}
		// Wire names are the dotted internal ids projected through `.` → `__`. The
		// backend matches on that separator, so a tool that reached the wire without one
		// would be invisible to every consumer while looking perfectly valid here — the
		// silent drift this whole generator exists to make loud.
		if !strings.Contains(name, "__") {
			t.Errorf("%s has no %q separator — the backend matches wire names on it and would not see this tool", name, "__")
		}
		if strings.Contains(name, ".") {
			t.Errorf("%s still contains a dot — it was not projected to its wire form", name)
		}
		if seen[name] {
			t.Errorf("%s appears twice — a duplicate silently shadows the first", name)
		}
		seen[name] = true

		// A description is what the model chooses the tool BY. `omitempty` on the wire
		// struct means an empty one vanishes from the payload entirely, leaving a tool
		// the model can see and cannot understand.
		var desc string
		if err := json.Unmarshal(fn["description"], &desc); err != nil || strings.TrimSpace(desc) == "" {
			t.Errorf("%s has no usable description", name)
		}

		// The schema must be an OBJECT. `null`, a string or an array would all satisfy a
		// "has bytes" check and none of them is a JSON Schema; the model would read the
		// tool as taking no arguments and call it wrong.
		var params map[string]json.RawMessage
		if err := json.Unmarshal(fn["parameters"], &params); err != nil {
			t.Errorf("%s: parameters is not a JSON object: %v", name, err)
		}
	}
}

// A nil inventory must serialize as an empty ARRAY. Go's encoder renders a nil slice as
// `null`, which a consumer parsing "the list of tools" would either reject or read as an
// absent field — a materially different claim from "no tools".
func TestRenderToolInventoryEncodesEmptyAsAList(t *testing.T) {
	out, err := RenderToolInventory(nil)
	if err != nil {
		t.Fatalf("RenderToolInventory: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "[]" {
		t.Errorf("nil inventory rendered as %q, want \"[]\"", got)
	}
}

// The flag-gated execution-graph tools are excluded by default and included on request.
// Both directions matter: the default is what the backend should pin (it is what a
// normal launch sends), and the opt-in is what makes the flagged surface inspectable at
// all rather than invisible. The exact seven-tool difference is pinned separately by
// TestWorkflowGraphToolsAreExactlyTheFlagGatedDifference; this only checks the option
// reaches the registry.
func TestToolInventoryHonoursTheWorkflowIntelligenceFlag(t *testing.T) {
	names := func(opts ToolInventoryOptions) map[string]bool {
		t.Helper()
		inv := buildInventory(t, opts)
		m := make(map[string]bool, len(inv))
		for _, tool := range inv {
			m[tool.Function.Name] = true
		}
		return m
	}
	off := names(ToolInventoryOptions{})
	on := names(ToolInventoryOptions{WorkflowIntelligence: true})

	// A wire name, not an internal one: the inventory is the wire payload.
	const graphTool = "workflow__plan"
	if off[graphTool] {
		t.Errorf("%s is in the default inventory, but it only registers under the flag", graphTool)
	}
	if !on[graphTool] {
		t.Errorf("%s is missing from the flagged inventory", graphTool)
	}
	for name := range off {
		if !on[name] {
			t.Errorf("%s is in the default inventory but not the flagged one — a flag must only add", name)
		}
	}
}

/* ------------------------------- test helpers ------------------------------ */

func buildInventory(t *testing.T, opts ToolInventoryOptions) []backend.Tool {
	t.Helper()
	inv, err := BuildToolInventory(opts)
	if err != nil {
		t.Fatalf("BuildToolInventory(%+v): %v", opts, err)
	}
	return inv
}

func render(t *testing.T, inv []backend.Tool) []byte {
	t.Helper()
	out, err := RenderToolInventory(inv)
	if err != nil {
		t.Fatalf("RenderToolInventory: %v", err)
	}
	return out
}

// assertExactKeys fails if obj's key set is not exactly want — catching BOTH a dropped
// or renamed field and an added one. An added field is worth failing on too: it means
// the payload grew a region the pinned fixture does not describe.
func assertExactKeys(t *testing.T, obj map[string]json.RawMessage, what string, want ...string) {
	t.Helper()
	expected := make(map[string]bool, len(want))
	for _, k := range want {
		expected[k] = true
		if _, ok := obj[k]; !ok {
			t.Errorf("%s is missing key %q", what, k)
		}
	}
	for k := range obj {
		if !expected[k] {
			t.Errorf("%s has unexpected key %q", what, k)
		}
	}
}
