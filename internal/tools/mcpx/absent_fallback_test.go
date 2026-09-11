package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// A Daintree build that predates the capability catalog: slashCommands.list is
// served, agentCapabilities.* is not. This is every develop build today.
func legacyCapabilityHost() *fakeMCP {
	return &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "slashCommands.list", Description: "List the slash commands an agent CLI offers, including any the project defines locally."},
		{Name: "terminal.rename", Description: "Rename the terminal tab."},
	}}
}

func searchFor(t *testing.T, mcp *fakeMCP, query string) tools.ToolResult {
	t.Helper()
	tool := newSearchTool(Deps{MCP: mcp})
	args, _ := json.Marshal(map[string]string{"query": query})
	decoded, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		t.Fatalf("decode %q: %v", query, err)
	}
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("search %q failed: %+v", query, res)
	}
	return res
}

func noteOf(t *testing.T, res tools.ToolResult) string {
	t.Helper()
	raw, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("result shape: %T", res.Result)
	}
	note, _ := raw["note"].(string)
	return note
}

// The observed failure: the runbook says to search for the catalog, the host has
// never heard of it, and a bare "Found 0" sent the model to search files for a
// user-level command the fs.* root cannot reach. The zero result must name the
// served stand-in, in the summary line the model reads first and in the note.
func TestSearchNamesServedFallbackWhenCatalogIsAbsent(t *testing.T) {
	// The last one is a query the model actually sent: extra words that appear in no
	// policy text ("skills", "commands") must not defeat an explicit identifier.
	for _, q := range []string{"agentCapabilities search", "agentCapabilities.search", "agentcapabilities", "agentCapabilities search skills commands"} {
		res := searchFor(t, legacyCapabilityHost(), q)
		for where, text := range map[string]string{"summary": res.Summary, "note": noteOf(t, res)} {
			if !strings.Contains(text, "not offered by this Daintree build") || !strings.Contains(text, "`slashCommands.list`") {
				t.Errorf("%q: %s should name slashCommands.list as the served stand-in, got %q", q, where, text)
			}
		}
	}
	// Both catalog actions answer to "agentCapabilities", and they share a fallback,
	// so the steer names both once rather than repeating itself.
	res := searchFor(t, legacyCapabilityHost(), "agentCapabilities")
	if !strings.Contains(res.Summary, "`agentCapabilities.get` and `agentCapabilities.search` are not offered") {
		t.Errorf("both catalog actions should be named in one sentence, got %q", res.Summary)
	}
	if strings.Count(res.Summary, "slashCommands.list") != 1 {
		t.Errorf("the shared fallback should be named once, got %q", res.Summary)
	}
	// The fallback returns metadata, not usage; the steer says so, because that is
	// where an answer went wrong (invented arguments, an unreadable path reported
	// as deleted).
	for _, want := range []string{"not their usage", "never as missing", "not plugin-bundled"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("steer should carry the fallback's limits (%q), got %q", want, res.Summary)
		}
	}
}

// An unrelated hit on the same word must not hide the steer: the model asked for
// the catalog, and a match on some other action is not an answer to that.
func TestSearchSteersEvenWhenOtherToolsMatch(t *testing.T) {
	mcp := legacyCapabilityHost()
	mcp.toolList = append(mcp.toolList, MCPToolInfo{Name: "agentRegistry.get", Description: "Read the agent registry (the agentCapabilities IPC namespace), not commands or skills."})
	res := searchFor(t, mcp, "agentCapabilities")
	if !strings.HasPrefix(res.Summary, "Found 1 ") {
		t.Fatalf("want the unrelated match kept, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "use `slashCommands.list` instead") {
		t.Errorf("the steer must survive an unrelated match, got %q", res.Summary)
	}
}

// When the host serves the catalog, the search finds it and nothing is steered,
// even though it withholds agentCapabilities.get: that is the app's own shape,
// and slashCommands.list would be a step down from a working search.
func TestSearchDoesNotSteerWhenCatalogIsServed(t *testing.T) {
	mcp := legacyCapabilityHost()
	mcp.toolList = append(mcp.toolList, MCPToolInfo{Name: "agentCapabilities.search", Description: "Find commands, skills and plugins for an agent."})
	res := searchFor(t, mcp, "agentCapabilities search")
	if strings.Contains(res.Summary, "not offered") {
		t.Errorf("a served action must not be reported absent, got %q", res.Summary)
	}
	if !strings.HasPrefix(res.Summary, "Found 1 ") {
		t.Errorf("want the served action found, got %q", res.Summary)
	}
}

// Pointing at a fallback the host lacks too would only move the dead end.
func TestSearchDoesNotSteerToAMissingFallback(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{{Name: "terminal.rename", Description: "Rename the terminal tab."}}}
	if res := searchFor(t, mcp, "agentCapabilities search"); strings.Contains(res.Summary, "slashCommands.list") {
		t.Errorf("must not name an action this host does not serve, got %q", res.Summary)
	}
}

// An ordinary miss stays an ordinary miss. So does a generic verb from the action
// name ("search", "get"), a word from the policy summary, or a fragment of the
// namespace ("capabilities", "abilities"): none of them names the catalog.
func TestSearchDoesNotSteerUnrelatedZeroResults(t *testing.T) {
	for _, q := range []string{"rename title", "search", "get", "agent", "bounded", "instructions", "invocation syntax", "plugin lookup",
		"capabilities", "terminal capabilities", "terminal abilities", "capabilit", "entcap"} {
		if res := searchFor(t, legacyCapabilityHost(), q); strings.Contains(res.Summary, "not offered") {
			t.Errorf("%q is not a lookup for a catalog action and must not be steered, got %q", q, res.Summary)
		}
	}
}

// tool.schema on the absent name has no near miss to offer and used to say
// "search first", which loops back to the same empty search.
func TestSchemaNamesServedFallbackForAbsentCatalogAction(t *testing.T) {
	res := callSchemaTool(t, legacyCapabilityHost(), `{"name":"agentCapabilities.search"}`)
	if res.Ok {
		t.Fatal("an action the host does not serve must not resolve")
	}
	if !strings.Contains(res.Error.Message, "use `slashCommands.list` instead") {
		t.Errorf("want the served stand-in named, got %q", res.Error.Message)
	}
	if strings.Contains(res.Error.Message, "Find the exact name with tool.search first") {
		t.Errorf("must not send the model back to the search that already came up empty, got %q", res.Error.Message)
	}
	// Without the fallback on the host, the ordinary miss message stands.
	bare := &fakeMCP{connected: true, toolList: []MCPToolInfo{{Name: "terminal.rename", Description: "Rename the terminal tab."}}}
	if res := callSchemaTool(t, bare, `{"name":"agentCapabilities.search"}`); !strings.Contains(res.Error.Message, "tool.search") {
		t.Errorf("with no served stand-in the usual guidance applies, got %q", res.Error.Message)
	}
}

// tool.schema for the withheld member of a served catalog gets the ordinary miss,
// not a fallback that cannot supply what .get would.
func TestSchemaDoesNotSteerWhenTheCatalogIsPartlyServed(t *testing.T) {
	mcp := legacyCapabilityHost()
	mcp.toolList = append(mcp.toolList, MCPToolInfo{Name: "agentCapabilities.search", Description: "Find commands, skills and plugins for an agent."})
	res := callSchemaTool(t, mcp, `{"name":"agentCapabilities.get"}`)
	if res.Ok {
		t.Fatal("a withheld action must not resolve")
	}
	if strings.Contains(res.Error.Message, "slashCommands.list") {
		t.Errorf("a partly served catalog must not be steered to the fallback, got %q", res.Error.Message)
	}
}
