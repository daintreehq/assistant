package watcher

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
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
//
// This comparison is NARROW, and it is worth being honest about how narrow. Both
// copies render from ONE template (watchConditionSchema), so a keyword deleted from
// that template disappears from both and this test still passes. It catches a future
// hand-inlined second copy drifting from the generated one — nothing else. The
// regression it cannot see (a lost enum value, a dropped bound) is pinned against a
// hardcoded expectation in TestBothConditionsCarryTheFullUnion instead.
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
	// Pinned by SHAPE against a hardcoded expectation, not just by key name. Keys
	// alone would pass while an enum value, a type or a bound went missing from the
	// shared template — and because both copies come from that one template, the
	// identity comparison above cannot see it either.
	wantLeaves := map[string]struct {
		typ    string
		enum   []string
		bounds map[string]float64
	}{
		"stateIs": {typ: "string", enum: []string{
			"idle", "working", "waiting", "directing", "completed", "exited",
		}},
		"runtimeStatusIs": {typ: "string", enum: []string{"running", "exited"}},
		"contains":        {typ: "string", bounds: map[string]float64{"minLength": 1}},
		"regex":           {typ: "string", bounds: map[string]float64{"minLength": 1}},
		"noOutputForMs":   {typ: "integer", bounds: map[string]float64{"minimum": 1}},
		// Watchers DO support modelJudge (unlike the extract tools' wait, which
		// rejects it) — so here it must stay generable.
		"modelJudge": {typ: "string", bounds: map[string]float64{"minLength": 1}},
		"all":        {typ: "array", bounds: map[string]float64{"minItems": 1}},
		"any":        {typ: "array", bounds: map[string]float64{"minItems": 1}},
		"not": {typ: "object", bounds: map[string]float64{
			"minProperties": 1, "maxProperties": 1,
		}},
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
		if len(leaves) != len(wantLeaves) {
			t.Errorf("%s has %d union keys, want %d", role, len(leaves), len(wantLeaves))
		}
		for k, want := range wantLeaves {
			leaf, ok := leaves[k].(map[string]any)
			if !ok {
				t.Errorf("%s is missing union key %q", role, k)
				continue
			}
			if leaf["type"] != want.typ {
				t.Errorf("%s.%s has type %v, want %q", role, k, leaf["type"], want.typ)
			}
			if want.enum != nil {
				raw, _ := leaf["enum"].([]any)
				got := make([]string, 0, len(raw))
				for _, v := range raw {
					str, _ := v.(string)
					got = append(got, str)
				}
				if !slices.Equal(got, want.enum) {
					t.Errorf("%s.%s enum is %v, want %v — a state dropped here is one no watcher can stop on",
						role, k, got, want.enum)
				}
			}
			for kw, n := range want.bounds {
				if leaf[kw] != n {
					t.Errorf("%s.%s lost %s (got %v, want %v)", role, k, kw, leaf[kw], n)
				}
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

// The split only pays if the two roles are wired to DIFFERENT modes, and nothing
// else in this file notices if they are not: both renderings satisfy every
// structural assertion and both document every leaf, so flipping either call site
// in terminalCreateSchema passes the whole file. The byte ceiling above is not a
// backstop in the other direction either — making stopWhen terse SHRINKS the
// schema, so it sails under the ceiling while silently deleting the hard-won
// warnings (the stateIs:'waiting' trap, the modelJudge cost note) that are the
// reason stopWhen is the verbose copy.
func TestLeafDocsAreWiredToTheRightRoles(t *testing.T) {
	// A clause that exists ONLY in the verbose rendering.
	const verboseOnly = "A bare stateIs:'waiting' fires too early"
	stateIsDesc := func(role string) string {
		leaf, _ := terminalCreateProps(t)[role].(map[string]any)
		leaves, _ := leaf["properties"].(map[string]any)
		s, _ := leaves["stateIs"].(map[string]any)
		d, _ := s["description"].(string)
		return d
	}

	if !strings.Contains(stateIsDesc("stopWhen"), verboseOnly) {
		t.Error("stopWhen must carry the FULL leaf prose — it is the copy alertWhen points at")
	}
	if strings.Contains(stateIsDesc("alertWhen"), verboseOnly) {
		t.Error("alertWhen is rendering the verbose leaves — the duplication this split removed is back")
	}

	// And the terse rendering must actually be smaller, or the split has eroded to
	// a no-op that still passes every assertion above.
	const minSaving = 400
	saved := len(watchConditionSchema("role.", true)) - len(watchConditionSchema("role.", false))
	if saved < minSaving {
		t.Errorf("the terse rendering now saves only %d bytes (want >= %d) — the leafDocs split has eroded", saved, minSaving)
	}
}
