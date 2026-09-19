package handback

import "testing"

func TestSchemaAccepts(t *testing.T) {
	supported := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":   map[string]any{"type": "string"},
			"handback": map[string]any{"type": "boolean"},
		},
	}
	cases := []struct {
		name     string
		schema   map[string]any
		provided bool
		want     bool
	}{
		{"advertised top-level property", supported, true, true},
		// The stand-in the client substitutes for a server that published nothing
		// accepts every object; it must never read as support.
		{"populated but not advertised", supported, false, false},
		{"advertised without the property", map[string]any{"properties": map[string]any{"prompt": map[string]any{}}}, true, false},
		{"nested only", map[string]any{"properties": map[string]any{
			"options": map[string]any{"properties": map[string]any{"handback": map[string]any{}}},
		}}, true, false},
		{"no properties key", map[string]any{"type": "object"}, true, false},
		{"malformed properties", map[string]any{"properties": []any{"handback"}}, true, false},
		{"nil schema", nil, true, false},
	}
	for _, tc := range cases {
		if got := SchemaAccepts(tc.schema, tc.provided); got != tc.want {
			t.Errorf("%s: SchemaAccepts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsRefusal(t *testing.T) {
	if !IsRefusal(true, "handback needs an agent pane, and terminal 't1' has no agent running. Send without handback.") {
		t.Error("the host's refusal of the flag must be recognised")
	}
	if !IsRefusal(true, "Unrecognized key: \"Handback\"") {
		t.Error("a strict-schema rejection naming the argument is the same refusal")
	}
	if IsRefusal(true, "Terminal not found") {
		t.Error("an unrelated rejection is not a handback refusal")
	}
	// A success whose text happens to mention the word was not refused.
	if IsRefusal(false, "handback requested") {
		t.Error("only an error result can be a refusal")
	}
}
