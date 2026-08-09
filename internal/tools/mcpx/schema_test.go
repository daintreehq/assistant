package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// catalog is the fixture the schema tests resolve against. copyTree.generate
// carries a genuinely nested schema because it IS the motivating case: the local
// wrapper forwards `options` opaquely ("do not invent keys"), so the underlying
// MCP schema is the only place those keys are written down.
func schemaCatalog() []MCPToolInfo {
	return []MCPToolInfo{
		{
			Name:        "copyTree.generate",
			Description: "Generate a copy tree.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worktreeId": map[string]any{"type": "string"},
					"options": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"scopePaths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"includePaths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required": []any{"scopePaths"},
					},
				},
				"required": []any{},
			},
		},
		{
			Name:        "panel.focus",
			Description: "Focus a panel.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"panelId": map[string]any{"type": "string"}},
				"required":   []any{"panelId"},
			},
		},
		{
			Name:        "terminal.getStatus",
			Description: "Terminal status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// callSchemaTool decodes + handles in one step, mirroring the direct
// constructor/Decode/Handle style the rest of the package's tests use.
func callSchemaTool(t *testing.T, mcp *fakeMCP, args string) tools.ToolResult {
	t.Helper()
	tool := newSchemaTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		t.Fatalf("decode %s: %v", args, err)
	}
	return tool.Handle(context.Background(), decoded, &tools.ToolContext{})
}

// The motivating case (#311): an exactly-named tool resolves in ONE call and
// hands back the server's schema structurally unchanged — including the nested
// `options` keys the wrapper forwards opaquely. Nothing is flattened away.
func TestSchemaReturnsNestedSchemaVerbatim(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	res := callSchemaTool(t, mcp, `{"name":"copyTree.generate"}`)
	if !res.Ok {
		t.Fatalf("lookup failed: %+v", res)
	}
	result := res.Result.(map[string]any)
	if result["name"] != "copyTree.generate" {
		t.Errorf("name = %v, want copyTree.generate", result["name"])
	}
	// No alias hop, so no requestedName/note noise.
	if _, ok := result["requestedName"]; ok {
		t.Error("requestedName should be absent for a direct (non-alias) hit")
	}

	got := result["inputSchema"].(map[string]any)
	want := schemaCatalog()[0].InputSchema
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("schema was not returned verbatim:\n got %s\nwant %s", gotJSON, wantJSON)
	}

	// The nested options keys — the whole point — must survive.
	opts := got["properties"].(map[string]any)["options"].(map[string]any)
	props := opts["properties"].(map[string]any)
	for _, k := range []string{"scopePaths", "includePaths"} {
		if _, ok := props[k]; !ok {
			t.Errorf("nested option key %q lost", k)
		}
	}
	if _, ok := opts["required"]; !ok {
		t.Error("nested required list lost")
	}
}

// The lookup reads the WARM cache and never invokes the target tool: it must
// call ListTools with force=false and CallTool not at all.
func TestSchemaUsesWarmCacheAndNeverCalls(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	if res := callSchemaTool(t, mcp, `{"name":"copyTree.generate"}`); !res.Ok {
		t.Fatalf("lookup failed: %+v", res)
	}
	if mcp.listCount != 1 {
		t.Errorf("ListTools called %d times, want 1", mcp.listCount)
	}
	if mcp.lastListForce {
		t.Error("ListTools called with force=true; the lookup must read the warm cache")
	}
	if mcp.callCount != 0 {
		t.Errorf("CallTool invoked %d times; the lookup must never call the target tool", mcp.callCount)
	}
}

// A renamed wrapper resolves through the explicit alias table, and SAYS SO:
// answering terminal.focus with panel.focus's schema silently would tell the
// model to pass `panelId` to a wrapper whose own parameter is `terminalId`.
func TestSchemaResolvesWrapperAliasAndReportsTheHop(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	res := callSchemaTool(t, mcp, `{"name":"terminal.focus"}`)
	if !res.Ok {
		t.Fatalf("alias lookup failed: %+v", res)
	}
	result := res.Result.(map[string]any)
	if result["name"] != "panel.focus" {
		t.Errorf("name = %v, want panel.focus", result["name"])
	}
	if result["requestedName"] != "terminal.focus" {
		t.Errorf("requestedName = %v, want terminal.focus", result["requestedName"])
	}
	note, _ := result["note"].(string)
	if !strings.Contains(note, "panel.focus") || !strings.Contains(note, "terminal.focus") {
		t.Errorf("note must name both the wrapper and the raw tool, got %q", note)
	}

	// The raw name still resolves directly, with no alias annotation.
	raw := callSchemaTool(t, mcp, `{"name":"panel.focus"}`)
	if !raw.Ok {
		t.Fatalf("raw panel.focus lookup failed: %+v", raw)
	}
	if _, ok := raw.Result.(map[string]any)["requestedName"]; ok {
		t.Error("a direct panel.focus hit should not be annotated as an alias hop")
	}
}

// agentTask.spawnForEdits forwards to agent.launch but TRANSFORMS the contract,
// so it is deliberately not aliased — returning agent.launch's raw schema under
// the local name would describe arguments the wrapper does not accept.
func TestSchemaDoesNotAliasTransformingWrappers(t *testing.T) {
	if target, ok := wrapperMCPAliases["agentTask.spawnForEdits"]; ok {
		t.Errorf("agentTask.spawnForEdits must not be aliased (it transforms the contract), got %q", target)
	}
}

// Matching is exact. A truncation, a case shift, and a namespace fragment must
// all FAIL rather than silently resolve to the wrong tool's contract — but each
// must come back with candidates so the model can self-correct in one round.
func TestSchemaNeverAutoCorrectsButSuggests(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}

	for _, tc := range []struct {
		name, requested, wantCandidate string
	}{
		{"truncated", "copyTree.generat", "copyTree.generate"},
		{"case shift", "copytree.generate", "copyTree.generate"},
		{"bare segment", "generate", "copyTree.generate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"name": tc.requested})
			res := callSchemaTool(t, mcp, string(args))
			if res.Ok {
				t.Fatalf("%q must not auto-resolve", tc.requested)
			}
			if res.Error.Code != codeToolNotFound {
				t.Errorf("code = %q, want %q", res.Error.Code, codeToolNotFound)
			}
			if !res.Error.Recoverable {
				t.Error("a correctable name should be recoverable")
			}
			details := res.Error.Details.(map[string]any)
			candidates := details["candidates"].([]string)
			found := false
			for _, c := range candidates {
				if c == tc.wantCandidate {
					found = true
				}
			}
			if !found {
				t.Errorf("candidates %v should include %q", candidates, tc.wantCandidate)
			}
		})
	}
}

// A miss with nothing plausible nearby must not invent candidates — it points at
// tool.search instead, which is the actual next step.
func TestSchemaUnrelatedMissPointsAtSearch(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	res := callSchemaTool(t, mcp, `{"name":"zzzznope"}`)
	if res.Ok {
		t.Fatal("unrelated name must not resolve")
	}
	details := res.Error.Details.(map[string]any)
	if got := details["candidates"].([]string); len(got) != 0 {
		t.Errorf("unrelated miss should suggest nothing, got %v", got)
	}
	if !strings.Contains(res.Error.Message, "tool.search") {
		t.Errorf("message should point at tool.search, got %q", res.Error.Message)
	}
}

// A wrapper alias is only suggested when the tool it forwards to is live —
// suggesting a name that then fails to resolve costs another wasted round.
func TestSchemaOnlySuggestsAliasesWithLiveTargets(t *testing.T) {
	// panel.focus present ⇒ terminal.focus is offerable.
	withTarget := schemaCandidates(schemaCatalog(), "terminal.foc")
	if !containsString(withTarget, "terminal.focus") {
		t.Errorf("terminal.focus should be suggested when panel.focus is live, got %v", withTarget)
	}
	// panel.focus absent ⇒ terminal.focus must not be suggested.
	without := schemaCandidates([]MCPToolInfo{{Name: "terminal.getStatus"}}, "terminal.foc")
	if containsString(without, "terminal.focus") {
		t.Errorf("terminal.focus must not be suggested when panel.focus is absent, got %v", without)
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// An over-cap schema fails HONESTLY: no partial schema (a clipped JSON Schema is
// invalid, not merely shorter), an explicit code, and the failure itself stays
// well inside the inline cap so it can never become a paged artifact.
func TestSchemaOversizeFailsWithoutPartialSchema(t *testing.T) {
	huge := map[string]any{"type": "object", "properties": map[string]any{}}
	props := huge["properties"].(map[string]any)
	for _, k := range []string{"alpha", "beta", "gamma"} {
		props[k] = map[string]any{"type": "string", "description": strings.Repeat("x", 4000)}
	}
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "huge.tool", Description: "big", InputSchema: huge},
	}}
	res := callSchemaTool(t, mcp, `{"name":"huge.tool"}`)
	if res.Ok {
		t.Fatal("an over-cap schema must fail, not return truncated")
	}
	if res.Error.Code != codeSchemaTooLarge {
		t.Fatalf("code = %q, want %q", res.Error.Code, codeSchemaTooLarge)
	}
	if res.Error.Recoverable {
		t.Error("retrying the same name cannot help; should be unrecoverable")
	}
	if res.Result != nil {
		t.Errorf("no partial schema may ride along, got %v", res.Result)
	}
	// The failure must itself stay inline, or it becomes the artifact-paging
	// problem this tool exists to remove.
	encoded, _ := json.Marshal(res)
	if len(encoded) > domain.MaxToolResultChars {
		t.Errorf("failure envelope is %d chars, must stay under %d", len(encoded), domain.MaxToolResultChars)
	}
	// It still names the top-level keys — an index, not a schema — so the model
	// has a next step rather than being returned to guessing.
	details := res.Error.Details.(map[string]any)
	keys := details["topLevelPropertyNames"].([]string)
	if len(keys) != 3 || keys[0] != "alpha" || keys[2] != "gamma" {
		t.Errorf("top-level keys should be listed sorted, got %v", keys)
	}
	for _, k := range keys {
		if !strings.Contains(res.Error.Message, k) {
			t.Errorf("message should name key %q, got %q", k, res.Error.Message)
		}
	}
}

// The cap is measured on the WHOLE result envelope, not the schema alone: the
// wrapper fields count toward what the turn serializer sees.
func TestSchemaSizeGuardMeasuresWholeEnvelope(t *testing.T) {
	// A schema that alone sits just under the cap, but exceeds it once the
	// alias-hop wrapper fields (requestedName + the explanatory note) are added.
	filler := strings.Repeat("y", domain.MaxToolResultChars-220)
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"panelId": map[string]any{"type": "string", "description": filler}},
	}
	raw, _ := json.Marshal(schema)
	if len(raw) > domain.MaxToolResultChars {
		t.Fatalf("fixture is mis-sized: schema alone is already %d chars", len(raw))
	}
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "panel.focus", Description: "focus", InputSchema: schema},
	}}
	// Requested through the alias, so the extra wrapper fields apply.
	res := callSchemaTool(t, mcp, `{"name":"terminal.focus"}`)
	if res.Ok {
		t.Fatal("envelope exceeds the cap once wrapper fields are added; must fail")
	}
	if res.Error.Code != codeSchemaTooLarge {
		t.Errorf("code = %q, want %q", res.Error.Code, codeSchemaTooLarge)
	}
}

// A schema that cannot round-trip through JSON is a broken catalog entry, not
// something a different argument would fix — it must fail cleanly, not panic.
func TestSchemaUnencodableFailsCleanly(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "bad.tool", InputSchema: map[string]any{"fn": func() {}}},
	}}
	res := callSchemaTool(t, mcp, `{"name":"bad.tool"}`)
	if res.Ok {
		t.Fatal("an unencodable schema must fail")
	}
	if res.Error.Code != codeSchemaInvalid {
		t.Errorf("code = %q, want %q", res.Error.Code, codeSchemaInvalid)
	}
	if res.Error.Recoverable {
		t.Error("a broken catalog entry is not fixable by retrying")
	}
}

// A tool that advertises no schema still resolves normally (the client
// substitutes the empty object) — we never reach for a network fallback, because
// the cache cannot tell "server omitted" from "server said empty".
func TestSchemaEmptyDefaultResolvesWithoutFallback(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	res := callSchemaTool(t, mcp, `{"name":"terminal.getStatus"}`)
	if !res.Ok {
		t.Fatalf("empty-schema tool should still resolve: %+v", res)
	}
	if mcp.callCount != 0 {
		t.Error("must not fall back to a network probe (e.g. actions.getSchema)")
	}
	schema := res.Result.(map[string]any)["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("empty default should be preserved, got %v", schema)
	}
}

// Argument validation: blank/whitespace/over-length names and unknown fields are
// rejected at decode, before any MCP traffic.
func TestSchemaRejectsBadArgs(t *testing.T) {
	tool := newSchemaTool(Deps{MCP: &fakeMCP{connected: true, toolList: schemaCatalog()}})
	for _, args := range []string{
		`{}`,
		`{"name":""}`,
		`{"name":"   "}`,
		`{"name":"` + strings.Repeat("a", maxSchemaNameLen+1) + `"}`,
		`{"name":"copyTree.generate","extra":1}`,
		`{"name":["copyTree.generate"]}`,
	} {
		if _, err := tool.Decode(json.RawMessage(args)); err == nil {
			t.Errorf("decode(%s) should have failed", args)
		}
	}
}

// Connectivity + a stale catalog read are reported the same way the sibling
// discovery tools report them, so the model gets one consistent recovery cue.
func TestSchemaHandlesDisconnectedAndStaleList(t *testing.T) {
	down := callSchemaTool(t, &fakeMCP{connected: false}, `{"name":"copyTree.generate"}`)
	if down.Ok || down.Error.Code != codeMCPUnavailable {
		t.Errorf("disconnected: want %s, got %+v", codeMCPUnavailable, down)
	}
	if !strings.Contains(down.Error.Message, "/reconnect") {
		t.Errorf("disconnected message should mention /reconnect, got %q", down.Error.Message)
	}

	stale := callSchemaTool(t, &fakeMCP{connected: true, listErr: context.DeadlineExceeded}, `{"name":"copyTree.generate"}`)
	if stale.Ok || stale.Error.Code != codeMCPUnavailable {
		t.Errorf("stale list: want %s, got %+v", codeMCPUnavailable, stale)
	}
}

// The tool's own declaration: read-risk, no confirmation, and a description that
// shows the LITERAL argument object. The description is forwarded to the model
// verbatim, so a prose abstraction there is exactly how the original bug
// happened (a dotted key copied out of prose).
func TestSchemaToolDeclaration(t *testing.T) {
	tool := newSchemaTool(Deps{MCP: &fakeMCP{}})
	if tool.Name != "tool.schema" {
		t.Errorf("name = %q", tool.Name)
	}
	if tool.Risk != domain.RiskRead {
		t.Errorf("risk = %v, want read", tool.Risk)
	}
	if tool.Consequence != "" {
		t.Error("a pure cache read needs no confirmation consequence")
	}
	if !strings.Contains(tool.Description, `{"name":"copyTree.generate"}`) {
		t.Error("description must show the literal argument object, not describe it in prose")
	}
	// The declared bounds must be real schema keywords (they are forwarded to the
	// model verbatim), not merely enforced in Go.
	var decl map[string]any
	if err := json.Unmarshal(tool.Schema, &decl); err != nil {
		t.Fatalf("declared schema is not valid JSON: %v", err)
	}
	if decl["additionalProperties"] != false {
		t.Error("declared schema should reject unknown fields")
	}
	name := decl["properties"].(map[string]any)["name"].(map[string]any)
	if name["minLength"] != float64(1) || name["maxLength"] != float64(maxSchemaNameLen) {
		t.Errorf("declared bounds should mirror Validate(), got %v", name)
	}
}

// Findability ships with the capability: both discovery tools must point at
// tool.schema, in their note AND their description, or the model keeps guessing
// arguments simply because it never learns the lookup exists.
func TestDiscoveryToolsPointAtSchemaLookup(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}

	list := newListToolsTool(Deps{MCP: mcp})
	decoded, err := list.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("decode listTools: %v", err)
	}
	listRes := list.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !listRes.Ok {
		t.Fatalf("listTools failed: %+v", listRes)
	}
	if note := listRes.Result.(map[string]any)["note"].(string); !strings.Contains(note, "tool.schema") {
		t.Errorf("daintree.listTools note must point at tool.schema, got %q", note)
	}
	if !strings.Contains(list.Description, "tool.schema") {
		t.Error("daintree.listTools description must point at tool.schema")
	}

	search := newSearchTool(Deps{MCP: mcp})
	// Both the hit and the zero-match path carry the pointer — a model that found
	// nothing is exactly the one about to guess.
	for _, q := range []string{`{"query":"copyTree"}`, `{"query":"nothingmatchesthis"}`} {
		dec, err := search.Decode(json.RawMessage(q))
		if err != nil {
			t.Fatalf("decode search %s: %v", q, err)
		}
		res := search.Handle(context.Background(), dec, &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("search %s failed: %+v", q, res)
		}
		if note := res.Result.(map[string]any)["note"].(string); !strings.Contains(note, "tool.schema") {
			t.Errorf("tool.search note for %s must point at tool.schema, got %q", q, note)
		}
	}
	if !strings.Contains(search.Description, "tool.schema") {
		t.Error("tool.search description must point at tool.schema")
	}
}
