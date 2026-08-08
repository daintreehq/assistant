package mcpwrap

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

// typedForgeTools are the four reads that carry the host's typed contract. They
// are constructed per-call because a Tool is cheap and tests must not share one.
func typedForgeTools() []*tools.Tool {
	return []*tools.Tool{
		newForgeListIssuesTool(), newForgeListPRsTool(),
		newForgeGetIssueTool(), newForgeGetPRTool(),
	}
}

// validArgsFor is a minimal legal call for each typed tool, used by the
// transport-path table tests.
func validArgsFor(name string) json.RawMessage {
	switch name {
	case "forge.getIssue":
		return json.RawMessage(`{"issueNumber":299}`)
	case "forge.getPR":
		return json.RawMessage(`{"prNumber":42}`)
	default:
		return json.RawMessage(`{}`)
	}
}

// decodeSchema unmarshals a tool's Schema so the assertions below read the real
// JSON-Schema keywords the model will be shown (the schema is forwarded verbatim
// upstream, so a bound expressed only in prose is not a bound at all).
func decodeSchema(t *testing.T, tool *tools.Tool) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(tool.Schema, &s); err != nil {
		t.Fatalf("%s schema is not valid JSON: %v", tool.Name, err)
	}
	return s
}

func schemaProps(t *testing.T, tool *tools.Tool) map[string]any {
	t.Helper()
	props, ok := decodeSchema(t, tool)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s schema has no properties object", tool.Name)
	}
	return props
}

// propSpec fetches one property's keyword map, failing (never panicking) when the
// property is missing or malformed.
func propSpec(t *testing.T, tool *tools.Tool, field string) map[string]any {
	t.Helper()
	spec, ok := schemaProps(t, tool)[field].(map[string]any)
	if !ok {
		t.Fatalf("%s schema has no %q property object", tool.Name, field)
	}
	return spec
}

// The forge tool schemas must mirror the host's strict forge contract exactly:
// the host rejects an unknown key outright, so a field we advertise but it does
// not accept becomes an undiagnosable refusal. This pins the field SETS, which is
// where the pre-#299 bug lived (`labels`/`limit` were advertised and do not exist).
func TestForgeSchemasMatchHostFieldSets(t *testing.T) {
	location := []string{"worktreeId", "worktreePath", "cwd"}
	paging := []string{"cursor", "perPage", "sort", "direction", "bypassCache", "view"}
	// Concatenate through a helper so no case can share a backing array with another.
	set := func(head ...string) []string {
		out := append([]string{}, head...)
		return out
	}
	listFields := func(head ...string) []string {
		return append(append(set(head...), paging...), location...)
	}

	cases := []struct {
		tool   *tools.Tool
		want   []string
		absent []string
	}{
		{
			tool: newForgeListIssuesTool(),
			want: listFields("state", "search"),
			// The pre-#299 description advertised these; the host has never accepted them.
			absent: []string{"labels", "limit", "arguments"},
		},
		{
			tool: newForgeListPRsTool(),
			want: listFields("state"),
			// The host's PR list schema has no `search` at all — advertising it would
			// produce a strict refusal the model cannot recover from.
			absent: []string{"search", "labels", "limit", "arguments"},
		},
		{
			tool:   newForgeGetIssueTool(),
			want:   append(set("issueNumber"), location...),
			absent: []string{"arguments", "prNumber", "state"},
		},
		{
			tool:   newForgeGetPRTool(),
			want:   append(set("prNumber"), location...),
			absent: []string{"arguments", "issueNumber", "state"},
		},
	}

	for _, c := range cases {
		t.Run(c.tool.Name, func(t *testing.T) {
			schema := decodeSchema(t, c.tool)
			if schema["type"] != "object" {
				t.Errorf("%s schema type = %v, want object", c.tool.Name, schema["type"])
			}
			if schema["additionalProperties"] != false {
				t.Errorf("%s must set additionalProperties:false (closed shape)", c.tool.Name)
			}
			props := schemaProps(t, c.tool)
			got := make([]string, 0, len(props))
			for k := range props {
				got = append(got, k)
			}
			sort.Strings(got)
			want := append([]string{}, c.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s property set =\n  %v\nwant\n  %v", c.tool.Name, got, want)
			}
			for _, k := range c.absent {
				if _, ok := props[k]; ok {
					t.Errorf("%s schema advertises %q, which the host does not accept", c.tool.Name, k)
				}
			}
		})
	}
}

// Every model-visible keyword is pinned: the model sees the schema BEFORE it ever
// calls, so a missing enum or bound cannot be recovered by runtime validation.
func TestForgeSchemaKeywordsArePinned(t *testing.T) {
	sameSet := func(t *testing.T, spec map[string]any, field string, want []string) {
		t.Helper()
		raw, ok := spec["enum"].([]any)
		if !ok {
			t.Errorf("%s must declare an enum keyword, got %#v", field, spec)
			return
		}
		got := make([]string, 0, len(raw))
		for _, v := range raw {
			s, ok := v.(string)
			if !ok {
				t.Errorf("%s enum contains a non-string %#v", field, v)
				return
			}
			got = append(got, s)
		}
		gotSorted, wantSorted := append([]string{}, got...), append([]string{}, want...)
		sort.Strings(gotSorted)
		sort.Strings(wantSorted)
		if !reflect.DeepEqual(gotSorted, wantSorted) {
			t.Errorf("%s enum = %v, want %v", field, got, want)
		}
	}

	for _, tool := range []*tools.Tool{newForgeListIssuesTool(), newForgeListPRsTool()} {
		t.Run(tool.Name, func(t *testing.T) {
			perPage := propSpec(t, tool, "perPage")
			if perPage["type"] != "integer" || perPage["minimum"] != float64(1) || perPage["maximum"] != float64(100) {
				t.Errorf("perPage must be integer with minimum 1 / maximum 100, got %#v", perPage)
			}
			if perPage["default"] != float64(20) {
				t.Errorf("perPage must advertise the host default 20, got %#v", perPage["default"])
			}

			// The host rejects an empty cursor (it would alias page one's cache entry).
			cursor := propSpec(t, tool, "cursor")
			if cursor["type"] != "string" || cursor["minLength"] != float64(1) {
				t.Errorf("cursor must be a string with minLength 1, got %#v", cursor)
			}

			bypass := propSpec(t, tool, "bypassCache")
			if bypass["type"] != "boolean" || bypass["default"] != false {
				t.Errorf("bypassCache must be boolean defaulting to false, got %#v", bypass)
			}

			sameSet(t, propSpec(t, tool, "sort"), "sort", []string{"created", "updated"})
			sameSet(t, propSpec(t, tool, "direction"), "direction", []string{"asc", "desc"})
			sameSet(t, propSpec(t, tool, "view"), "view", []string{"summary", "full"})

			for field, want := range map[string]string{
				"sort": "created", "direction": "desc", "view": "summary", "state": "open",
			} {
				if got := propSpec(t, tool, field)["default"]; got != want {
					t.Errorf("%s default = %#v, want %q", field, got, want)
				}
			}

			// The two list tools differ ONLY here: PRs add "merged".
			wantState := []string{"open", "closed", "all"}
			if tool.Name == "forge.listPRs" {
				wantState = append(wantState, "merged")
			}
			sameSet(t, propSpec(t, tool, "state"), "state", wantState)

			for _, field := range []string{"worktreeId", "worktreePath", "cwd"} {
				if got := propSpec(t, tool, field)["type"]; got != "string" {
					t.Errorf("%s type = %#v, want string", field, got)
				}
			}
		})
	}

	if got := propSpec(t, newForgeListIssuesTool(), "search")["type"]; got != "string" {
		t.Errorf("search type = %#v, want string", got)
	}

	// The get tools keep a positive-integer keyword bound and require exactly it.
	for _, c := range []struct {
		tool  *tools.Tool
		field string
	}{
		{newForgeGetIssueTool(), "issueNumber"},
		{newForgeGetPRTool(), "prNumber"},
	} {
		spec := propSpec(t, c.tool, c.field)
		if spec["type"] != "integer" || spec["minimum"] != float64(1) {
			t.Errorf("%s.%s must be integer with minimum 1, got %#v", c.tool.Name, c.field, spec)
		}
		req, _ := decodeSchema(t, c.tool)["required"].([]any)
		if len(req) != 1 || req[0] != c.field {
			t.Errorf("%s must require exactly [%s], got %v", c.tool.Name, c.field, req)
		}
	}
}

// Every list field is optional host-side, so an argument-free call is legal and
// must stay legal — the disconnected-MCP tests dispatch forge.listIssues with `{}`.
func TestForgeListsAcceptEmptyArgs(t *testing.T) {
	for _, tool := range []*tools.Tool{newForgeListIssuesTool(), newForgeListPRsTool()} {
		// Absent and empty `required` are equivalent; only a non-empty one is a bug.
		if req, ok := decodeSchema(t, tool)["required"].([]any); ok && len(req) > 0 {
			t.Errorf("%s must require no fields (every host list field is optional), got %v", tool.Name, req)
		}
		if _, err := tool.Decode(json.RawMessage(`{}`)); err != nil {
			t.Errorf("%s must decode an empty argument object: %v", tool.Name, err)
		}
	}
}

// Unset optionals must be OMITTED from the outgoing call rather than sent as zero
// values: the host's list schemas are strict AND bounded, so a perPage:0 or
// state:"" for a field the model never chose would be refused.
func TestForgeListsOmitUnsetOptions(t *testing.T) {
	for _, tool := range []*tools.Tool{newForgeListIssuesTool(), newForgeListPRsTool()} {
		t.Run(tool.Name, func(t *testing.T) {
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			parsed, err := tool.Decode(json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if m.lastArgs == nil || len(m.lastArgs) != 0 {
				t.Fatalf("an empty call must forward an empty non-nil map, got %#v", m.lastArgs)
			}

			// Explicitly-set zero-ish values are NOT "unset" — pointer fields keep the
			// two distinguishable, so each must survive to the host.
			for _, tc := range []struct {
				raw   string
				field string
				want  any
			}{
				{`{"bypassCache":false}`, "bypassCache", false},
				{`{"bypassCache":true}`, "bypassCache", true},
				{`{"cursor":"x"}`, "cursor", "x"},
				{`{"perPage":1}`, "perPage", 1},
				{`{"perPage":100}`, "perPage", 100},
				{`{"worktreePath":"/wt/b"}`, "worktreePath", "/wt/b"},
			} {
				m2 := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
				parsed2, err := tool.Decode(json.RawMessage(tc.raw))
				if err != nil {
					t.Fatalf("decode %s: %v", tc.raw, err)
				}
				if res := tool.Handle(context.Background(), parsed2, ctxWith(m2)); !res.Ok {
					t.Fatalf("%s: expected ok, got %+v", tc.raw, res.Error)
				}
				if got, ok := m2.lastArgs[tc.field]; !ok || got != tc.want {
					t.Errorf("%s must forward %s=%#v, got %#v", tc.raw, tc.field, tc.want, m2.lastArgs)
				}
				if len(m2.lastArgs) != 1 {
					t.Errorf("%s must forward exactly one key, got %#v", tc.raw, m2.lastArgs)
				}
			}
		})
	}

	// An explicit empty search is legal host-side (unlike an empty cursor) and must
	// reach the host rather than being silently dropped as "unset".
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := newForgeListIssuesTool()
	parsed, err := tool.Decode(json.RawMessage(`{"search":""}`))
	if err != nil {
		t.Fatalf("decode empty search: %v", err)
	}
	if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if got, ok := m.lastArgs["search"]; !ok || got != "" {
		t.Errorf(`search:"" must be forwarded explicitly, got %#v`, m.lastArgs)
	}
}

// The typed options reach the MCP call FLAT (no arguments wrapper), under the
// host's own key names, at the right action, with nothing extra.
func TestForgeListIssuesForwardsTypedOptions(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := newForgeListIssuesTool()
	raw := json.RawMessage(`{"state":"all","search":"no:assignee -label:human-review","perPage":50,` +
		`"sort":"updated","direction":"asc","cursor":"cur-1","view":"full","bypassCache":true,"worktreeId":"/wt/a"}`)
	parsed, err := tool.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastName != "forge.listIssues" {
		t.Fatalf("forwarded to %q, want forge.listIssues", m.lastName)
	}
	// Numbers arrive as a Go int, not the float64 the opaque map[string]any path
	// yields — the typed struct decodes them before they reach the call payload.
	want := map[string]any{
		"state": "all", "search": "no:assignee -label:human-review", "perPage": 50,
		"sort": "updated", "direction": "asc", "cursor": "cur-1", "view": "full",
		"bypassCache": true, "worktreeId": "/wt/a",
	}
	if !reflect.DeepEqual(m.lastArgs, want) {
		t.Errorf("forwarded payload =\n  %#v\nwant\n  %#v", m.lastArgs, want)
	}
}

// forge.listPRs forwards its own paging/location set to its own action.
func TestForgeListPRsForwardsTypedOptions(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := newForgeListPRsTool()
	parsed, err := tool.Decode(json.RawMessage(`{"state":"merged","perPage":5,"view":"full","cwd":"/repo"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastName != "forge.listPRs" {
		t.Fatalf("forwarded to %q, want forge.listPRs", m.lastName)
	}
	want := map[string]any{"state": "merged", "perPage": 5, "view": "full", "cwd": "/repo"}
	if !reflect.DeepEqual(m.lastArgs, want) {
		t.Errorf("forwarded payload =\n  %#v\nwant\n  %#v", m.lastArgs, want)
	}
}

// The two list tools' state enums are NOT interchangeable, and only issues take a
// search — both differences are strict host-side, so both must be caught locally.
func TestForgeListStateEnumsAreNotInterchangeable(t *testing.T) {
	if _, err := newForgeListPRsTool().Decode(json.RawMessage(`{"search":"anything"}`)); err == nil {
		t.Error("forge.listPRs must reject search — the host schema has no such field")
	}
	// Also at the direct-Handle gate, which decodes independently of Decode.
	m := &fakeMCP{connected: true}
	res := newForgeListPRsTool().Handle(context.Background(), json.RawMessage(`{"search":"x"}`), ctxWith(m))
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Errorf("forge.listPRs Handle must reject search, got %+v", res)
	}
	if m.lastName != "" {
		t.Errorf("a rejected call must never reach the MCP, called %q", m.lastName)
	}
	if _, err := newForgeListIssuesTool().Decode(json.RawMessage(`{"state":"merged"}`)); err == nil {
		t.Error("forge.listIssues must reject state:merged — issues have no merged state")
	}
}

// Out-of-contract values are rejected at BOTH gates: the registry's Decode (which
// runs Validate via StrictDecoder) and a direct Handle call (whose strictDecode is
// structural only). Neither may reach the transport.
func TestForgeRejectsOutOfContractValues(t *testing.T) {
	issues, prs := newForgeListIssuesTool(), newForgeListPRsTool()
	getIssue, getPR := newForgeGetIssueTool(), newForgeGetPRTool()
	cases := []struct {
		label string
		tool  *tools.Tool
		raw   string
	}{
		{"perPage below minimum", issues, `{"perPage":0}`},
		{"perPage above maximum", issues, `{"perPage":101}`},
		{"perPage negative", issues, `{"perPage":-1}`},
		{"perPage non-integral", issues, `{"perPage":1.5}`},
		{"perPage as string", issues, `{"perPage":"20"}`},
		{"empty cursor", issues, `{"cursor":""}`},
		{"bad sort", issues, `{"sort":"priority"}`},
		{"bad direction", prs, `{"direction":"sideways"}`},
		{"bad view", prs, `{"view":"verbose"}`},
		{"bad issue state", issues, `{"state":"draft"}`},
		{"bad pr state", prs, `{"state":"rejected"}`},
		{"state wrong type", issues, `{"state":3}`},
		{"bypassCache wrong type", issues, `{"bypassCache":"yes"}`},
		{"unknown key labels", issues, `{"labels":["bug"]}`},
		{"unknown key limit", issues, `{"limit":10}`},
		{"legacy arguments wrapper", issues, `{"arguments":{"state":"open"}}`},
		{"zero issueNumber", getIssue, `{"issueNumber":0}`},
		{"negative issueNumber", getIssue, `{"issueNumber":-3}`},
		{"string issueNumber", getIssue, `{"issueNumber":"299"}`},
		{"missing issueNumber", getIssue, `{"cwd":"/repo"}`},
		{"zero prNumber", getPR, `{"prNumber":0}`},
		{"negative prNumber", getPR, `{"prNumber":-7}`},
		{"missing prNumber", getPR, `{"cwd":"/repo"}`},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if _, err := c.tool.Decode(json.RawMessage(c.raw)); err == nil {
				t.Errorf("%s: Decode must reject %s", c.tool.Name, c.raw)
			}
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			res := c.tool.Handle(context.Background(), json.RawMessage(c.raw), ctxWith(m))
			if res.Ok || res.Error.Code != codeInvalidArgs {
				t.Errorf("%s: Handle must fail INVALID_ARGS for %s, got %+v", c.tool.Name, c.raw, res)
			}
			if m.lastName != "" {
				t.Errorf("%s: invalid args must never reach the MCP, called %q", c.tool.Name, m.lastName)
			}
		})
	}

	// Every legal enum value and both inclusive perPage edges stay accepted.
	legal := map[*tools.Tool][]string{
		issues: {`{"perPage":1}`, `{"perPage":100}`, `{"state":"open"}`, `{"state":"closed"}`, `{"state":"all"}`},
		prs:    {`{"state":"open"}`, `{"state":"closed"}`, `{"state":"all"}`, `{"state":"merged"}`},
	}
	for tool, raws := range legal {
		for _, raw := range raws {
			if _, err := tool.Decode(json.RawMessage(raw)); err != nil {
				t.Errorf("%s must accept %s: %v", tool.Name, raw, err)
			}
		}
	}
	for _, raw := range []string{
		`{"sort":"created"}`, `{"sort":"updated"}`, `{"direction":"asc"}`, `{"direction":"desc"}`,
		`{"view":"summary"}`, `{"view":"full"}`, `{"cursor":"x"}`,
	} {
		for _, tool := range []*tools.Tool{newForgeListIssuesTool(), newForgeListPRsTool()} {
			if _, err := tool.Decode(json.RawMessage(raw)); err != nil {
				t.Errorf("%s must accept %s: %v", tool.Name, raw, err)
			}
		}
	}
}

// A rejected call must tell the model what to do INSTEAD, or it retries the same
// shape. These are the mistakes this contract actually invites.
//
// The hint MUST ride the Decode error: Registry.Dispatch returns as soon as
// Decode fails and never calls Handle, so a hint that only existed in the handler
// would never reach a real model. Both gates are asserted.
func TestForgeInvalidArgsHintsAreSelfCorrecting(t *testing.T) {
	cases := []struct {
		tool     *tools.Tool
		raw      string
		wantHint string
	}{
		{newForgeListIssuesTool(), `{"labels":["bug"]}`, "search"},
		{newForgeListIssuesTool(), `{"limit":10}`, "perPage"},
		{newForgeListIssuesTool(), `{"arguments":{"state":"open"}}`, "TOP LEVEL"},
		{newForgeListPRsTool(), `{"search":"x"}`, "NO search"},
		{newForgeListPRsTool(), `{"labels":["bug"]}`, "no label or search"},
		{newForgeGetIssueTool(), `{"prNumber":5}`, "issueNumber"},
		{newForgeGetPRTool(), `{"issueNumber":5}`, "prNumber"},
		// A getter must NOT be told to use `search` — it has no such field, so
		// that advice would just trade one refusal for another.
		{newForgeGetIssueTool(), `{"issueNumber":1,"labels":["bug"]}`, "ONE item"},
		{newForgeGetPRTool(), `{"prNumber":1,"limit":5}`, "ONE item"},
	}
	for _, c := range cases {
		t.Run(c.tool.Name+c.raw, func(t *testing.T) {
			// The production gate.
			_, err := c.tool.Decode(json.RawMessage(c.raw))
			if err == nil {
				t.Fatalf("%s should have been rejected at Decode", c.raw)
			}
			if !strings.Contains(err.Error(), c.wantHint) {
				t.Errorf("Decode error must point at %q, got %q", c.wantHint, err.Error())
			}
			// The direct-Handle gate keeps the same guidance.
			res := c.tool.Handle(context.Background(), json.RawMessage(c.raw), ctxWith(&fakeMCP{connected: true}))
			if res.Ok {
				t.Fatalf("%s should have failed in Handle", c.raw)
			}
			if !strings.Contains(res.Error.Message, c.wantHint) {
				t.Errorf("Handle error must point at %q, got %q", c.wantHint, res.Error.Message)
			}
		})
	}

	// A getter must never be steered at `search`, which it does not accept.
	for _, tool := range []*tools.Tool{newForgeGetIssueTool(), newForgeGetPRTool()} {
		_, err := tool.Decode(json.RawMessage(`{"issueNumber":1,"prNumber":1,"labels":["x"]}`))
		if err == nil {
			t.Fatalf("%s should have rejected the mixed shape", tool.Name)
		}
		if strings.Contains(err.Error(), "search:") {
			t.Errorf("%s must not recommend search — it has no such field: %q", tool.Name, err.Error())
		}
	}

	// The label/assignee/author case is shared by BOTH getters, so each must be sent
	// to its OWN list tool. Steering forge.getPR at forge.listIssues would hand the
	// model the wrong tool family — precisely the misdirection this contract removes.
	for _, c := range []struct {
		tool  *tools.Tool
		want  string
		avoid string
	}{
		{newForgeGetIssueTool(), "forge.listIssues", "forge.listPRs"},
		{newForgeGetPRTool(), "forge.listPRs", "forge.listIssues"},
	} {
		_, err := c.tool.Decode(json.RawMessage(`{"labels":["bug"],"assignee":"x"}`))
		if err == nil {
			t.Fatalf("%s should have rejected a label/assignee filter", c.tool.Name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s must be steered at %s, got %q", c.tool.Name, c.want, err.Error())
		}
		if strings.Contains(err.Error(), c.avoid) {
			t.Errorf("%s must not be steered at %s (wrong tool family): %q", c.tool.Name, c.avoid, err.Error())
		}
	}
}

// An empty worktree selector must be rejected, not forwarded: the host treats an
// ABSENT selector as "use the active worktree", so an empty string that slipped
// through would silently retarget the call.
func TestForgeRejectsEmptyWorktreeSelector(t *testing.T) {
	for _, tool := range typedForgeTools() {
		for _, field := range []string{"worktreeId", "worktreePath", "cwd"} {
			raw := `{"` + field + `":""`
			switch tool.Name {
			case "forge.getIssue":
				raw += `,"issueNumber":299`
			case "forge.getPR":
				raw += `,"prNumber":42`
			}
			raw += "}"

			if _, err := tool.Decode(json.RawMessage(raw)); err == nil {
				t.Errorf("%s must reject %s at Decode", tool.Name, raw)
			}
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			res := tool.Handle(context.Background(), json.RawMessage(raw), ctxWith(m))
			if res.Ok || res.Error.Code != codeInvalidArgs {
				t.Errorf("%s must reject %s in Handle, got %+v", tool.Name, raw, res)
			}
			if m.lastName != "" {
				t.Errorf("%s: an empty selector must never reach the MCP", tool.Name)
			}
		}
	}
}

// The get tools forward a typed number plus only the location fields that were set.
func TestForgeGetToolsForwardTypedNumber(t *testing.T) {
	for _, c := range []struct {
		tool *tools.Tool
		raw  string
		want map[string]any
	}{
		{newForgeGetIssueTool(), `{"issueNumber":299,"cwd":"/repo"}`, map[string]any{"issueNumber": 299, "cwd": "/repo"}},
		{newForgeGetPRTool(), `{"prNumber":42,"cwd":"/repo"}`, map[string]any{"prNumber": 42, "cwd": "/repo"}},
	} {
		t.Run(c.tool.Name, func(t *testing.T) {
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			parsed, err := c.tool.Decode(json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if res := c.tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if m.lastName != c.tool.Name {
				t.Fatalf("forwarded to %q, want %q", m.lastName, c.tool.Name)
			}
			// Exact payload: the number survives as a typed int and no unset
			// location field leaks in alongside it.
			if !reflect.DeepEqual(m.lastArgs, c.want) {
				t.Errorf("forwarded payload =\n  %#v\nwant\n  %#v", m.lastArgs, c.want)
			}
		})
	}
}

// Every typed forge read maps the transport outcomes the same way: a Daintree
// refusal is MCP_TOOL_ERROR, a disconnected MCP is MCP_UNAVAILABLE and never
// reaches the wire, and the success envelope passes structuredContent through.
func TestForgeTypedToolsTransportPaths(t *testing.T) {
	for _, tool := range typedForgeTools() {
		t.Run(tool.Name, func(t *testing.T) {
			args := validArgsFor(tool.Name)

			m := &fakeMCP{connected: true, result: tools.MCPCallResult{
				Text: "raw", StructuredContent: map[string]any{"items": []any{"one"}},
			}}
			res := tool.Handle(context.Background(), args, ctxWith(m))
			if !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			payload, ok := res.Result.(map[string]any)
			if !ok {
				t.Fatalf("result not a map: %#v", res.Result)
			}
			sc, ok := payload["structuredContent"].(map[string]any)
			if !ok || sc["items"] == nil {
				t.Errorf("structuredContent must pass through untouched, got %#v", payload["structuredContent"])
			}

			refuse := &fakeMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "not found"}}
			if res := tool.Handle(context.Background(), args, ctxWith(refuse)); res.Ok || res.Error.Code != codeMCPToolError {
				t.Errorf("expected MCP_TOOL_ERROR on IsError, got %+v", res)
			}

			down := &fakeMCP{connected: false}
			res = tool.Handle(context.Background(), args, ctxWith(down))
			if res.Ok || res.Error.Code != codeMCPUnavailable {
				t.Errorf("expected MCP_UNAVAILABLE, got %+v", res)
			}
			if !strings.Contains(res.Error.Message, "/reconnect") {
				t.Errorf("disconnected hint must name /reconnect: %q", res.Error.Message)
			}
			if down.lastName != "" {
				t.Errorf("a disconnected call must not reach the MCP, called %q", down.lastName)
			}

			if tool.Risk != domain.RiskRead {
				t.Errorf("risk = %s, want read", tool.Risk)
			}
		})
	}
}

// The tool descriptions ARE the model's contract — the pre-#299 bug was purely a
// description that advertised fields the host never accepted. These assertions are
// semantic (which field names appear at all), not prose-exact, so ordinary wording
// edits stay free while a regression to the old guidance fails loudly.
func TestForgeDescriptionsStateTheRealContract(t *testing.T) {
	issues := newForgeListIssuesTool()
	// The exact false claim this issue removes must not come back.
	for _, banned := range []string{"labels", "limit"} {
		if strings.Contains(strings.ToLower(issues.Description), banned) {
			t.Errorf("forge.listIssues description must not advertise %q — the host has no such argument", banned)
		}
	}
	if !strings.Contains(issues.Description, "search") {
		t.Error("forge.listIssues description must point filters at search")
	}

	prs := newForgeListPRsTool()
	// The model must learn PRs have no search HERE; the host answers with a
	// strict refusal that carries no such guidance. Assert the NEGATION, not just
	// the word — "use search to filter PRs" would be exactly backwards.
	if !strings.Contains(prs.Description, "NO search") {
		t.Errorf("forge.listPRs description must explicitly say it has NO search field, got %q", prs.Description)
	}

	for _, tool := range []*tools.Tool{issues, prs} {
		if !strings.Contains(tool.Description, "summary") || !strings.Contains(tool.Description, "full") {
			t.Errorf("%s description must explain the summary/full projection", tool.Name)
		}
	}

	// A branch name in worktreeId silently targets nothing — the schema is the
	// only place the model is warned.
	desc, ok := propSpec(t, issues, "worktreeId")["description"].(string)
	if !ok {
		t.Fatal("worktreeId has no string description")
	}
	if !strings.Contains(desc, "branch") {
		t.Error("worktreeId description must warn that it is not a branch name")
	}

	if !strings.Contains(newForgeGetIssueTool().Description, "issueNumber") {
		t.Error("forge.getIssue description must name the issueNumber field literally")
	}
	if !strings.Contains(newForgeGetPRTool().Description, "prNumber") {
		t.Error("forge.getPR description must name the prNumber field literally")
	}
}
