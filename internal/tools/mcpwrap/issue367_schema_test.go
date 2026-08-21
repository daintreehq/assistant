package mcpwrap

import (
	"encoding/json"
	"testing"
)

// The JSON Schema is the ONLY thing the model ever sees — it is forwarded verbatim into
// the tool spec. Go's Validate() runs after the model has already composed a call, so a
// bound that exists only in Validate() teaches the model nothing and merely rejects it
// one round later. These tests therefore assert on the SCHEMA, not on the handler.
//
// Both directions are failures with real consequences, because each wrapped action is
// refused on the raw daintree.call path:
//
//   - A bound TIGHTER than the host's removes capability outright. There is no other
//     route to the action, so an argument the wrapper won't express cannot be sent at all.
//   - A bound LOOSER than the host's turns a clean local rejection into a mid-turn
//     validation error from Daintree, which the model can act on far less usefully.
//
// The expectations below are transcribed from the Daintree host's zod argsSchemas
// (src/services/actions/definitions/*.ts). Where the host declares a plain optional
// string, the wrapper must NOT invent a minLength — see the presence-awareness notes on
// browser.getConsoleMessages and worktree.resource.status for why an explicit empty
// string has to reach the host rather than be rejected or dropped here.

type propExpect struct {
	typ       string
	minimum   *float64
	maximum   *float64
	minLength *float64
	enum      []string
	// hasDefault records that the host declares a default the schema should advertise.
	hasDefault bool
}

func f(v float64) *float64 { return &v }

func TestIssue367SchemasMirrorTheHostContract(t *testing.T) {
	cases := []struct {
		tool     string
		required []string
		props    map[string]propExpect
	}{
		{
			// projectCheckActions.ts + shared/types/projectCheck.ts
			tool:     "project.runCheck",
			required: []string{"projectId", "runnerId"},
			props: map[string]propExpect{
				"projectId": {typ: "string", minLength: f(1)},
				"runnerId":  {typ: "string", minLength: f(1)},
				"cwd":       {typ: "string", minLength: f(1)},
				"timeoutMs": {typ: "integer", minimum: f(1000), maximum: f(3600000), hasDefault: true},
			},
		},
		{
			// projectActions.ts: z.object({ projectId: z.string().optional() }) — a PLAIN
			// optional string, so no minLength here.
			tool:     "project.detectRunners",
			required: nil,
			props:    map[string]propExpect{"projectId": {typ: "string"}},
		},
		{
			// forgeActions.ts + locationArgs.ts (worktreeLocationShape with legacy cwd).
			// Every selector there is `.min(1)`.
			tool:     "forge.listIssueComments",
			required: []string{"issueNumber"},
			props: map[string]propExpect{
				"cwd":          {typ: "string", minLength: f(1)},
				"worktreeId":   {typ: "string", minLength: f(1)},
				"worktreePath": {typ: "string", minLength: f(1)},
				"issueNumber":  {typ: "integer", minimum: f(1)},
				// The host accepts an EMPTY cursor, so no minLength: narrowing it would
				// make a legal page request unexpressible.
				"cursor":  {typ: "string"},
				"perPage": {typ: "integer", minimum: f(1), maximum: f(100), hasDefault: true},
			},
		},
		{
			// agentActions.ts: SessionHistoryListArgsSchema — ids `.min(1)`,
			// limit 1..100 default 20, offset >=0 default 0.
			tool:     "agentSessionHistory.list",
			required: nil,
			props: map[string]propExpect{
				"worktreeId": {typ: "string", minLength: f(1)},
				"projectId":  {typ: "string", minLength: f(1)},
				"limit":      {typ: "integer", minimum: f(1), maximum: f(100), hasDefault: true},
				"offset":     {typ: "integer", minimum: f(0), hasDefault: true},
			},
		},
		{
			// worktreeResourceActions.ts: a plain optional string. No minLength — the
			// host distinguishes an explicit "" (which fails) from omission (which falls
			// back to the focused worktree), and so must the wrapper.
			tool:     "worktree.resource.status",
			required: nil,
			props:    map[string]propExpect{"worktreeId": {typ: "string"}},
		},
		{
			// browserActions.ts: getConsoleMessagesArgsSchema.
			tool:     "browser.getConsoleMessages",
			required: nil,
			props: map[string]propExpect{
				"terminalId": {typ: "string"},
				"level":      {typ: "string", enum: []string{"log", "info", "warning", "error"}},
				"limit":      {typ: "integer", minimum: f(1), maximum: f(500)},
			},
		},
		{
			// logActions.ts
			tool:     "errors.recent",
			required: nil,
			props: map[string]propExpect{
				"limit":             {typ: "integer", minimum: f(1), maximum: f(50), hasDefault: true},
				"includesDismissed": {typ: "boolean", hasDefault: true},
			},
		},
		{
			tool:     "notifications.recent",
			required: nil,
			props: map[string]propExpect{
				"limit":      {typ: "integer", minimum: f(1), maximum: f(50), hasDefault: true},
				"type":       {typ: "string", enum: []string{"success", "error", "info", "warning"}},
				"unreadOnly": {typ: "boolean", hasDefault: true},
			},
		},
	}

	all := issue367Tools(t)
	for _, c := range cases {
		tool := findTool(all, c.tool)
		if tool == nil {
			t.Errorf("%s is not registered", c.tool)
			continue
		}
		var schema struct {
			Type                 string                     `json:"type"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			t.Errorf("%s: schema is not valid JSON: %v", c.tool, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("%s: schema type = %q, want object", c.tool, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: additionalProperties must be false, or a typo reaches Daintree as a silently ignored key", c.tool)
		}
		assertStringSet(t, c.tool, "required", schema.Required, c.required)

		// Exact property set, both directions: a MISSING property is a host argument with
		// no route at all (the raw path is denied), and an EXTRA one is a key Daintree
		// will reject mid-turn.
		var gotProps []string
		for name := range schema.Properties {
			gotProps = append(gotProps, name)
		}
		var wantProps []string
		for name := range c.props {
			wantProps = append(wantProps, name)
		}
		assertStringSet(t, c.tool, "properties", gotProps, wantProps)

		for name, want := range c.props {
			raw, present := schema.Properties[name]
			if !present {
				continue // already reported by the set comparison
			}
			var got struct {
				Type      string   `json:"type"`
				Minimum   *float64 `json:"minimum"`
				Maximum   *float64 `json:"maximum"`
				MinLength *float64 `json:"minLength"`
				Enum      []string `json:"enum"`
				Default   any      `json:"default"`
				Desc      string   `json:"description"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("%s.%s: not valid JSON: %v", c.tool, name, err)
				continue
			}
			where := c.tool + "." + name
			if got.Type != want.typ {
				t.Errorf("%s: type = %q, want %q", where, got.Type, want.typ)
			}
			assertNum(t, where, "minimum", got.Minimum, want.minimum)
			assertNum(t, where, "maximum", got.Maximum, want.maximum)
			assertNum(t, where, "minLength", got.MinLength, want.minLength)
			assertStringSet(t, where, "enum", got.Enum, want.enum)
			if want.hasDefault && got.Default == nil {
				t.Errorf("%s: the host declares a default; advertise it so the model knows what omission means", where)
			}
			// Every property is model-facing; an undescribed one is a key the model has
			// to guess the meaning of, which is the whole failure mode #367 addresses.
			if got.Desc == "" {
				t.Errorf("%s: has no description", where)
			}
		}
	}
}

func assertNum(t *testing.T, where, key string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s: %s = %v, but the host declares none — a bound tighter than the host removes capability, since the raw path is denied", where, key, *got)
	case want != nil && got == nil:
		t.Errorf("%s: %s is absent, want %v — the model never learns the bound and only finds out mid-turn", where, key, *want)
	case want != nil && got != nil && *got != *want:
		t.Errorf("%s: %s = %v, want %v (the host's own bound)", where, key, *got, *want)
	}
}

func assertStringSet(t *testing.T, where, key string, got, want []string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, v := range got {
		gotSet[v] = true
	}
	wantSet := map[string]bool{}
	for _, v := range want {
		wantSet[v] = true
	}
	for v := range wantSet {
		if !gotSet[v] {
			t.Errorf("%s: %s is missing %q", where, key, v)
		}
	}
	for v := range gotSet {
		if !wantSet[v] {
			t.Errorf("%s: %s has unexpected %q", where, key, v)
		}
	}
}
