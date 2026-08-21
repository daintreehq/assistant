package mcp

import (
	"context"
	"testing"
)

// The substituted default is a DISPLAY convenience: it stands in so a caller
// rendering the catalog never has to nil-check. A caller that VALIDATES against it
// gets the opposite of what it asked for — {"type":"object","properties":{}} accepts
// every possible object, so "we have a schema" would be true for a tool that
// published none and the check would pass vacuously.
//
// InputSchemaProvided is the bit that separates the two, and it has to be pinned
// here: every consumer test injects its own ToolInfo downstream of this mapping, so
// nothing else would notice the field going missing.
func TestListToolsReportsWhetherServerProvidedTheSchema(t *testing.T) {
	real := map[string]any{"type": "object", "properties": map[string]any{"cwd": map[string]any{"type": "string"}}}
	low := &fakeLow{tools: []rawTool{
		{Name: "advertised", Description: "has a schema", InputSchema: real},
		{Name: "silent", Description: "advertised none", InputSchema: nil},
		// A non-object schema value is not a schema either; it must not be mistaken
		// for one just because the field was populated.
		{Name: "malformed", Description: "schema is not an object", InputSchema: "nonsense"},
	}}
	c := newInjected(low)
	got, err := c.ListTools(context.Background(), true)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]ToolInfo{}
	for _, ti := range got {
		byName[ti.Name] = ti
	}

	adv := byName["advertised"]
	if !adv.InputSchemaProvided {
		t.Error("a server-advertised schema must be reported as provided")
	}
	props, ok := adv.InputSchema["properties"].(map[string]any)
	if !ok || props["cwd"] == nil {
		t.Errorf("the advertised schema must survive verbatim, got %v", adv.InputSchema)
	}

	for _, name := range []string{"silent", "malformed"} {
		ti := byName[name]
		if ti.InputSchemaProvided {
			t.Errorf("%s: a substituted default must NOT be reported as provided", name)
		}
		if ti.InputSchema == nil {
			t.Errorf("%s: the default must still be substituted for display callers", name)
		}
	}
}
