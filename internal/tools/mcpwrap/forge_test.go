package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

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

// The forge tool schemas must mirror the host's strict forge contract exactly:
// the host rejects an unknown key outright, so a field we advertise but it does
// not accept becomes an undiagnosable refusal. This pins the field SETS, which is
// where the pre-#299 bug lived (`labels`/`limit` were advertised and do not exist).
func TestForgeSchemasMatchHostFieldSets(t *testing.T) {
	location := []string{"worktreeId", "worktreePath", "cwd"}
	paging := []string{"cursor", "perPage", "sort", "direction", "bypassCache", "view"}

	cases := []struct {
		tool   *tools.Tool
		want   []string
		absent []string
	}{
		{
			tool: newForgeListIssuesTool(),
			want: append(append([]string{"state", "search"}, paging...), location...),
			// The pre-#299 description advertised these; the host has never accepted them.
			absent: []string{"labels", "limit", "arguments"},
		},
		{
			tool: newForgeListPRsTool(),
			want: append(append([]string{"state"}, paging...), location...),
			// The host's PR list schema has no `search` at all — advertising it would
			// produce a strict refusal the model cannot recover from.
			absent: []string{"search", "labels", "limit", "arguments"},
		},
		{
			tool:   newForgeGetIssueTool(),
			want:   append([]string{"issueNumber"}, location...),
			absent: []string{"arguments", "prNumber"},
		},
		{
			tool:   newForgeGetPRTool(),
			want:   append([]string{"prNumber"}, location...),
			absent: []string{"arguments", "issueNumber"},
		},
	}

	for _, c := range cases {
		t.Run(c.tool.Name, func(t *testing.T) {
			schema := decodeSchema(t, c.tool)
			if schema["additionalProperties"] != false {
				t.Errorf("%s must set additionalProperties:false (closed shape)", c.tool.Name)
			}
			props := schemaProps(t, c.tool)
			for _, k := range c.want {
				if _, ok := props[k]; !ok {
					t.Errorf("%s schema is missing host field %q", c.tool.Name, k)
				}
			}
			for _, k := range c.absent {
				if _, ok := props[k]; ok {
					t.Errorf("%s schema advertises %q, which the host does not accept", c.tool.Name, k)
				}
			}
			if len(props) != len(c.want) {
				t.Errorf("%s schema has %d properties, want exactly %d (%v)", c.tool.Name, len(props), len(c.want), c.want)
			}
		})
	}
}

// Bounds and enums must be real JSON-Schema keywords, not prose in a description:
// the backend forwards `parameters` to the model verbatim, so only keywords bind.
func TestForgeListSchemasEncodeBoundsAsKeywords(t *testing.T) {
	for _, tool := range []*tools.Tool{newForgeListIssuesTool(), newForgeListPRsTool()} {
		t.Run(tool.Name, func(t *testing.T) {
			props := schemaProps(t, tool)

			perPage, _ := props["perPage"].(map[string]any)
			if perPage["type"] != "integer" || perPage["minimum"] != float64(1) || perPage["maximum"] != float64(100) {
				t.Errorf("perPage must be integer with minimum 1 / maximum 100, got %#v", perPage)
			}
			if perPage["default"] != float64(20) {
				t.Errorf("perPage must advertise the host default 20, got %#v", perPage["default"])
			}

			enums := map[string][]string{
				"sort":      {"created", "updated"},
				"direction": {"asc", "desc"},
				"view":      {"summary", "full"},
			}
			// The two list tools intentionally differ on `state` — issues have no
			// "merged" and PRs do.
			if tool.Name == "forge.listIssues" {
				enums["state"] = []string{"open", "closed", "all"}
			} else {
				enums["state"] = []string{"open", "closed", "merged", "all"}
			}
			for field, want := range enums {
				spec, _ := props[field].(map[string]any)
				raw, ok := spec["enum"].([]any)
				if !ok {
					t.Errorf("%s must declare an enum keyword, got %#v", field, spec)
					continue
				}
				got := make([]string, 0, len(raw))
				for _, v := range raw {
					got = append(got, v.(string))
				}
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s enum = %v, want %v", field, got, want)
				}
			}
			if props["view"].(map[string]any)["default"] != "summary" {
				t.Errorf("view must default to the host's summary projection")
			}
		})
	}

	// The get tools keep a positive-integer keyword bound on their number.
	for _, c := range []struct {
		tool  *tools.Tool
		field string
	}{
		{newForgeGetIssueTool(), "issueNumber"},
		{newForgeGetPRTool(), "prNumber"},
	} {
		spec, _ := schemaProps(t, c.tool)[c.field].(map[string]any)
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
		if _, ok := decodeSchema(t, tool)["required"]; ok {
			t.Errorf("%s must declare no required fields (every host list field is optional)", tool.Name)
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

			// An explicitly-false boolean is NOT "unset" — pointer fields keep the
			// two distinguishable, so bypassCache:false must still be forwarded.
			m2 := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			parsed2, err := tool.Decode(json.RawMessage(`{"bypassCache":false}`))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if res := tool.Handle(context.Background(), parsed2, ctxWith(m2)); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if got, ok := m2.lastArgs["bypassCache"]; !ok || got != false {
				t.Fatalf("explicit bypassCache:false must be forwarded, got %#v", m2.lastArgs)
			}
		})
	}
}

// The typed options reach the MCP call FLAT (no arguments wrapper) and under the
// host's own key names.
func TestForgeListIssuesForwardsTypedOptions(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := newForgeListIssuesTool()
	raw := json.RawMessage(`{"state":"all","search":"no:assignee -label:human-review","perPage":50,` +
		`"sort":"updated","direction":"asc","cursor":"cur-1","view":"full","worktreeId":"/wt/a"}`)
	parsed, err := tool.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	// Numbers arrive as a Go int, not the float64 the opaque map[string]any path
	// yields — the typed struct decodes them before they reach the call payload.
	want := map[string]any{
		"state": "all", "search": "no:assignee -label:human-review", "perPage": 50,
		"sort": "updated", "direction": "asc", "cursor": "cur-1", "view": "full", "worktreeId": "/wt/a",
	}
	for k, v := range want {
		if got := m.lastArgs[k]; got != v {
			t.Errorf("%s forwarded as %#v, want %#v", k, got, v)
		}
	}
	if _, wrapped := m.lastArgs["arguments"]; wrapped {
		t.Error("typed options must be forwarded flat, never nested under arguments")
	}
}

// forge.listPRs accepts "merged" (issues do not) and rejects `search` outright,
// matching the host's PR list schema.
func TestForgeListPRsStateAndNoSearch(t *testing.T) {
	tool := newForgeListPRsTool()
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	parsed, err := tool.Decode(json.RawMessage(`{"state":"merged"}`))
	if err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	if res := tool.Handle(context.Background(), parsed, ctxWith(m)); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastArgs["state"] != "merged" {
		t.Errorf("state:merged not forwarded: %#v", m.lastArgs)
	}
	if _, err := tool.Decode(json.RawMessage(`{"search":"anything"}`)); err == nil {
		t.Error("forge.listPRs must reject search — the host schema has no such field")
	}
	// The reverse: issues have no "merged" state.
	if _, err := newForgeListIssuesTool().Decode(json.RawMessage(`{"state":"merged"}`)); err == nil {
		t.Error("forge.listIssues must reject state:merged — issues have no merged state")
	}
}

// Out-of-contract values are rejected at BOTH gates: the registry's Decode (which
// runs Validate via StrictDecoder) and a direct Handle call (whose strictDecode is
// structural only). Neither may reach the transport.
func TestForgeRejectsOutOfContractValues(t *testing.T) {
	cases := []struct {
		label string
		tool  *tools.Tool
		raw   string
	}{
		{"perPage below minimum", newForgeListIssuesTool(), `{"perPage":0}`},
		{"perPage above maximum", newForgeListIssuesTool(), `{"perPage":101}`},
		{"empty cursor", newForgeListIssuesTool(), `{"cursor":""}`},
		{"bad sort", newForgeListIssuesTool(), `{"sort":"priority"}`},
		{"bad direction", newForgeListPRsTool(), `{"direction":"sideways"}`},
		{"bad view", newForgeListPRsTool(), `{"view":"verbose"}`},
		{"bad issue state", newForgeListIssuesTool(), `{"state":"draft"}`},
		{"bad pr state", newForgeListPRsTool(), `{"state":"rejected"}`},
		{"unknown key", newForgeListIssuesTool(), `{"labels":["bug"]}`},
		{"legacy arguments wrapper", newForgeListIssuesTool(), `{"arguments":{"state":"open"}}`},
		{"zero issueNumber", newForgeGetIssueTool(), `{"issueNumber":0}`},
		{"negative issueNumber", newForgeGetIssueTool(), `{"issueNumber":-3}`},
		{"string issueNumber", newForgeGetIssueTool(), `{"issueNumber":"299"}`},
		{"zero prNumber", newForgeGetPRTool(), `{"prNumber":0}`},
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

	// The bounds' inclusive edges stay legal.
	for _, raw := range []string{`{"perPage":1}`, `{"perPage":100}`, `{"state":"all"}`, `{"view":"summary"}`} {
		if _, err := newForgeListIssuesTool().Decode(json.RawMessage(raw)); err != nil {
			t.Errorf("forge.listIssues must accept %s: %v", raw, err)
		}
	}
}

// The get tools forward a typed number plus only the location fields that were set.
func TestForgeGetToolsForwardTypedNumber(t *testing.T) {
	for _, c := range []struct {
		tool  *tools.Tool
		raw   string
		field string
	}{
		{newForgeGetIssueTool(), `{"issueNumber":299,"cwd":"/repo"}`, "issueNumber"},
		{newForgeGetPRTool(), `{"prNumber":42,"cwd":"/repo"}`, "prNumber"},
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
			// A typed int, never a string and never a float64 — the whole point of
			// the typed contract is that the number survives as a number.
			num, ok := m.lastArgs[c.field].(int)
			if !ok || num <= 0 {
				t.Fatalf("%s must forward a positive int, got %#v", c.field, m.lastArgs[c.field])
			}
			if m.lastArgs["cwd"] != "/repo" {
				t.Errorf("cwd not forwarded: %#v", m.lastArgs)
			}
			if _, leaked := m.lastArgs["worktreeId"]; leaked {
				t.Errorf("an unset location field must be omitted, got %#v", m.lastArgs)
			}
		})
	}
}
