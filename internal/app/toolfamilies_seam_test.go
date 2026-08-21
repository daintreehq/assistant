package app

import (
	"testing"

	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/tools/mcpx"
)

// The catalog seam is the single point where an MCP action's argument contract
// either reaches the tool family or is dropped. It WAS dropped — the mapping
// carried only name and description — and the consequence was that no local tool
// could report an action's argument shape at all, which is the gap tool.schema and
// daintree.invoke are both built on top of.
//
// Every mcpx test constructs MCPToolInfo directly, i.e. downstream of this line, so
// nothing over there would fail if this mapping regressed: tool.schema would return
// a placeholder and every dynamic invocation would be refused as unvalidatable,
// with a fully green suite.
func TestToMcpxToolInfosCarriesTheWholeContract(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"options": map[string]any{
				"type":       "object",
				"properties": map[string]any{"depth": map[string]any{"type": "integer"}},
			},
		},
		"required": []any{"options"},
	}
	got := toMcpxToolInfos([]mcp.ToolInfo{
		{Name: "recipe.run", Description: "run a recipe", InputSchema: nested, InputSchemaProvided: true},
		{Name: "silent.action", Description: "no schema", InputSchema: map[string]any{"type": "object"}},
	})
	if len(got) != 2 {
		t.Fatalf("mapped %d entries, want 2", len(got))
	}

	if got[0].Name != "recipe.run" || got[0].Description != "run a recipe" {
		t.Errorf("identity lost: %+v", got[0])
	}
	if !got[0].InputSchemaProvided {
		t.Error("InputSchemaProvided must survive the seam")
	}
	// Not just "a schema is present" — the NESTED shape, which is the whole reason
	// the raw schema is worth returning rather than a flattened key list.
	props, ok := got[0].InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties lost: %+v", got[0].InputSchema)
	}
	opts, ok := props["options"].(map[string]any)
	if !ok {
		t.Fatalf("nested object lost: %+v", props)
	}
	inner, ok := opts["properties"].(map[string]any)
	if !ok || inner["depth"] == nil {
		t.Errorf("nested properties lost: %+v", opts)
	}
	if got[0].InputSchema["required"] == nil {
		t.Error("required list lost")
	}

	// The "server published nothing" bit must survive as FALSE, or the substituted
	// permissive default would be validated against as though it were a contract.
	if got[1].InputSchemaProvided {
		t.Error("InputSchemaProvided must stay false when the server advertised none")
	}
}

// A dynamically-classified MCP action must never collide with a LOCAL tool name,
// across every wrapper package.
//
// mcpx can only check itself: its two wrapper indexes are the daintree.call
// denylist and its own registration list, and neither sees internal/tools/mcpwrap
// — where recipe, workflow, worktree and forge wrappers live. So the invariant
// "a classified action has no typed wrapper" is only half-enforced over there.
// This is the other half, and it belongs here because this is the only package
// that has the whole registry.
//
// The failure it prevents: someone adds a typed wrapper for an action the catalog
// classifies. Both routes then exist with different policies, and which one the
// model takes depends on which index it consulted.
func TestClassifiedMCPActionsDoNotCollideWithLocalTools(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	registered := map[string]bool{}
	for _, tool := range a.Registry.List() {
		if tool != nil {
			registered[tool.Name] = true
		}
	}
	for _, action := range mcpx.ClassifiedActionNames() {
		if registered[action] {
			t.Errorf("%s is dynamically classified AND registered as a local tool — the typed tool must win, so "+
				"remove the classification (and add the raw name to the daintree.call denylist)", action)
		}
	}
	if len(mcpx.ClassifiedActionNames()) == 0 {
		t.Fatal("no classified actions — the check above would be vacuous")
	}
}
