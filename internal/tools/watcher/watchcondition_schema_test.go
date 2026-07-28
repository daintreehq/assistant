package watcher

import (
	"encoding/json"
	"reflect"
	"testing"
)

// stripDescriptions recursively removes every "description" key, leaving only the
// MACHINE-READABLE half of a JSON Schema.
func stripDescriptions(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if k == "description" {
				continue
			}
			out[k] = stripDescriptions(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripDescriptions(val)
		}
		return out
	default:
		return v
	}
}

func terminalCreateProps(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(terminalCreateSchema, &schema); err != nil {
		t.Fatalf("terminalCreateSchema is not valid JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("terminalCreateSchema has no properties object")
	}
	return props
}

// stopWhen carries the full leaf prose and alertWhen a terse pointer to it — that
// asymmetry is a DOCUMENTATION saving only. Structurally the two must remain the
// identical union, or the model gets a weaker contract for alertWhen and can emit a
// shape the domain validator then rejects (the exact class of failure the schema
// exists to make impossible).
func TestStopWhenAndAlertWhenAreStructurallyIdentical(t *testing.T) {
	props := terminalCreateProps(t)
	stop := stripDescriptions(props["stopWhen"])
	alert := stripDescriptions(props["alertWhen"])
	if !reflect.DeepEqual(stop, alert) {
		sb, _ := json.MarshalIndent(stop, "", "  ")
		ab, _ := json.MarshalIndent(alert, "", "  ")
		t.Fatalf("stopWhen and alertWhen differ once descriptions are stripped:\nstopWhen:\n%s\n\nalertWhen:\n%s", sb, ab)
	}
}

// Both copies must still enumerate every union key AND carry the exactly-one-key
// encoding. A terse rendering that quietly dropped a leaf would silently remove a
// capability from alertWhen.
func TestBothConditionsCarryTheFullUnion(t *testing.T) {
	wantKeys := []string{
		"stateIs", "runtimeStatusIs", "contains", "regex",
		"noOutputForMs", "modelJudge", "all", "any", "not",
	}
	props := terminalCreateProps(t)
	for _, role := range []string{"stopWhen", "alertWhen"} {
		sub, ok := props[role].(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", role)
		}
		if sub["minProperties"] != float64(1) || sub["maxProperties"] != float64(1) {
			t.Errorf("%s must encode exactly-one-key as minProperties/maxProperties 1, got %v/%v",
				role, sub["minProperties"], sub["maxProperties"])
		}
		if sub["additionalProperties"] != false {
			t.Errorf("%s must set additionalProperties:false", role)
		}
		leaves, ok := sub["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", role)
		}
		if len(leaves) != len(wantKeys) {
			t.Errorf("%s has %d union keys, want %d", role, len(leaves), len(wantKeys))
		}
		for _, k := range wantKeys {
			leaf, ok := leaves[k].(map[string]any)
			if !ok {
				t.Errorf("%s is missing union key %q", role, k)
				continue
			}
			// Every leaf must still be DOCUMENTED — terse is fine, absent is not.
			if d, _ := leaf["description"].(string); d == "" {
				t.Errorf("%s.%s has no description", role, k)
			}
		}
	}
}

// The whole point of the split was size. Pin a ceiling so the duplication cannot
// creep back: the inventory ships on EVERY model round.
func TestTerminalCreateSchemaStaysBounded(t *testing.T) {
	var v any
	if err := json.Unmarshal(terminalCreateSchema, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v) // canonical wire form, as the registry emits it
	if err != nil {
		t.Fatal(err)
	}
	// 4384 today, down from 4953 when both copies carried the full leaf prose.
	// The headroom absorbs a small wording change; a second full copy would blow it.
	const ceiling = 4500
	if len(b) > ceiling {
		t.Errorf("watcher.terminal.create schema is %d bytes, ceiling %d — did the leaf prose get duplicated again?", len(b), ceiling)
	}
}
