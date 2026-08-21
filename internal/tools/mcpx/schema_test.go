package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// catalog is the fixture the schema tests resolve against. copyTree.generate
// carries a genuinely nested schema because it is the case that surfaced #311:
// its `options` bag was forwarded opaquely ("do not invent keys"), so the raw
// MCP schema was the only place those keys were written down. That particular
// bag has since been typed locally, but the shape of the test still matters —
// every wrapper forwarding an `arguments` record is in the same position.
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
	// Lookups are raw-name-only, so a result never carries a redirected name.
	if _, ok := result["requestedName"]; ok {
		t.Error("results should not carry a requestedName — lookups are raw MCP names only")
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

// Lookups are RAW MCP names only. A local wrapper name must NOT resolve to the
// raw tool it forwards to: terminal.focus takes terminalId where panel.focus
// takes panelId, so returning panel.focus's structured schema under the local
// name would have the model build a call its own decoder rejects.
func TestSchemaDoesNotResolveLocalWrapperNames(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: schemaCatalog()}
	res := callSchemaTool(t, mcp, `{"name":"terminal.focus"}`)
	if res.Ok {
		t.Fatalf("terminal.focus is a local wrapper name and must not resolve, got %+v", res.Result)
	}
	if res.Error.Code != codeToolNotFound {
		t.Errorf("code = %q, want %q", res.Error.Code, codeToolNotFound)
	}
	// The raw name it forwards to still resolves directly.
	if raw := callSchemaTool(t, mcp, `{"name":"panel.focus"}`); !raw.Ok {
		t.Fatalf("raw panel.focus lookup should succeed: %+v", raw)
	}
}

// When a same-named typed wrapper governs the call, the result must SAY the raw
// schema is not the wrapper's call shape. Several wrappers materially transform
// the contract (terminal.rename makes optional args required; terminal.close
// adds a plural batch form; recipe.run nests fields under `arguments`), so an
// unannotated raw schema is a new version of the bug this tool fixes.
func TestSchemaAnnotatesWrappedTools(t *testing.T) {
	catalog := append(schemaCatalog(), MCPToolInfo{
		Name:        "terminal.close",
		Description: "Close a terminal.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"terminalId": map[string]any{"type": "string"}},
			"required":   []any{"terminalId"},
		},
	})
	mcp := &fakeMCP{connected: true, toolList: catalog}

	res := callSchemaTool(t, mcp, `{"name":"terminal.close"}`)
	if !res.Ok {
		t.Fatalf("terminal.close lookup failed: %+v", res)
	}
	result := res.Result.(map[string]any)
	note, _ := result["note"].(string)
	if note == "" {
		t.Fatal("a wrapped tool's schema must carry a wrapper caveat")
	}
	// The caveat must point at the wrapper, not merely mention that one exists.
	if wrapper, _ := result["localWrapper"].(string); wrapper != "terminal.close" {
		t.Errorf("localWrapper should name the typed wrapper, got %q", wrapper)
	}
	if !strings.Contains(note, "arguments") {
		t.Errorf("note should warn that wrapper parameters differ (incl. nesting), got %q", note)
	}
	// The annotation set is derived from the family's own registration, so every
	// name it registers is covered — including copyTree.generate, which has a
	// wrapper but (deliberately) no daintree.call denylist entry.
	if !getLocalWrapperNames()["copyTree.generate"] || !getLocalWrapperNames()["terminal.close"] {
		t.Error("the wrapper set should be derived from the registered family")
	}
	if getLocalWrapperNames()["terminal.getStatus"] {
		t.Error("terminal.getStatus has no local wrapper and must not be in the set")
	}

	// copyTree.generate is wrapped too — the motivating case — so it is annotated
	// while still returning the raw schema that makes the lookup worth doing.
	gen := callSchemaTool(t, mcp, `{"name":"copyTree.generate"}`)
	if !gen.Ok {
		t.Fatalf("copyTree.generate lookup failed: %+v", gen)
	}
	genResult := gen.Result.(map[string]any)
	if _, ok := genResult["note"]; !ok {
		t.Error("copyTree.generate is a wrapped tool and should be annotated")
	}
	opts := genResult["inputSchema"].(map[string]any)["properties"].(map[string]any)["options"].(map[string]any)
	if _, ok := opts["properties"]; !ok {
		t.Error("the raw options schema must still be returned — that is the point of the lookup")
	}

	// An UNwrapped tool gets no caveat, so the annotation stays a real signal.
	plain := callSchemaTool(t, mcp, `{"name":"terminal.getStatus"}`)
	if !plain.Ok {
		t.Fatalf("terminal.getStatus lookup failed: %+v", plain)
	}
	if _, ok := plain.Result.(map[string]any)["note"]; ok {
		t.Error("an unwrapped tool should carry no wrapper caveat")
	}
}

// A catalog advertising one name twice with DIFFERENT schemas is ambiguous —
// picking either would be the confident contract guess exact matching exists to
// prevent. Identical duplicates are harmless and must still resolve.
func TestSchemaRejectsAmbiguousDuplicates(t *testing.T) {
	conflicting := []MCPToolInfo{
		{Name: "dup.tool", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}},
		{Name: "dup.tool", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "number"}}}},
	}
	res := callSchemaTool(t, &fakeMCP{connected: true, toolList: conflicting}, `{"name":"dup.tool"}`)
	if res.Ok {
		t.Fatal("conflicting duplicate schemas must not silently resolve to one")
	}
	if res.Error.Code != codeSchemaInvalid {
		t.Errorf("code = %q, want %q", res.Error.Code, codeSchemaInvalid)
	}

	same := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	agreeing := []MCPToolInfo{{Name: "dup.tool", InputSchema: same}, {Name: "dup.tool", InputSchema: same}}
	if ok := callSchemaTool(t, &fakeMCP{connected: true, toolList: agreeing}, `{"name":"dup.tool"}`); !ok.Ok {
		t.Errorf("identical duplicates should resolve normally: %+v", ok)
	}

	// A duplicated name must also not be suggested twice on a miss.
	cands := schemaCandidates(agreeing, "dup.too")
	if len(cands) != 1 {
		t.Errorf("duplicate names should be deduplicated in candidates, got %v", cands)
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

// Candidates come from the live catalog only — a local tool name that is not an
// MCP tool must never be suggested, because retrying with it would just miss
// again.
func TestSchemaCandidatesAreCatalogNamesOnly(t *testing.T) {
	got := schemaCandidates(schemaCatalog(), "terminal.foc")
	if containsString(got, "terminal.focus") {
		t.Errorf("terminal.focus is a local tool, not an MCP tool; must not be suggested, got %v", got)
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
	// domain.Fail always leaves Result nil, so asserting that proves nothing. The
	// real risk is a schema FRAGMENT leaking through the message or details, so
	// assert against the fully serialized failure: none of the filler may appear,
	// and the details must carry only the documented keys.
	serialized := agent.SerializeToolResult(res, nil)
	if strings.Contains(serialized, strings.Repeat("x", 50)) {
		t.Error("a fragment of the oversized schema leaked into the failure")
	}
	assertNotStubbed(t, serialized)
	details := res.Error.Details.(map[string]any)
	allowed := map[string]bool{
		"name": true, "resultChars": true, "maxToolResultChars": true,
		"topLevelPropertyNames": true, "omittedPropertyNames": true,
	}
	for k := range details {
		if !allowed[k] {
			t.Errorf("unexpected detail key %q — schema content may be leaking", k)
		}
	}
	// It still names the top-level keys — an index, not a schema — so the model
	// has a next step rather than being returned to guessing.
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

// assertNotStubbed fails the test if the serializer converted a result into an
// overflow stub. Checking for "artifactId" alone is NOT sufficient: with a nil
// artifact store the stub carries `"truncated":true` and no artifactId at all,
// so an artifactId-only check passes exactly when the result WAS stubbed.
func assertNotStubbed(t *testing.T, serialized string) {
	t.Helper()
	if !json.Valid([]byte(serialized)) {
		t.Fatalf("serialized result is not valid JSON: %s", serialized)
	}
	var probe struct {
		Result struct {
			Truncated  bool   `json:"truncated"`
			ArtifactID string `json:"artifactId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(serialized), &probe); err != nil {
		return // not an object with a result field; nothing to assert
	}
	if probe.Result.Truncated || probe.Result.ArtifactID != "" {
		t.Errorf("result was stubbed by the serializer (truncated=%v artifactId=%q)",
			probe.Result.Truncated, probe.Result.ArtifactID)
	}
}

// THE load-bearing size test: a result this tool calls "fine" must survive the
// REAL serializer without becoming a paged artifact stub. Measuring only the
// inner result map under-counts by the {ok,summary,result} wrapper, so a schema
// landing in that gap would pass the local guard and then be turned into exactly
// the paged artifact this feature exists to eliminate. This drives a schema up to
// the boundary from below and asserts the two agree at every step.
func TestSchemaGuardAgreesWithRealSerializer(t *testing.T) {
	// Walk sizes across the cap boundary, INCLUDING clearly-small ones: a guard
	// that rejected everything would satisfy a one-sided "never stubbed" check,
	// so each case asserts the accept/reject decision independently.
	var sawAccept, sawReject bool
	for _, fill := range []int{
		100,
		domain.MaxToolResultChars / 2,
		domain.MaxToolResultChars - 400,
		domain.MaxToolResultChars - 200,
		domain.MaxToolResultChars - 120,
		domain.MaxToolResultChars - 60,
		domain.MaxToolResultChars - 10,
		domain.MaxToolResultChars + 500,
	} {
		schema := map[string]any{
			"type":       "object",
			"properties": map[string]any{"panelId": map[string]any{"type": "string", "description": strings.Repeat("y", fill)}},
		}
		mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
			{Name: "panel.focus", Description: "focus", InputSchema: schema},
		}}
		res := callSchemaTool(t, mcp, `{"name":"panel.focus"}`)
		serialized := agent.SerializeToolResult(res, nil)

		// Independently compute what the serializer will do, and require the tool
		// to have made the matching call.
		wantOk := len(serialized) <= domain.MaxToolResultChars && res.Ok
		if res.Ok != wantOk {
			t.Errorf("fill=%d: accepted=%v but serialized length is %d (cap %d)",
				fill, res.Ok, len(serialized), domain.MaxToolResultChars)
		}
		// Whether accepted or rejected, what reaches the model must never be a
		// paged stub — that is the outcome this whole feature exists to remove.
		assertNotStubbed(t, serialized)

		if res.Ok {
			sawAccept = true
			// An accepted result must actually carry the schema.
			if _, ok := res.Result.(map[string]any)["inputSchema"]; !ok {
				t.Errorf("fill=%d: accepted result is missing inputSchema", fill)
			}
		} else {
			sawReject = true
			if res.Error.Code != codeSchemaTooLarge {
				t.Errorf("fill=%d: unexpected failure %q", fill, res.Error.Code)
			}
		}
	}
	// Both branches must have been exercised, or the sweep proved nothing.
	if !sawAccept || !sawReject {
		t.Errorf("sweep did not cross the boundary (accepted=%v rejected=%v)", sawAccept, sawReject)
	}
}

// The cap counts CHARACTERS, not bytes (matching the serializer's charLen), so a
// multibyte schema whose byte length exceeds the cap but whose rune count does
// not must still be returned inline.
func TestSchemaCapCountsRunesNotBytes(t *testing.T) {
	// 3 bytes per rune: comfortably over the cap in bytes, well under in runes.
	desc := strings.Repeat("あ", domain.MaxToolResultChars/2)
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"k": map[string]any{"type": "string", "description": desc}},
	}
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{{Name: "wide.rune", InputSchema: schema}}}
	res := callSchemaTool(t, mcp, `{"name":"wide.rune"}`)
	serialized := agent.SerializeToolResult(res, nil)
	if len(serialized) <= domain.MaxToolResultChars {
		t.Fatalf("fixture is mis-sized: %d bytes should exceed the cap", len(serialized))
	}
	if !res.Ok {
		t.Error("a schema under the cap in RUNES must be returned inline despite its byte length")
	}
	assertNotStubbed(t, serialized)
}

// The envelope we measure must keep the field names the serializer emits — the
// shape is duplicated (mcpx cannot import internal/agent), so drift is only
// caught by pinning it.
func TestSerializedEnvelopeMatchesSerializerShape(t *testing.T) {
	mine, err := json.Marshal(serializedEnvelope{Ok: true, Summary: "s", Result: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	theirs := agent.SerializeToolResult(domain.Ok("s", map[string]any{"k": "v"}), nil)
	if string(mine) != theirs {
		t.Errorf("envelope shape drifted from the serializer:\n mine %s\ntheirs %s", mine, theirs)
	}
}

// A schema with a huge number of property names must not turn the "too large"
// report into something itself too large.
func TestSchemaOversizeReportIsBounded(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 400; i++ {
		props[strings.Repeat("k", 30)+itoaTest(i)] = map[string]any{"type": "string", "description": strings.Repeat("z", 60)}
	}
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "wide.tool", InputSchema: map[string]any{"type": "object", "properties": props}},
	}}
	res := callSchemaTool(t, mcp, `{"name":"wide.tool"}`)
	if res.Ok {
		t.Fatal("a schema this wide must exceed the cap")
	}
	assertNotStubbed(t, agent.SerializeToolResult(res, nil))
	details := res.Error.Details.(map[string]any)
	if omitted := details["omittedPropertyNames"].(int); omitted <= 0 {
		t.Errorf("with 400 keys the report should omit some, got %d", omitted)
	}
}

// Key names that EXPAND under JSON escaping must not blow the report past the
// cap. A budget computed from raw byte lengths misses this: quotes, backslashes
// and control characters can multiply a name's encoded size several-fold, and
// the names are emitted three times (summary, message, details).
func TestSchemaOversizeReportSurvivesEscaping(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 120; i++ {
		// Every character escapes to at least two, several to six.
		nasty := strings.Repeat(`"\`, 15) + strings.Repeat("\x01", 15) + itoaTest(i)
		props[nasty] = map[string]any{"type": "string", "description": strings.Repeat("z", 200)}
	}
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "escaping.tool", InputSchema: map[string]any{"type": "object", "properties": props}},
	}}
	res := callSchemaTool(t, mcp, `{"name":"escaping.tool"}`)
	if res.Ok {
		t.Fatal("this schema must exceed the cap")
	}
	assertNotStubbed(t, agent.SerializeToolResult(res, nil))
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
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
		// Padding is REJECTED, not trimmed: the tool promises exact matching, and
		// silently trimming would make the audit record disagree with the name
		// actually looked up.
		`{"name":" copyTree.generate"}`,
		`{"name":"copyTree.generate "}`,
	} {
		if _, err := tool.Decode(json.RawMessage(args)); err == nil {
			t.Errorf("decode(%s) should have failed", args)
		}
	}
	// The exact name still decodes.
	if _, err := tool.Decode(json.RawMessage(`{"name":"copyTree.generate"}`)); err != nil {
		t.Errorf("the exact name should decode: %v", err)
	}
}

// Near-miss suggestions must stay signal. A stubby catalog name must not be
// offered for every miss just because it appears somewhere in the request.
func TestSchemaCandidatesAvoidNoise(t *testing.T) {
	catalog := []MCPToolInfo{{Name: "a"}, {Name: "go"}, {Name: "copyTree.generate"}}
	got := schemaCandidates(catalog, "terminal.getStatus")
	for _, noise := range []string{"a", "go"} {
		if containsString(got, noise) {
			t.Errorf("short name %q should not be suggested for an unrelated request, got %v", noise, got)
		}
	}
	// A real truncation still resolves to a suggestion.
	if got := schemaCandidates(catalog, "copyTree.gen"); !containsString(got, "copyTree.generate") {
		t.Errorf("a truncated name should still suggest the full one, got %v", got)
	}
	// The list is capped.
	wide := make([]MCPToolInfo, 0, 20)
	for i := 0; i < 20; i++ {
		wide = append(wide, MCPToolInfo{Name: "terminal.thing" + itoaTest(i)})
	}
	if got := schemaCandidates(wide, "terminal.thing"); len(got) > maxSchemaCandidates {
		t.Errorf("candidates should cap at %d, got %d", maxSchemaCandidates, len(got))
	}
}

// An empty catalog must not send the model to tool.search, which reads the same
// empty catalog and can only return nothing.
func TestSchemaEmptyCatalogMessage(t *testing.T) {
	res := callSchemaTool(t, &fakeMCP{connected: true}, `{"name":"copyTree.generate"}`)
	if res.Ok {
		t.Fatal("an empty catalog cannot resolve anything")
	}
	if strings.Contains(res.Error.Message, "tool.search") {
		t.Errorf("an empty catalog should not point at tool.search, got %q", res.Error.Message)
	}
	if !strings.Contains(res.Error.Message, "empty") {
		t.Errorf("message should say the catalog is empty, got %q", res.Error.Message)
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
	if !strings.Contains(tool.Description, `{"name":"recipe.run"}`) {
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
