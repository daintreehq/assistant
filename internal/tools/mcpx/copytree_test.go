package mcpx

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// The full curation surface, spelled out once. This list is the LOCAL half of the
// cross-repo drift guard the issue asks for: Daintree's CopyTreeOptionsSchema
// lives in another repo that isn't vendored and isn't available in CI, so nothing
// here can prove the two still agree. What it CAN do is pin our three copies to
// each other and to the Go decoder, so a field added to the struct without a
// schema entry (or vice versa) fails loudly instead of becoming unreachable.
var wantCopyTreeOptionKeys = []string{
	"always",
	"changed",
	"charLimit",
	"exclude",
	"filter",
	"format",
	"includePaths",
	"maxFileCount",
	"maxFileSize",
	"maxTotalSize",
	"modified",
	"scopePaths",
	"sort",
	"withLineNumbers",
}

// schemaObject is the slice of JSON Schema these tests need to reason about.
type schemaObject struct {
	Type                 string                  `json:"type"`
	AdditionalProperties *bool                   `json:"additionalProperties"`
	Properties           map[string]schemaObject `json:"properties"`
	Required             []string                `json:"required"`
	Items                *schemaObject           `json:"items"`
	MinItems             *int                    `json:"minItems"`
	MinLength            *int                    `json:"minLength"`
	Minimum              *int                    `json:"minimum"`
	Enum                 []string                `json:"enum"`
}

func parseSchema(t *testing.T, raw json.RawMessage) schemaObject {
	t.Helper()
	var s schemaObject
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return s
}

// copyTreeCases is every wrapper carrying the shared options object, paired with
// the minimal args that decode successfully for it (generateAndCopyFile requires
// an explicit target, the others don't). Held as constructors so each subtest can
// wire its own fakeMCP.
var copyTreeCases = []struct {
	name     string
	build    func(Deps) tools.Tool
	baseArgs map[string]any
}{
	{"copyTree.generate", newCopyTreeGenerateTool, map[string]any{}},
	{"copyTree.generateAndCopyFile", newCopyTreeGenerateAndCopyFileTool, map[string]any{"worktreeId": "wt-1"}},
	{"copyTree.injectToTerminal", newCopyTreeInjectTool, map[string]any{"terminalId": "t1"}},
}

// argsWithOptions builds a decodable args payload for one wrapper with the given
// options object attached.
func argsWithOptions(t *testing.T, base map[string]any, options any) json.RawMessage {
	t.Helper()
	payload := map[string]any{}
	for k, v := range base {
		payload[k] = v
	}
	if options != nil {
		payload["options"] = options
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

// The advertised options schema, the Go decoder, and the host's field list must
// name exactly the same keys on every wrapper. An untyped field is an
// UNREACHABLE field once additionalProperties is false, so a mismatch here is
// the model silently losing a curation lever — the exact failure #303 exists to
// fix.
func TestCopyTreeOptionsSchemaMatchesStruct(t *testing.T) {
	var structKeys []string
	rt := reflect.TypeOf(copyTreeOptions{})
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			t.Fatalf("copyTreeOptions.%s has no json tag", rt.Field(i).Name)
		}
		structKeys = append(structKeys, name)
	}
	sort.Strings(structKeys)
	if !reflect.DeepEqual(structKeys, wantCopyTreeOptionKeys) {
		t.Errorf("copyTreeOptions fields drifted from the host mirror:\n got %v\nwant %v", structKeys, wantCopyTreeOptionKeys)
	}

	for _, tc := range copyTreeCases {
		t.Run(tc.name, func(t *testing.T) {
			schema := tc.build(Deps{}).Schema
			// Tool schemas are forwarded verbatim to the upstream model, where
			// combinators are honoured inconsistently at best. Keep them out.
			for _, banned := range []string{"oneOf", "anyOf", "allOf", "$ref"} {
				if strings.Contains(string(schema), banned) {
					t.Errorf("schema uses %s; this repo encodes bounds as plain keywords instead", banned)
				}
			}

			outer := parseSchema(t, schema)
			if outer.AdditionalProperties == nil || *outer.AdditionalProperties {
				t.Errorf("outer schema must set additionalProperties:false")
			}
			opts, ok := outer.Properties["options"]
			if !ok {
				t.Fatalf("no options property")
			}
			if opts.AdditionalProperties == nil || *opts.AdditionalProperties {
				t.Errorf("options must set additionalProperties:false, else an unknown key decodes to a silent no-op")
			}

			var schemaKeys []string
			for k := range opts.Properties {
				schemaKeys = append(schemaKeys, k)
			}
			sort.Strings(schemaKeys)
			if !reflect.DeepEqual(schemaKeys, wantCopyTreeOptionKeys) {
				t.Errorf("options schema keys drifted:\n got %v\nwant %v", schemaKeys, wantCopyTreeOptionKeys)
			}

			// The three selection lists carry the host's .min(1) at both levels,
			// and the budgets its .positive(). These are real schema keywords so
			// the model sees the bound, not just the validator's rejection.
			for _, sel := range []string{"filter", "includePaths", "scopePaths"} {
				p := opts.Properties[sel]
				if p.Type != "array" {
					t.Errorf("%s must be an array (we do not model the host's string|string[] union)", sel)
				}
				if p.MinItems == nil || *p.MinItems != 1 {
					t.Errorf("%s must declare minItems:1", sel)
				}
				if p.Items == nil || p.Items.MinLength == nil || *p.Items.MinLength != 1 {
					t.Errorf("%s items must declare minLength:1", sel)
				}
			}
			for _, budget := range []string{"maxFileSize", "maxTotalSize", "maxFileCount", "charLimit"} {
				p := opts.Properties[budget]
				if p.Minimum == nil || *p.Minimum != 1 {
					t.Errorf("%s must declare minimum:1", budget)
				}
			}
			if got := opts.Properties["format"].Enum; !reflect.DeepEqual(got, copyTreeFormats) {
				t.Errorf("format enum drifted: got %v want %v", got, copyTreeFormats)
			}
			if got := opts.Properties["sort"].Enum; !reflect.DeepEqual(got, copyTreeSorts) {
				t.Errorf("sort enum drifted: got %v want %v", got, copyTreeSorts)
			}
		})
	}
}

// Every typed field must survive decode → canonical re-marshal → forwardMap and
// land on the wire under the host's own key. The re-marshal step is the risky
// one: it is where `omitempty` silently eats values.
func TestCopyTreeWrappersForwardTypedOptions(t *testing.T) {
	full := map[string]any{
		"format":          "markdown",
		"filter":          []string{"**/*.go"},
		"exclude":         []string{"vendor/**"},
		"always":          []string{"README.md"},
		"includePaths":    []string{"internal/a.go", "internal/a_test.go"},
		"scopePaths":      []string{"internal"},
		"modified":        true,
		"changed":         "main",
		"maxFileSize":     1000,
		"maxTotalSize":    2000,
		"maxFileCount":    30,
		"withLineNumbers": true,
		"charLimit":       5000,
		"sort":            "size",
	}

	for _, tc := range copyTreeCases {
		t.Run(tc.name, func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := tc.build(Deps{MCP: mcp})

			decoded, err := tool.Decode(argsWithOptions(t, tc.baseArgs, full))
			if err != nil {
				t.Fatalf("full typed options must decode: %v", err)
			}
			res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
			if !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			got, _ := mcp.lastArgs["options"].(map[string]any)
			if got == nil {
				t.Fatalf("options did not reach MCP: %v", mcp.lastArgs)
			}
			var gotKeys []string
			for k := range got {
				gotKeys = append(gotKeys, k)
			}
			sort.Strings(gotKeys)
			if !reflect.DeepEqual(gotKeys, wantCopyTreeOptionKeys) {
				t.Errorf("a typed field was dropped on the way to Daintree:\n got %v\nwant %v", gotKeys, wantCopyTreeOptionKeys)
			}
			if got["maxFileCount"] != float64(30) || got["sort"] != "size" || got["modified"] != true {
				t.Errorf("values mangled in transit: %v", got)
			}
		})
	}
}

// The load-bearing guard. Daintree reads an empty selection as "no filter" — i.e.
// the whole worktree — so an empty-but-present list must never reach it. It has
// to be caught in Validate rather than by the schema or the host, because
// StrictDecoder's canonical re-marshal runs AFTER Validate and `omitempty`
// erases the empty slice before anyone downstream could see it.
func TestCopyTreeRejectsUnsafeEmptySelections(t *testing.T) {
	unsafe := []struct {
		name    string
		options map[string]any
	}{
		{"empty includePaths", map[string]any{"includePaths": []string{}}},
		{"empty scopePaths", map[string]any{"scopePaths": []string{}}},
		{"empty filter", map[string]any{"filter": []string{}}},
		{"blank includePaths entry", map[string]any{"includePaths": []string{"a.go", "   "}}},
		{"blank scopePaths entry", map[string]any{"scopePaths": []string{""}}},
		{"blank filter entry", map[string]any{"filter": []string{" "}}},
	}

	for _, tc := range copyTreeCases {
		for _, u := range unsafe {
			t.Run(tc.name+"/"+u.name, func(t *testing.T) {
				mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
				tool := tc.build(Deps{MCP: mcp})
				if _, err := tool.Decode(argsWithOptions(t, tc.baseArgs, u.options)); err == nil {
					t.Fatalf("%s must be rejected — it would silently bundle the whole worktree", u.name)
				}
				if mcp.lastName != "" {
					t.Errorf("rejected args must not reach MCP, called %q", mcp.lastName)
				}
			})
		}
	}

	// A one-element list is the normal curated case and must survive intact —
	// the guard above must not overreach into rejecting valid narrow selections.
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCopyTreeGenerateTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(argsWithOptions(t, nil, map[string]any{"includePaths": []string{"only.go"}}))
	if err != nil {
		t.Fatalf("single-entry includePaths must decode: %v", err)
	}
	if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("expected ok: %+v", res.Error)
	}
	opts, _ := mcp.lastArgs["options"].(map[string]any)
	if paths, _ := opts["includePaths"].([]any); len(paths) != 1 || paths[0] != "only.go" {
		t.Errorf("valid single-entry selection was mangled: %v", opts)
	}
}

// Bounds the schema advertises but strict decoding alone can't enforce, plus the
// closed-object rejection that makes a mistyped key visible instead of silent.
func TestCopyTreeRejectsInvalidTypedValues(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"unknown nested option key", `{"options":{"depth":2}}`},
		{"scalar filter (host union arm we do not accept)", `{"options":{"filter":"**/*.go"}}`},
		{"scalar exclude", `{"options":{"exclude":"vendor/**"}}`},
		{"off-menu format", `{"options":{"format":"yaml"}}`},
		{"off-menu sort", `{"options":{"sort":"random"}}`},
		{"zero maxFileCount", `{"options":{"maxFileCount":0}}`},
		{"negative charLimit", `{"options":{"charLimit":-1}}`},
		{"fractional maxTotalSize", `{"options":{"maxTotalSize":1.5}}`},
		{"wrong type for modified", `{"options":{"modified":"yes"}}`},
		{"unknown top-level key", `{"nope":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := newCopyTreeGenerateTool(Deps{MCP: mcp})
			if _, err := tool.Decode(json.RawMessage(c.args)); err == nil {
				t.Errorf("%s must be rejected at decode", c.name)
			}
			if mcp.lastName != "" {
				t.Errorf("rejected args must not reach MCP, called %q", mcp.lastName)
			}
		})
	}

	// A zero budget is the dangerous one worth stating twice: `omitempty` would
	// erase it, and Daintree reads an absent budget as "no budget", so `0` must
	// fail rather than quietly become an unbounded bundle.
	zero := 0
	if err := (&copyTreeOptions{MaxFileCount: &zero}).validate(); err == nil {
		t.Errorf("maxFileCount:0 must be rejected, not dropped into an unbounded bundle")
	}
}

// Mirrors Daintree's requireExplicitWorktreeForAgentDispatch (#11722): our calls
// arrive there as dispatchSource "agent", which has no active-worktree fallback.
// Rejecting at decode keeps the user from confirming a system-tier clipboard
// overwrite that was always going to fail host-side.
func TestCopyTreeGenerateAndCopyFileRequiresExplicitTarget(t *testing.T) {
	for _, bad := range []string{`{}`, `{"worktreeId":"  "}`, `{"worktreePath":""}`, `{"options":{"includePaths":["a.go"]}}`} {
		mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
		tool := newCopyTreeGenerateAndCopyFileTool(Deps{MCP: mcp})
		decoded, err := tool.Decode(json.RawMessage(bad))
		if err == nil {
			t.Errorf("%s must be rejected: the clipboard copy has no active-worktree fallback", bad)
		}
		// Defense in depth: the handler refuses too, for any path skipping Decode.
		if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); res.Ok || res.Error.Code != domain.CodeValidation {
			t.Errorf("handler must also refuse %s: %+v", bad, res)
		}
		if mcp.lastName != "" {
			t.Errorf("targetless copy must not reach MCP, called %q", mcp.lastName)
		}
	}

	// Either selector satisfies it, and both reach Daintree — the host applies
	// its own id-wins precedence, so we forward what we were given.
	for _, good := range []struct{ key, val string }{{"worktreeId", "wt-1"}, {"worktreePath", "/tmp/wt"}} {
		mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
		tool := newCopyTreeGenerateAndCopyFileTool(Deps{MCP: mcp})
		decoded, err := tool.Decode(json.RawMessage(`{"` + good.key + `":"` + good.val + `"}`))
		if err != nil {
			t.Fatalf("%s must be accepted: %v", good.key, err)
		}
		if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
			t.Fatalf("expected ok: %+v", res.Error)
		}
		if mcp.lastArgs[good.key] != good.val {
			t.Errorf("%s not forwarded: %v", good.key, mcp.lastArgs)
		}
	}

	// The clipboard overwrite stays system tier with a stated consequence.
	tool := newCopyTreeGenerateAndCopyFileTool(Deps{})
	if tool.Risk != domain.RiskSystem {
		t.Errorf("generateAndCopyFile must stay RiskSystem, got %v", tool.Risk)
	}
	if !strings.Contains(tool.Consequence, "clipboard") {
		t.Errorf("consequence must name the clipboard: %q", tool.Consequence)
	}
}

// copyTree.generate is the endpoint a curation loop ends on, so its description
// has to be honest about what comes back: a file PATH, not the bundle text. The
// old wording claimed it "returned it as text", which is what makes the model
// expect inline content that never arrives.
func TestCopyTreeGenerateForwardsTargetAndIncludeContent(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCopyTreeGenerateTool(Deps{MCP: mcp})

	decoded, err := tool.Decode(json.RawMessage(`{"worktreePath":"/tmp/wt","includeContent":true}`))
	if err != nil {
		t.Fatalf("worktreePath + includeContent must decode: %v", err)
	}
	if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("expected ok: %+v", res.Error)
	}
	if mcp.lastArgs["worktreePath"] != "/tmp/wt" {
		t.Errorf("worktreePath not forwarded: %v", mcp.lastArgs)
	}
	if mcp.lastArgs["includeContent"] != true {
		t.Errorf("includeContent not forwarded: %v", mcp.lastArgs)
	}

	// An omitted target keeps Daintree's active-worktree fallback: unlike the
	// clipboard copy, generate was deliberately left without the explicit-target
	// guard host-side, so we must not invent one.
	mcp2 := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool2 := newCopyTreeGenerateTool(Deps{MCP: mcp2})
	d2, err := tool2.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("an omitted target must stay legal for generate: %v", err)
	}
	if res := tool2.Handle(context.Background(), d2, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("expected ok: %+v", res.Error)
	}
	if _, present := mcp2.lastArgs["worktreeId"]; present {
		t.Errorf("an omitted worktreeId must stay absent on the wire: %v", mcp2.lastArgs)
	}

	if tool.Risk != domain.RiskRead {
		t.Errorf("generate must stay RiskRead so a curation loop can end on it, got %v", tool.Risk)
	}
	if strings.Contains(tool.Description, "return it as text") {
		t.Errorf("stale description: generate returns a file path, not the bundle text")
	}
	for _, want := range []string{"includePaths", "PATH"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description must mention %q so the model knows how to curate and what comes back: %q", want, tool.Description)
		}
	}
}

// Each wrapper has to teach the curation field, or the typed schema is reachable
// in principle and unused in practice.
func TestCopyTreeDescriptionsTeachCuration(t *testing.T) {
	for _, tc := range copyTreeCases {
		t.Run(tc.name, func(t *testing.T) {
			desc := tc.build(Deps{}).Description
			if !strings.Contains(desc, "options.includePaths") {
				t.Errorf("description must point at options.includePaths: %q", desc)
			}
			if strings.Contains(desc, "forwarded verbatim") || strings.Contains(desc, "don't invent keys") {
				t.Errorf("stale opaque-bag wording survived: %q", desc)
			}
		})
	}
}
