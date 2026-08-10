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
	Description          string                  `json:"description"`
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

			// Advertised TYPE per field. Without this the key-set check above
			// would stay green while `modified` was advertised as a string or
			// `exclude` as an array of integers — the model would then emit
			// something the strict decoder rejects, with nothing to explain why.
			wantType := map[string]string{
				"always": "array", "changed": "string", "charLimit": "integer",
				"exclude": "array", "filter": "array", "format": "string",
				"includePaths": "array", "maxFileCount": "integer", "maxFileSize": "integer",
				"maxTotalSize": "integer", "modified": "boolean", "scopePaths": "array",
				"sort": "string", "withLineNumbers": "boolean",
			}
			for field, want := range wantType {
				p := opts.Properties[field]
				if p.Type != want {
					t.Errorf("%s advertised as %q, want %q", field, p.Type, want)
				}
				if want == "array" && (p.Items == nil || p.Items.Type != "string") {
					t.Errorf("%s must declare string items", field)
				}
			}

			// Every field carries model-facing prose — these descriptions ARE the
			// feature, so an empty one is a silent regression.
			for field, p := range opts.Properties {
				if strings.TrimSpace(p.Description) == "" {
					t.Errorf("options.%s has no description; the model has nothing to go on", field)
				}
			}
			if strings.TrimSpace(opts.Description) == "" {
				t.Errorf("the options object itself needs a description")
			}
		})
	}

	// Required sets and risk classes, pinned per tool.
	if got := parseSchema(t, newCopyTreeInjectTool(Deps{}).Schema).Required; !reflect.DeepEqual(got, []string{"terminalId"}) {
		t.Errorf("inject must require terminalId, got %v", got)
	}
	for _, tool := range []string{"copyTree.generate", "copyTree.generateAndCopyFile"} {
		found := false
		for _, tc := range copyTreeCases {
			if tc.name != tool {
				continue
			}
			found = true
			// Neither worktree selector is `required`: "at least one of two" needs
			// a combinator, so Validate carries that constraint instead.
			if got := parseSchema(t, tc.build(Deps{}).Schema).Required; len(got) != 0 {
				t.Errorf("%s should require no field in the schema, got %v", tool, got)
			}
		}
		if !found {
			t.Fatalf("%s is missing from copyTreeCases; this assertion was silently doing nothing", tool)
		}
	}
	if got := newCopyTreeInjectTool(Deps{}).Risk; got != domain.RiskTerminal {
		t.Errorf("inject must stay RiskTerminal, got %v", got)
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

			// The label rides along with a maximal options object so a regression
			// can't make name and options quietly displace each other.
			base := map[string]any{"name": "auth flow context"}
			for k, v := range tc.baseArgs {
				base[k] = v
			}
			decoded, err := tool.Decode(argsWithOptions(t, base, full))
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
			if mcp.lastArgs["name"] != "auth flow context" {
				t.Errorf("top-level name must survive alongside a full options object, got %v", mcp.lastArgs["name"])
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
		wantErr string
	}{
		{"empty includePaths", map[string]any{"includePaths": []string{}}, "options.includePaths was supplied as an empty list"},
		{"empty scopePaths", map[string]any{"scopePaths": []string{}}, "options.scopePaths was supplied as an empty list"},
		{"empty filter", map[string]any{"filter": []string{}}, "options.filter was supplied as an empty list"},
		{"blank includePaths entry", map[string]any{"includePaths": []string{"a.go", "   "}}, "is blank"},
		{"blank scopePaths entry", map[string]any{"scopePaths": []string{""}}, "is blank"},
		{"blank filter entry", map[string]any{"filter": []string{" "}}, "is blank"},

		// Explicit JSON null is the same widening through a different door:
		// encoding/json maps it onto a nil slice, which is indistinguishable from
		// "omitted" by the time Validate runs, and omitempty then erases it during
		// the canonical re-marshal. Daintree reads the resulting absent selection
		// as "no filter" — the whole worktree.
		{"null includePaths", map[string]any{"includePaths": nil}, "is null"},
		{"null scopePaths", map[string]any{"scopePaths": nil}, "is null"},
		{"null filter", map[string]any{"filter": nil}, "is null"},
		{"null exclude", map[string]any{"exclude": nil}, "is null"},
		{"null budget", map[string]any{"maxFileCount": nil}, "is null"},
		{"null format", map[string]any{"format": nil}, "is null"},
		{"blank changed", map[string]any{"changed": "   "}, "is blank"},
		{"blank format", map[string]any{"format": ""}, "is blank"},
		{"blank sort", map[string]any{"sort": ""}, "is blank"},
		{"blank exclude entry", map[string]any{"exclude": []string{""}}, "is blank"},
		{"blank always entry", map[string]any{"always": []string{" "}}, "is blank"},
	}

	for _, tc := range copyTreeCases {
		for _, u := range unsafe {
			t.Run(tc.name+"/"+u.name, func(t *testing.T) {
				tool := tc.build(Deps{})
				_, err := tool.Decode(argsWithOptions(t, tc.baseArgs, u.options))
				if err == nil {
					t.Fatalf("%s must be rejected — it would silently bundle the whole worktree", u.name)
				}
				// Pin WHICH guard fired. Without this the test would still pass if
				// the value were rejected incidentally (a type error, an unknown
				// field) while the widening guard itself had been deleted.
				if !strings.Contains(err.Error(), u.wantErr) {
					t.Errorf("rejected for the wrong reason: want a message containing %q, got %q", u.wantErr, err)
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
	// wantErr pins the MECHANISM, so a case can't quietly start passing for a
	// different reason than the one it is named for.
	cases := []struct {
		name    string
		args    string
		wantErr string
	}{
		{"unknown nested option key", `{"options":{"depth":2}}`, "unknown field"},
		{"scalar filter (host union arm we do not accept)", `{"options":{"filter":"**/*.go"}}`, "cannot unmarshal"},
		{"scalar exclude", `{"options":{"exclude":"vendor/**"}}`, "cannot unmarshal"},
		{"off-menu format", `{"options":{"format":"yaml"}}`, "options.format must be one of"},
		{"off-menu sort", `{"options":{"sort":"random"}}`, "options.sort must be one of"},
		{"zero maxFileCount", `{"options":{"maxFileCount":0}}`, "options.maxFileCount must be a positive integer"},
		{"negative charLimit", `{"options":{"charLimit":-1}}`, "options.charLimit must be a positive integer"},
		{"fractional maxTotalSize", `{"options":{"maxTotalSize":1.5}}`, "cannot unmarshal"},
		{"wrong type for modified", `{"options":{"modified":"yes"}}`, "cannot unmarshal"},
		{"unknown top-level key", `{"nope":1}`, "unknown field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := newCopyTreeGenerateTool(Deps{})
			_, err := tool.Decode(json.RawMessage(c.args))
			if err == nil {
				t.Fatalf("%s must be rejected at decode", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("rejected for the wrong reason: want a message containing %q, got %q", c.wantErr, err)
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
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("%s must be rejected: the clipboard copy has no active-worktree fallback", bad)
		}
		// Defense in depth: feed the handler the ORIGINAL payload, not the nil
		// Decode returned — otherwise this only proves the handler rejects EOF.
		if res := tool.Handle(context.Background(), json.RawMessage(bad), &tools.ToolContext{}); res.Ok || res.Error.Code != domain.CodeValidation {
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

// An explicitly empty or null worktree selector must be rejected, never quietly
// dropped. Dropping it is what turns "bundle THIS worktree" into "bundle
// whichever worktree happens to be active" — which is exactly the silent
// retarget Daintree's own .min(1) on its selectors exists to prevent
// (locationArgs.ts), and our schema already advertises minLength:1.
func TestCopyTreeRejectsEmptyWorktreeSelectors(t *testing.T) {
	cases := []struct {
		tool string
		args string
	}{
		{"copyTree.generate", `{"worktreeId":""}`},
		{"copyTree.generate", `{"worktreePath":"   "}`},
		{"copyTree.generate", `{"worktreeId":null}`},
		{"copyTree.injectToTerminal", `{"terminalId":"t1","worktreeId":""}`},
		{"copyTree.injectToTerminal", `{"terminalId":"t1","worktreeId":null}`},
		// A valid target alongside a blank one is still malformed: the host would
		// reject the blank spelling rather than let the good one paper over it.
		{"copyTree.generateAndCopyFile", `{"worktreeId":"wt-1","worktreePath":""}`},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.args, func(t *testing.T) {
			var tool tools.Tool
			for _, tc := range copyTreeCases {
				if tc.name == c.tool {
					tool = tc.build(Deps{})
				}
			}
			_, err := tool.Decode(json.RawMessage(c.args))
			if err == nil {
				t.Fatalf("an explicitly empty/null selector must be rejected, not dropped to the active worktree")
			}
			if !strings.Contains(err.Error(), "is blank") && !strings.Contains(err.Error(), "is null") {
				t.Errorf("rejected for the wrong reason: %q", err)
			}
		})
	}
}

// A repeated JSON key is the one shape a map-based scan cannot see: decoding the
// payload into map[string]any keeps only the LAST occurrence, while
// encoding/json merges repeated members into the same destination struct — so a
// null hidden in an earlier copy survives into the struct while the scan looks at
// a clean later one. That lands as "no selection", i.e. the whole worktree, on the
// clipboard. Built from raw strings on purpose: json.Marshal of a Go map can
// never produce a duplicate key, so the helper used elsewhere cannot express it.
func TestCopyTreeRejectsDuplicateKeys(t *testing.T) {
	cases := []struct {
		tool string
		args string
		// The scan walks lexically, so a null/blank inside the FIRST copy is
		// reported before the duplicate key itself is reached. Either message
		// proves the bypass is closed; wantErr records which one to expect so the
		// case still pins a mechanism rather than "some error happened".
		wantErr string
	}{
		// Purely duplicate — nothing else wrong with these, so ONLY duplicate
		// detection can reject them.
		{"copyTree.generate", `{"worktreeId":"wt-1","worktreeId":"wt-2"}`, "more than once"},
		{"copyTree.generate", `{"options":{"includePaths":["a.go"]},"options":{}}`, "more than once"},
		{"copyTree.generateAndCopyFile", `{"worktreeId":"wt-1","options":{"scopePaths":["internal"]},"options":{}}`, "more than once"},
		{"copyTree.injectToTerminal", `{"terminalId":"t1","options":{"filter":["*.go"]},"options":{}}`, "more than once"},
		// A repeated `name` is still a duplicate even though a BLANK name is the
		// one string the scan tolerates — the carve-out must not weaken duplicate
		// detection for the same key.
		{"copyTree.generate", `{"name":"","name":"auth flow"}`, "more than once"},
		// Duplicate hiding a null/blank in the earlier copy: caught on the way in.
		{"copyTree.generateAndCopyFile", `{"worktreeId":"wt-1","options":{"includePaths":null},"options":{}}`, "is null"},
		{"copyTree.generate", `{"options":{"changed":"","format":"xml"},"options":{"format":"xml"}}`, "is blank"},
		{"copyTree.injectToTerminal", `{"terminalId":"t1","options":{"scopePaths":null},"options":{}}`, "is null"},
	}
	for _, c := range cases {
		t.Run(c.tool+" "+c.args, func(t *testing.T) {
			var tool tools.Tool
			found := false
			for _, tc := range copyTreeCases {
				if tc.name == c.tool {
					tool = tc.build(Deps{})
					found = true
				}
			}
			if !found {
				t.Fatalf("unknown tool %q in the case table", c.tool)
			}
			_, err := tool.Decode(json.RawMessage(c.args))
			if err == nil {
				t.Fatalf("a repeated key must be rejected; it can hide a null that widens the bundle")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("rejected for the wrong reason: want a message containing %q, got %q", c.wantErr, err)
			}
		})
	}
}

// The host's optional `name` label (a free-text "2 to 4 words" run-history
// label, daintree#11734) must be declared on every wrapper schema — a field the
// closed schema doesn't name is a field the model can never pass — and must
// stay TOP-LEVEL: the host keeps it out of CopyTreeOptions so it stays out of
// the run-history dedupe key, which wantCopyTreeOptionKeys pins separately.
func TestCopyTreeSchemasDeclareName(t *testing.T) {
	for _, tc := range copyTreeCases {
		t.Run(tc.name, func(t *testing.T) {
			schema := parseSchema(t, tc.build(Deps{}).Schema)
			name, ok := schema.Properties["name"]
			if !ok {
				t.Fatalf("schema does not declare top-level name; the closed schema makes it unreachable")
			}
			if name.Type != "string" {
				t.Errorf("name must be a string, got %q", name.Type)
			}
			// Blank means "derive a label", so the schema must not contradict that
			// with a length floor, and the label must never be demanded.
			if name.MinLength != nil {
				t.Errorf("name must not carry minLength — blank means 'derive a label', not an error")
			}
			for _, r := range schema.Required {
				if r == "name" {
					t.Errorf("name must never be required")
				}
			}
			if !strings.Contains(name.Description, "2 to 4 words") || !strings.Contains(name.Description, "derive") {
				t.Errorf("name description must carry the host's guidance (2 to 4 words; omitted derives a label), got %q", name.Description)
			}
		})
	}
}

// The wire contract for `name`: a real label is forwarded verbatim, while a
// blank one decodes fine but stays OFF the wire — the host treats an absent
// name as "derive a label from the selection", and sending "" would hand it an
// empty label instead. Null keeps the family-wide rejection: optional means
// undefined-able, never nullable.
func TestCopyTreeForwardsName(t *testing.T) {
	wire := []struct {
		name     string
		value    any // nil = omit the field entirely
		wantWire any // nil = must be absent from the wire
	}{
		{"real label forwarded", "auth flow context", "auth flow context"},
		// Forwarded unchanged — presence is tested with TrimSpace, but the value
		// is the caller's to spell; the host owns any normalization.
		{"padded label forwarded verbatim", "  auth flow context ", "  auth flow context "},
		{"omitted stays absent", nil, nil},
		{"empty string stays absent", "", nil},
		{"whitespace-only stays absent", "   ", nil},
	}
	for _, tc := range copyTreeCases {
		for _, w := range wire {
			t.Run(tc.name+"/"+w.name, func(t *testing.T) {
				payload := map[string]any{}
				for k, v := range tc.baseArgs {
					payload[k] = v
				}
				if w.value != nil {
					payload["name"] = w.value
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatalf("marshal args: %v", err)
				}
				mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
				tool := tc.build(Deps{MCP: mcp})
				decoded, err := tool.Decode(raw)
				if err != nil {
					t.Fatalf("name=%#v must decode: %v", w.value, err)
				}
				if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
					t.Fatalf("expected ok, got %+v", res.Error)
				}
				got, present := mcp.lastArgs["name"]
				if w.wantWire == nil {
					if present {
						t.Errorf("blank/omitted name must stay off the wire so the host derives a label, got %#v", got)
					}
				} else if got != w.wantWire {
					t.Errorf("name mangled in transit: got %#v want %#v", got, w.wantWire)
				}
			})
		}
	}

	// The blank carve-out is EXACTLY the top-level `name` key. The same key
	// nested anywhere else is still a blank string in a family where blank is
	// never meaningful — the pre-scan must reject it before the strict decoder
	// even reports the unknown field.
	for _, tc := range copyTreeCases {
		t.Run(tc.name+"/blank options.name still rejected", func(t *testing.T) {
			tool := tc.build(Deps{})
			_, err := tool.Decode(argsWithOptions(t, tc.baseArgs, map[string]any{"name": " "}))
			if err == nil {
				t.Fatalf("a blank nested name must not inherit the top-level carve-out")
			}
			if !strings.Contains(err.Error(), "options.name is blank") {
				t.Errorf("rejected for the wrong reason: %q", err)
			}
		})
		t.Run(tc.name+"/null name rejected", func(t *testing.T) {
			payload := map[string]any{"name": nil}
			for k, v := range tc.baseArgs {
				payload[k] = v
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			tool := tc.build(Deps{})
			if _, err := tool.Decode(raw); err == nil {
				t.Fatalf("null name must be rejected — optional means undefined-able, not nullable")
			} else if !strings.Contains(err.Error(), "name is null") {
				t.Errorf("rejected for the wrong reason: %q", err)
			}
		})
	}

	// A non-string name is a type error, not a label. The raw scan has nothing
	// to say about numbers/arrays/objects, so the STRICT DECODER must be the
	// guard — this is what catches a wrapper whose Name field quietly stopped
	// being a string while every valid-string case stayed green.
	for _, tc := range copyTreeCases {
		for _, bad := range []string{`5`, `["auth"]`, `{"label":"auth"}`} {
			t.Run(tc.name+"/non-string name "+bad, func(t *testing.T) {
				payload := map[string]any{"name": json.RawMessage(bad)}
				for k, v := range tc.baseArgs {
					payload[k] = v
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatalf("marshal args: %v", err)
				}
				tool := tc.build(Deps{})
				_, err = tool.Decode(raw)
				if err == nil {
					t.Fatalf("a non-string name must fail the strict decode")
				}
				if !strings.Contains(err.Error(), "cannot unmarshal") || !strings.Contains(err.Error(), "name") {
					t.Errorf("rejected for the wrong reason: want a type error naming the field, got %q", err)
				}
			})
		}
	}

	// The carve-out is computed from the rendered token path, and an empty
	// top-level key used to leave its children LOOKING top-level. Pin the fix:
	// a blank name nested under "" is still a blank string, not the label.
	t.Run("blank name under an empty top-level key is rejected", func(t *testing.T) {
		tool := newCopyTreeGenerateTool(Deps{})
		if _, err := tool.Decode(json.RawMessage(`{"":{"name":""}}`)); err == nil {
			t.Fatalf("an empty top-level key must not launder a nested blank name")
		} else if !strings.Contains(err.Error(), "is blank") {
			t.Errorf("rejected for the wrong reason: %q", err)
		}
	})
}

// An empty exclude/always list is LEGAL host-side and means "clear the project's
// configured defaults for this call". Daintree back-fills excludedPaths /
// alwaysExclude / alwaysInclude only when the field is undefined
// (electron/ipc/handlers/copyTree.ts, mergeCopyTreeOptions), so an empty list
// erased by omitempty would arrive as undefined and restore precisely the
// patterns the caller was trying to drop. Since `always` is a force-include that
// overrides the filter, a project pattern like "**/*" would then defeat a
// curated includePaths — a narrow request widened back to the whole worktree.
func TestCopyTreeEmptyExcludeAndAlwaysSurviveToTheWire(t *testing.T) {
	for _, field := range []string{"exclude", "always"} {
		t.Run(field, func(t *testing.T) {
			mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
			tool := newCopyTreeGenerateTool(Deps{MCP: mcp})
			decoded, err := tool.Decode(argsWithOptions(t, nil, map[string]any{
				"includePaths": []string{"safe.go"},
				field:          []string{},
			}))
			if err != nil {
				t.Fatalf("an empty %s is legal host-side and must decode: %v", field, err)
			}
			if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
				t.Fatalf("expected ok: %+v", res.Error)
			}
			opts, _ := mcp.lastArgs["options"].(map[string]any)
			got, present := opts[field]
			if !present {
				t.Fatalf("an empty %s was erased; Daintree will back-fill the project's own patterns and can widen the bundle", field)
			}
			// Assert the type too: a present null, string or object would all
			// yield a nil []any of length 0 and pass a length-only check.
			list, ok := got.([]any)
			if !ok || len(list) != 0 {
				t.Errorf("%s should have arrived as an empty JSON array, got %#v", field, got)
			}
		})
	}

	// An empty options object carries no instruction at all, so it may be dropped
	// — the host treats {} and absence identically.
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCopyTreeGenerateTool(Deps{MCP: mcp})
	decoded, err := tool.Decode(json.RawMessage(`{"options":{}}`))
	if err != nil {
		t.Fatalf("an empty options object must decode: %v", err)
	}
	if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("expected ok: %+v", res.Error)
	}
	if _, present := mcp.lastArgs["options"]; present {
		t.Errorf("an empty options object should stay off the wire, got %v", mcp.lastArgs["options"])
	}
}

// A rejected injection must not reach the MCP *or* mark the terminal as
// command-sent: that mark invalidates cross-call settle evidence, so firing it
// for a call that never happened would make a settled agent look busy.
func TestCopyTreeInjectRejectionTouchesNothing(t *testing.T) {
	for _, bad := range []string{`{"terminalId":"  "}`, `{"terminalId":"t1","options":{"includePaths":[]}}`} {
		mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
		obs := &recordObserver{}
		tool := newCopyTreeInjectTool(Deps{MCP: mcp, Observer: obs})

		_, err := tool.Decode(json.RawMessage(bad))
		if err == nil {
			t.Fatalf("%s must be rejected at decode", bad)
		}
		// Feed the ORIGINAL raw (not the nil Decode returned) so the handler's
		// defense-in-depth path is genuinely exercised on real arguments.
		res := tool.Handle(context.Background(), json.RawMessage(bad), &tools.ToolContext{})
		if res.Ok || res.Error.Code != domain.CodeValidation {
			t.Errorf("handler must refuse %s: %+v", bad, res)
		}
		if mcp.lastName != "" {
			t.Errorf("rejected inject must not reach MCP, called %q", mcp.lastName)
		}
		if len(obs.marked) != 0 {
			t.Errorf("rejected inject must not mark the terminal command-sent: %v", obs.marked)
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
