package app

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/mcp"
)

// The mcpx seam silently dropped InputSchema for the whole life of the family,
// which is why no local tool could report an MCP tool's argument shape (#311).
// The mapper is pure and tested directly so the field can never be dropped again
// without a red test — a nested schema is used because a shallow one would still
// pass if the mapping degraded to a top-level copy.
func TestToMcpxToolInfosForwardsNestedInputSchema(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"options": map[string]any{
				"type":       "object",
				"properties": map[string]any{"scopePaths": map[string]any{"type": "array"}},
				"required":   []any{"scopePaths"},
			},
		},
	}
	in := []mcp.ToolInfo{
		{Name: "copyTree.generate", Description: "Generate a copy tree.", InputSchema: nested},
		{Name: "terminal.getStatus", Description: "Status.", InputSchema: map[string]any{"type": "object"}},
	}

	out := toMcpxToolInfos(in)
	if len(out) != len(in) {
		t.Fatalf("mapped %d entries, want %d", len(out), len(in))
	}
	for i, got := range out {
		if got.Name != in[i].Name || got.Description != in[i].Description {
			t.Errorf("entry %d: name/description not forwarded: %+v", i, got)
		}
		if got.InputSchema == nil {
			t.Fatalf("entry %d (%s): InputSchema dropped at the app seam", i, got.Name)
		}
	}

	gotJSON, _ := json.Marshal(out[0].InputSchema)
	wantJSON, _ := json.Marshal(nested)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("nested schema altered in transit:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// A nil schema must stay nil rather than becoming an empty non-nil map: the
// client already substitutes its own default, and a second substitution here
// would make "server omitted a schema" indistinguishable at the consumer.
func TestToMcpxToolInfosPreservesNilSchema(t *testing.T) {
	out := toMcpxToolInfos([]mcp.ToolInfo{{Name: "x", Description: "d"}})
	if len(out) != 1 {
		t.Fatalf("mapped %d entries, want 1", len(out))
	}
	if out[0].InputSchema != nil {
		t.Errorf("nil schema should stay nil, got %v", out[0].InputSchema)
	}
}

// Mapping an empty catalog must yield an empty (non-nil) slice, not a nil that
// a caller could mistake for an error result.
func TestToMcpxToolInfosEmpty(t *testing.T) {
	out := toMcpxToolInfos(nil)
	if out == nil {
		t.Error("empty catalog should map to an empty slice, not nil")
	}
	if len(out) != 0 {
		t.Errorf("want 0 entries, got %d", len(out))
	}
}
