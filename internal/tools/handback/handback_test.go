package handback

import (
	"context"
	"testing"
	"time"
)

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
		{"untyped declaration", map[string]any{"properties": map[string]any{"handback": map[string]any{"description": "x"}}}, true, true},
		// `false` FORBIDS the key; a non-boolean type would reject the `true` we send.
		{"declared false", map[string]any{"properties": map[string]any{"handback": false}}, true, false},
		{"declared null", map[string]any{"properties": map[string]any{"handback": nil}}, true, false},
		{"declared as a string", map[string]any{"properties": map[string]any{"handback": map[string]any{"type": "string"}}}, true, false},
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
	// Incidental mentions are unrelated failures that may have come AFTER dispatch.
	for _, text := range []string{
		"Terminal not found",
		"EBADF: terminal handback-worker not found",
		"command `echo handback` timed out",
		"lastHandback is unavailable",
	} {
		if IsRefusal(true, text) {
			t.Errorf("%q is not a refusal of the flag and must not be re-sent", text)
		}
	}
	// A success whose text happens to mention the word was not refused.
	if IsRefusal(false, "handback requested") {
		t.Error("only an error result can be a refusal")
	}
}

func TestLookupContextExpiresByCancelNotDeadline(t *testing.T) {
	old := lookupBudget
	lookupBudget = 10 * time.Millisecond
	defer func() { lookupBudget = old }()

	ctx, done := LookupContext(context.Background())
	defer done()
	if _, has := ctx.Deadline(); has {
		t.Fatal("the bound must be a cancel, not a deadline")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup context never expired on its own")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expiry surfaced as %v, want Canceled", ctx.Err())
	}
}
