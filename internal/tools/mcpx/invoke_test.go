package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/tools"
)

// invokeCatalog is a live-session catalog: a classified read, a classified
// mutation, an action with a typed wrapper, and an action this repo has never
// classified. Everything absent from it is, by definition, not eligible for the
// session — which is how a hidden or restricted action is modelled.
func invokeCatalog() []MCPToolInfo {
	objSchema := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	// InputSchemaProvided is true throughout: these model a server that ADVERTISED
	// each schema. The substituted-default case has its own test.
	return []MCPToolInfo{
		{Name: "terminal.list", Description: "List terminals.", InputSchemaProvided: true,
			InputSchema: objSchema(map[string]any{})},
		{Name: "terminal.new", Description: "Open a terminal.", InputSchemaProvided: true,
			InputSchema: objSchema(map[string]any{
				"cwd":  map[string]any{"type": "string"},
				"name": map[string]any{"type": "string"},
			}, "cwd")},
		{Name: "terminal.rename", Description: "Rename a terminal.", InputSchemaProvided: true,
			InputSchema: objSchema(map[string]any{"terminalId": map[string]any{"type": "string"}})},
		{Name: "vendor.mysteryAction", Description: "Something this build has never classified.",
			InputSchemaProvided: true, InputSchema: objSchema(map[string]any{})},
	}
}

func invokeDeps() (*fakeMCP, Deps) {
	m := &fakeMCP{connected: true, toolList: invokeCatalog(), result: MCPCallResult{Text: "done"}}
	return m, Deps{MCP: m}
}

// runInvoke drives the tool the way dispatch does: Decode, then the resolver (the
// gate), then Handle. Returning the resolver's TargetInfo lets a test assert on
// the identity/risk dispatch would have gated at.
func runInvoke(t *testing.T, deps Deps, raw string) (tools.TargetInfo, *tools.ToolResult, tools.ToolResult) {
	t.Helper()
	tool := newInvokeTool(deps)
	decoded, err := tool.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	target, refusal := tool.ResolveTarget(context.Background(), decoded, &tools.ToolContext{})
	if refusal != nil {
		return target, refusal, tools.ToolResult{}
	}
	return target, nil, tool.Handle(context.Background(), decoded, &tools.ToolContext{})
}

func requireRefusal(t *testing.T, refusal *tools.ToolResult, code string) *tools.ToolResult {
	t.Helper()
	if refusal == nil {
		t.Fatalf("expected a %s refusal, got none", code)
	}
	if refusal.Error == nil || refusal.Error.Code != code {
		t.Fatalf("expected %s, got %+v", code, refusal.Error)
	}
	return refusal
}

/* -------------------------------- read path ------------------------------- */

// A classified read resolves at read risk, which is what makes it run with no
// confirmation at all — the acceptance criterion the whole feature exists for.
func TestInvokeReadResolvesAtReadRisk(t *testing.T) {
	m, deps := invokeDeps()
	target, refusal, res := runInvoke(t, deps, `{"action":"terminal.list"}`)
	if refusal != nil {
		t.Fatalf("read must not be refused: %+v", refusal.Error)
	}
	if target.Risk != domain.RiskRead {
		t.Errorf("risk = %q, want read", target.Risk)
	}
	if safety.AlwaysConfirm(target.Risk) {
		t.Error("a read target must not require confirmation")
	}
	if target.Name != "daintree.invoke:terminal.list" {
		t.Errorf("identity = %q", target.Name)
	}
	if target.Display != "terminal.list" {
		t.Errorf("display = %q, want the raw action", target.Display)
	}
	if !res.Ok {
		t.Fatalf("handler failed: %+v", res.Error)
	}
	if m.lastName != "terminal.list" {
		t.Errorf("forwarded %q, want terminal.list", m.lastName)
	}
}

// The catalog read must be CACHE-first: a resolver that forced a refetch would
// make every gated call pay a round-trip, twice (resolver + handler).
func TestInvokeReadsCatalogFromCache(t *testing.T) {
	m, deps := invokeDeps()
	if _, refusal, _ := runInvoke(t, deps, `{"action":"terminal.list"}`); refusal != nil {
		t.Fatalf("unexpected refusal: %+v", refusal.Error)
	}
	// EVERY read, not just the last: the resolver and the handler each read the
	// catalog, and checking only lastListForce would let a forcing resolver hide
	// behind a cache-reading handler.
	if m.listCount < 2 {
		t.Fatalf("expected the resolver and the handler each to read the catalog, got %d reads", m.listCount)
	}
	for i, force := range m.listForces {
		if force {
			t.Errorf("catalog read %d used force=true; every read must be cache-first", i)
		}
	}
}

/* ------------------------------ mutation path ----------------------------- */

// A classified mutation resolves at its real risk — confirmed, and previewed with
// the ACTION rather than the invoker.
func TestInvokeMutationResolvesAtTargetRisk(t *testing.T) {
	_, deps := invokeDeps()
	target, refusal, res := runInvoke(t, deps, `{"action":"terminal.new","arguments":{"cwd":"/tmp"}}`)
	if refusal != nil {
		t.Fatalf("classified mutation must resolve: %+v", refusal.Error)
	}
	if target.Risk != domain.RiskTerminal {
		t.Errorf("risk = %q, want terminal", target.Risk)
	}
	if !safety.AlwaysConfirm(target.Risk) {
		t.Error("a terminal-risk target must require confirmation")
	}
	if safety.NeedsTypedConfirm(target.Risk) {
		t.Error("a terminal-risk target must not inherit the invoker's typed-confirm requirement")
	}
	if !strings.Contains(target.Consequence, "terminal.new") {
		t.Errorf("the approval preview must name the action, got %q", target.Consequence)
	}
	if !res.Ok {
		t.Fatalf("handler failed: %+v", res.Error)
	}
}

// requestKey must reach the wire — it is how Daintree dedupes an autonomous
// mutation, and dropping it silently turns a retry into a double-execution.
func TestInvokeForwardsRequestKey(t *testing.T) {
	m, deps := invokeDeps()
	_, refusal, res := runInvoke(t, deps, `{"action":"terminal.new","arguments":{"cwd":"/tmp"},"requestKey":"req-42"}`)
	if refusal != nil || !res.Ok {
		t.Fatalf("expected success, refusal=%+v res=%+v", refusal, res.Error)
	}
	if got := m.lastArgs["requestKey"]; got != "req-42" {
		t.Errorf("requestKey = %v, want req-42", got)
	}
	if got := m.lastArgs["cwd"]; got != "/tmp" {
		t.Errorf("declared arguments must still be forwarded, got %v", got)
	}
}

/* ------------------------------- hard ceilings ---------------------------- */

// An action absent from the live catalog is hidden, restricted, or simply not in
// this build. All three are the same answer: not invocable, and no argument makes
// it so.
func TestInvokeRefusesActionOutsideSessionCatalog(t *testing.T) {
	m, deps := invokeDeps()
	// Classified through the HOST seam rather than by mutating the reviewed global
	// map: a test that writes package state is one `t.Parallel()` away from a
	// concurrent-map fault, and the reviewed catalog is the last thing that should be
	// writable from a test. This also proves the refusal comes from the eligibility
	// gate rather than from the policy gate.
	deps.Policy = hostSource{"test.hiddenAction": {Risk: domain.RiskTerminal, Danger: "confirm"}}

	_, refusal, _ := runInvoke(t, deps, `{"action":"test.hiddenAction"}`)
	requireRefusal(t, refusal, codeActionUnavailable)
	if m.callCount != 0 {
		t.Error("an ineligible action must never reach the MCP layer")
	}
}

// An unclassified action fails closed: refused here, and explicitly handed to
// daintree.call, whose system-tier typed confirmation is a strictly higher bar
// than any classification this tool could have invented.
func TestInvokeRefusesPolicyUnknownAction(t *testing.T) {
	m, deps := invokeDeps()
	_, refusal, _ := runInvoke(t, deps, `{"action":"vendor.mysteryAction"}`)
	res := requireRefusal(t, refusal, codePolicyUnknown)
	if !strings.Contains(res.Error.Message, "daintree.call") {
		t.Errorf("the refusal must name the fail-closed fallback, got %q", res.Error.Message)
	}
	if m.callCount != 0 {
		t.Error("an unclassified action must never reach the MCP layer")
	}
}

// A typed wrapper always wins: routing around it loses its identifier resolution,
// watcher attachment and strict decoding.
func TestInvokeRedirectsToTypedWrapper(t *testing.T) {
	// panel.focus → terminal.focus is the load-bearing case: the wrapper has a
	// DIFFERENT name from the raw action, so asserting the message names the wrapper
	// actually proves a lookup happened. A same-named pair (terminal.rename) would
	// pass on an implementation that merely echoed the requested action back.
	m := &fakeMCP{connected: true, result: MCPCallResult{Text: "done"}, toolList: []MCPToolInfo{
		{Name: "panel.focus", Description: "Focus a panel.", InputSchemaProvided: true,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	}}
	deps := Deps{MCP: m}
	_, refusal, _ := runInvoke(t, deps, `{"action":"panel.focus"}`)
	res := requireRefusal(t, refusal, codeUseTypedWrapper)
	if !strings.Contains(res.Error.Message, "terminal.focus") {
		t.Errorf("the redirect must name the WRAPPER (terminal.focus), got %q", res.Error.Message)
	}
	if m.callCount != 0 {
		t.Error("a wrapped action must never reach the MCP layer")
	}

	// Discovery must give the same answer, and must NOT send the model to
	// daintree.call — which denylists the same name and would refuse it too.
	row := discoveryRow(deps, m.toolList[0], func(string) bool { return true })
	if row["preferredTool"] != "terminal.focus" {
		t.Errorf("preferredTool = %v, want terminal.focus", row["preferredTool"])
	}
	if row["invocable"] != false {
		t.Errorf("a wrapped action must be reported non-invocable, got %+v", row)
	}
	reason, _ := row["unavailableReason"].(string)
	if !strings.Contains(reason, "terminal.focus") {
		t.Errorf("the reason must name the wrapper, got %q", reason)
	}
	// It may MENTION daintree.call — to say it will not forward this name either —
	// but it must never present it as the route, which is the dead end the old
	// branch ordering produced.
	if strings.Contains(reason, "reachable only through daintree.call") {
		t.Errorf("a wrapped action must not be redirected to daintree.call, which refuses it too: %q", reason)
	}
}

// The redirect is normalization-proof, exactly as daintree.call's is: a padded or
// case-shifted spelling must not slip past into the raw forward.
func TestInvokeRedirectResistsNameEvasion(t *testing.T) {
	for _, name := range []string{"Terminal.Rename", "  terminal.rename  ", "terminal.re name"} {
		m, deps := invokeDeps()
		raw, _ := json.Marshal(map[string]any{"action": name})
		tool := newInvokeTool(deps)
		// Padding is rejected by Decode; the rest must be rejected by the resolver.
		decoded, err := tool.Decode(raw)
		if err != nil {
			continue
		}
		_, refusal := tool.ResolveTarget(context.Background(), decoded, &tools.ToolContext{})
		if refusal == nil {
			t.Errorf("%q resolved instead of being refused", name)
			continue
		}
		if m.callCount != 0 {
			t.Errorf("%q reached the MCP layer", name)
		}
	}
}

// The no-file-edit invariant is a property of the process, not of the catalog, so
// it is re-checked on the raw forwarded name here exactly as daintree.call does.
func TestInvokeRefusesFileEditNames(t *testing.T) {
	m, deps := invokeDeps()
	_, refusal, _ := runInvoke(t, deps, `{"action":"fs.write_file"}`)
	requireRefusal(t, refusal, safety.FileEditForbiddenCode)
	if m.callCount != 0 {
		t.Error("a file-edit name must never reach the MCP layer")
	}
}

// A disconnected link refuses rather than resolving optimistically.
func TestInvokeRefusesWhenDisconnected(t *testing.T) {
	m, deps := invokeDeps()
	m.connected = false
	_, refusal, _ := runInvoke(t, deps, `{"action":"terminal.list"}`)
	requireRefusal(t, refusal, codeMCPUnavailable)
}

// ...but a wrapper redirect and a policy refusal hold WITHOUT the link, because
// neither depends on this session: answering "not connected" for a call that was
// wrong anyway sends the model to /reconnect and then straight back into the same
// mistake.
func TestInvokeConnectivityIndependentRefusals(t *testing.T) {
	for _, tc := range []struct{ action, code string }{
		{"terminal.rename", codeUseTypedWrapper},
		{"vendor.mysteryAction", codePolicyUnknown},
	} {
		m, deps := invokeDeps()
		m.connected = false
		_, refusal, _ := runInvoke(t, deps, `{"action":"`+tc.action+`"}`)
		requireRefusal(t, refusal, tc.code)
	}
}

/* --------------------------- argument validation -------------------------- */

// Arguments are validated against the ACTION's own schema, before anything is
// invoked or confirmed.
func TestInvokeRejectsArgumentsFailingTargetSchema(t *testing.T) {
	m, deps := invokeDeps()
	// terminal.new requires `cwd`.
	_, refusal, _ := runInvoke(t, deps, `{"action":"terminal.new","arguments":{"name":"x"}}`)
	res := requireRefusal(t, refusal, codeArgsSchemaInvalid)
	if !strings.Contains(res.Error.Message, "cwd") {
		t.Errorf("the failure must name the missing constraint, got %q", res.Error.Message)
	}
	if m.callCount != 0 {
		t.Error("invalid arguments must never reach the MCP layer")
	}
	// And a type mismatch, not just a missing field.
	m2, deps2 := invokeDeps()
	_, refusal2, _ := runInvoke(t, deps2, `{"action":"terminal.new","arguments":{"cwd":5}}`)
	requireRefusal(t, refusal2, codeArgsSchemaInvalid)
	if m2.callCount != 0 {
		t.Error("a type mismatch must never reach the MCP layer")
	}
}

// A no-argument action must accept an absent `arguments` field: an omitted object
// has to validate exactly as `{}` does, or every argument-free read would fail its
// own "type":"object" check.
func TestInvokeAcceptsOmittedArgumentsForArgFreeAction(t *testing.T) {
	_, deps := invokeDeps()
	_, refusal, res := runInvoke(t, deps, `{"action":"terminal.list"}`)
	if refusal != nil {
		t.Fatalf("an argument-free action must resolve: %+v", refusal.Error)
	}
	if !res.Ok {
		t.Fatalf("handler failed: %+v", res.Error)
	}
}

// A schema we cannot use is a refusal, never a skipped check — otherwise a broken
// or hostile schema becomes the easiest way to turn the validated path back into
// an unvalidated one.
func TestInvokeRefusesUnusableTargetSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema map[string]any
		code   string
	}{
		// A server that published no schema, an empty map, and an uncompilable schema
		// are all SERVER METADATA failures — not argument mismatches. The distinction
		// is load-bearing for the model: no change to `arguments` can fix any of them,
		// so a retry-the-args code would send it round a loop that cannot terminate.
		{"missing", nil, codeSchemaInvalid},
		{"empty", map[string]any{}, codeSchemaInvalid},
		{"uncompilable", map[string]any{"type": 17}, codeSchemaInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeMCP{connected: true, result: MCPCallResult{Text: "done"},
				toolList: []MCPToolInfo{{Name: "terminal.list", Description: "d",
					InputSchema: tc.schema, InputSchemaProvided: tc.schema != nil}}}
			_, refusal, _ := runInvoke(t, Deps{MCP: m}, `{"action":"terminal.list"}`)
			requireRefusal(t, refusal, tc.code)
			if m.callCount != 0 {
				t.Error("an unusable schema must never reach the MCP layer")
			}
		})
	}
}

// THE fail-open this gate exists for. When a server advertises no inputSchema the
// MCP client substitutes {"type":"object","properties":{}} — a NON-EMPTY map that a
// JSON Schema validator accepts every object against. Keying the refusal off map
// length alone would therefore have turned "this action published no contract" into
// "this action permits anything", with validation still nominally "on".
func TestInvokeRefusesServerSubstitutedDefaultSchema(t *testing.T) {
	substituted := map[string]any{"type": "object", "properties": map[string]any{}}
	m := &fakeMCP{connected: true, result: MCPCallResult{Text: "done"},
		toolList: []MCPToolInfo{{Name: "terminal.list", Description: "d",
			InputSchema: substituted, InputSchemaProvided: false}}}
	// Arguments that this placeholder would happily accept.
	_, refusal, _ := runInvoke(t, Deps{MCP: m}, `{"action":"terminal.list","arguments":{"anything":"goes"}}`)
	requireRefusal(t, refusal, codeSchemaInvalid)
	if m.callCount != 0 {
		t.Error("an unchecked action must never reach the MCP layer")
	}
	// ...and discovery must say so up front rather than making the model find out.
	row := discoveryRow(Deps{MCP: m}, m.toolList[0], func(string) bool { return true })
	if row["invocable"] != false {
		t.Errorf("a schema-less action must be reported non-invocable, got %+v", row)
	}
}

// requestKey is a transport field. Accepting it inside `arguments` too would mean
// the object validated is not the object sent — the outer value overwrites the
// inner one after validation has already passed.
func TestInvokeRejectsRequestKeyInsideArguments(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		// Both present: the outer value would silently overwrite the validated inner one.
		{"inner and outer", `{"action":"terminal.new","arguments":{"cwd":"/tmp","requestKey":"inner"},"requestKey":"outer"}`},
		// Inner ONLY is the subtler case and must be refused too: nothing overwrites it,
		// but it is still a transport key smuggled through the argument object, where
		// the action's own schema never agreed to accept it.
		{"inner only", `{"action":"terminal.new","arguments":{"cwd":"/tmp","requestKey":"inner"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, deps := invokeDeps()
			_, refusal, _ := runInvoke(t, deps, tc.args)
			requireRefusal(t, refusal, codeArgsSchemaInvalid)
			if m.callCount != 0 {
				t.Error("a requestKey inside arguments must never reach the MCP layer")
			}
		})
	}
}

// A catalog advertising one name twice with DIFFERENT schemas has no knowable
// contract, so the invoker refuses rather than picking one.
func TestInvokeRefusesAmbiguousCatalogDuplicate(t *testing.T) {
	m := &fakeMCP{connected: true, result: MCPCallResult{Text: "done"}, toolList: []MCPToolInfo{
		{Name: "terminal.list", Description: "d", InputSchemaProvided: true,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "terminal.list", Description: "d", InputSchemaProvided: true,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
	}}
	_, refusal, _ := runInvoke(t, Deps{MCP: m}, `{"action":"terminal.list"}`)
	res := requireRefusal(t, refusal, codeSchemaInvalid)
	if res.Error.Recoverable {
		t.Error("an ambiguous catalog entry is not fixable by retrying")
	}
	if m.callCount != 0 {
		t.Error("an ambiguous action must never reach the MCP layer")
	}
}

// An oversized schema is refused rather than compiled: it is unbounded input from
// whatever server DAINTREE_MCP_URL points at.
func TestInvokeRefusesOversizedSchema(t *testing.T) {
	props := map[string]any{}
	for i := 0; len(props) < 5000; i++ {
		props[fmt.Sprintf("property_with_a_fairly_long_name_%06d", i)] = map[string]any{"type": "string"}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if raw, _ := json.Marshal(schema); len(raw) <= maxInvokeSchemaBytes {
		t.Fatalf("fixture is only %d bytes; it must exceed the %d cap", len(raw), maxInvokeSchemaBytes)
	}
	m := &fakeMCP{connected: true, result: MCPCallResult{Text: "done"},
		toolList: []MCPToolInfo{{Name: "terminal.list", Description: "d", InputSchema: schema, InputSchemaProvided: true}}}
	_, refusal, _ := runInvoke(t, Deps{MCP: m}, `{"action":"terminal.list"}`)
	requireRefusal(t, refusal, codeSchemaTooLarge)
	if m.callCount != 0 {
		t.Error("an oversized schema must never reach the MCP layer")
	}
}

// clampErr must stay inside its bound INCLUDING the marker, and must keep the tail
// — the validator puts the outer path first and the actual violated rule last, so a
// head-only clamp drops the answer on exactly the schemas that need it most.
func TestClampErrKeepsBothEndsWithinBudget(t *testing.T) {
	long := strings.Repeat("validating deeply.nested.path: ", 200) + "required: missing properties: [\"cwd\"]"
	got := clampErr(long)
	if n := utf8.RuneCountInString(got); n > maxValidationErrChars {
		t.Errorf("clamped message is %d runes, over the %d budget", n, maxValidationErrChars)
	}
	if !strings.HasSuffix(got, `required: missing properties: ["cwd"]`) {
		t.Errorf("the violated rule (the tail) must survive, got %q", got)
	}
	if !strings.HasPrefix(got, "validating") {
		t.Errorf("the failing path (the head) must survive, got %q", got)
	}
	// A message already inside the budget is returned untouched.
	if short := "required: missing properties"; clampErr(short) != short {
		t.Errorf("a short message must not be altered, got %q", clampErr(short))
	}
}

// A `$ref` pointing off-host must fail to resolve rather than being fetched: the
// schema comes from whatever DAINTREE_MCP_URL points at, and compiling it must
// never make this process issue an outbound request.
func TestInvokeDoesNotFetchRemoteRefs(t *testing.T) {
	// A REAL server, so "it failed" cannot be confused with "the host did not
	// resolve". A future loader that tried the fetch would be caught by the counter
	// even though the refusal code would look identical.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"type":"object"}`))
	}))
	defer srv.Close()

	m := &fakeMCP{connected: true, toolList: []MCPToolInfo{{
		Name: "terminal.list", Description: "d", InputSchemaProvided: true,
		InputSchema: map[string]any{"$ref": srv.URL + "/schema.json"},
	}}}
	_, refusal, _ := runInvoke(t, Deps{MCP: m}, `{"action":"terminal.list"}`)
	requireRefusal(t, refusal, codeSchemaInvalid)
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("compiling a target schema made %d outbound request(s); it must make none", n)
	}
}

/* ------------------------------ policy catalog ---------------------------- */

// Fail-closed is the catalog's whole contract: an unknown name is UNKNOWN, never
// the zero-value risk, which would read as a permissive default.
func TestResolveTargetPolicyFailsClosed(t *testing.T) {
	p := ResolveTargetPolicy(nil, "never.classified")
	if p.Known {
		t.Fatal("an unclassified action must not be Known")
	}
	if p.Risk.IsValid() {
		t.Errorf("an unclassified action must carry no valid risk, got %q", p.Risk)
	}
}

// Daintree's three always-confirm dangerous actions must stay unclassified, so
// they can never be reached dynamically at all — and so must agentSettings.get,
// which is a workbench-tier "safe" READ whose result carries preset environment
// configuration including provider auth tokens. Read-only bounds what an action
// writes, not what it discloses, and a no-confirm classification would let the
// model pull credentials into the conversation with nobody asked.
func TestDangerousActionsAreNotDynamicallyClassified(t *testing.T) {
	excluded := []string{"git.commit", "git.push", "worktree.delete", "terminal.kill", "agentSettings.get"}
	for _, name := range excluded {
		if p := ResolveTargetPolicy(nil, name); p.Known {
			t.Errorf("%s must not be dynamically invocable (got risk %q)", name, p.Risk)
		}
	}
	// And a hostile — or merely newer — host cannot re-enable them. Absence from the
	// local catalog only means "unclassified", and the host seam classifies
	// unclassified names; the exclusion has to be enforcement, not prose.
	hostile := hostSource{}
	for _, name := range excluded {
		hostile[name] = TargetPolicy{Risk: domain.RiskTerminal, Danger: "confirm"}
	}
	for _, name := range excluded {
		if p := ResolveTargetPolicy(hostile, name); p.Known {
			t.Errorf("a host source re-enabled %s (risk %q) — the exclusion must be absolute", name, p.Risk)
		}
	}
}

// No classified action may also have a typed wrapper: that would be two routes to
// the same call with two different policies, and the invoker would redirect away
// from a classification nobody could ever use.
func TestClassifiedActionsHaveNoTypedWrapper(t *testing.T) {
	for _, name := range ClassifiedActionNames() {
		if w := preferredWrapperFor(name); w != "" {
			t.Errorf("%s is classified AND wrapped (%s) — remove the classification", name, w)
		}
	}
}

// The two exclusion mechanisms must not contradict each other. neverDynamic is
// checked first, so an action in both would be silently unreachable while
// localTargetPolicies claimed — and documented — that it was invocable. That is a
// lie in the one file whose entire job is to be the reviewed truth.
func TestNeverDynamicAndCatalogAreDisjoint(t *testing.T) {
	for name := range neverDynamic {
		if _, dup := localTargetPolicies[name]; dup {
			t.Errorf("%s is in BOTH neverDynamic and localTargetPolicies — the classification is dead and misleading", name)
		}
	}
	// And every hard exclusion must actually resolve to unknown, whatever the route.
	for name := range neverDynamic {
		if ResolveTargetPolicy(nil, name).Known {
			t.Errorf("%s is in neverDynamic but still resolves as known", name)
		}
	}
}

// Every local mcpx tool that shares its name with a raw MCP action must be on the
// daintree.call denylist. The two indexes have drifted repeatedly — copyTree.generate
// and both worktree reads each had a wrapper for a while with the raw forward still
// open beside it — and a raw route around a typed wrapper is precisely what the
// denylist exists to close.
func TestLocalWrappersAreDenylistedForRawCalls(t *testing.T) {
	// Names that are NOT raw MCP actions: they are this family's own tools, so there
	// is no raw forward to deny.
	localOnly := map[string]bool{
		"daintree.status": true, "daintree.listTools": true, "tool.search": true,
		"tool.schema": true, "daintree.call": true, "daintree.invoke": true,
		// terminal.focus wraps the raw panel.focus, which IS denylisted.
		"terminal.focus": true,
	}
	for name := range getLocalWrapperNames() {
		if localOnly[name] {
			continue
		}
		if _, denied := denylistLookup[normalizeMCPName(name)]; !denied {
			t.Errorf("%s is a typed wrapper over a raw MCP action but daintree.call would still forward it raw", name)
		}
	}
}

// Every classified risk must be one the tier matrix actually knows, or
// requiredTier would report "" and the discovery result would advertise a tier
// that cannot be selected.
func TestClassifiedRisksAreValidAndTierable(t *testing.T) {
	for _, name := range ClassifiedActionNames() {
		p := ResolveTargetPolicy(nil, name)
		if !p.Risk.IsValid() {
			t.Errorf("%s has invalid risk %q", name, p.Risk)
		}
		if p.RequiredTier() == "" {
			t.Errorf("%s (risk %q) has no tier that permits it", name, p.Risk)
		}
	}
}

// hostSource is a stand-in for the manifest daintreehq/daintree#11910 will supply.
type hostSource map[string]TargetPolicy

func (h hostSource) Lookup(action string) (TargetPolicy, bool) { p, ok := h[action]; return p, ok }

// The host seam ADDS classifications and never overrides a reviewed one — the
// supervised process must not get to choose the policy it is supervised under.
func TestHostPolicySourceAddsButNeverOverrides(t *testing.T) {
	host := hostSource{
		"terminal.list": {Risk: domain.RiskSystem}, // an attempt to re-classify
		// A genuine addition. It must be a CONFIRMING risk: the clamp below refuses a
		// host that claims an unknown action is safe.
		"host.onlyAction": {Risk: domain.RiskTerminal},
	}
	if p := ResolveTargetPolicy(host, "terminal.list"); p.Risk != domain.RiskRead || p.Source != "local" {
		t.Errorf("the host must not override a local classification, got %+v", p)
	}
	if p := ResolveTargetPolicy(host, "host.onlyAction"); !p.Known || p.Source != "host" {
		t.Errorf("the host must be able to add a classification, got %+v", p)
	}
	// A host entry with a nonsense risk is not a classification.
	bad := hostSource{"host.bogus": {Risk: domain.RiskClass("nope")}}
	if p := ResolveTargetPolicy(bad, "host.bogus"); p.Known {
		t.Error("an invalid host risk must fail closed to unknown")
	}
	// A host may tell us an action needs MORE care; it may not tell us one is safe.
	// The channel is unauthenticated and the host is the process being driven, so a
	// no-confirm assertion from it would let a compromised or merely newer Daintree
	// turn every unknown action into a silent read.
	permissive := hostSource{
		"host.claimsRead":  {Risk: domain.RiskRead},
		"host.claimsLocal": {Risk: domain.RiskLocal},
		"host.claimsUI":    {Risk: domain.RiskUI},
	}
	for _, name := range []string{"host.claimsRead", "host.claimsLocal", "host.claimsUI"} {
		if p := ResolveTargetPolicy(permissive, name); p.Known {
			t.Errorf("the host must not be able to classify %s as no-confirm (got %q)", name, p.Risk)
		}
	}
	// And an absent host source (every install today) changes nothing.
	if p := ResolveTargetPolicy(nil, "host.onlyAction"); p.Known {
		t.Error("without a host source an unclassified action must stay unknown")
	}
}

/* --------------------------- authorization drift -------------------------- */

// flippingPolicy returns a different risk on each Lookup — the shape a stateful or
// refreshing host manifest could take once daintree#11910 ships.
type flippingPolicy struct {
	risks []domain.RiskClass
	n     int
}

func (f *flippingPolicy) Lookup(_ string) (TargetPolicy, bool) {
	r := f.risks[f.n]
	if f.n < len(f.risks)-1 {
		f.n++
	}
	return TargetPolicy{Risk: r, Danger: "confirm"}, true
}

// The resolver gates one policy and the handler re-derives another. Everything
// derived from the (immutable) arguments necessarily agrees, but the host source is
// not immutable — so without the gated-target comparison the call would be
// authorized as a terminal-risk mutation and then executed as an external one,
// with the audit still recording the risk that was approved.
func TestInvokeRefusesPolicyDriftBetweenGateAndExecution(t *testing.T) {
	m, deps := invokeDeps()
	deps.Policy = &flippingPolicy{risks: []domain.RiskClass{domain.RiskTerminal, domain.RiskExternal}}

	tool := newInvokeTool(deps)
	raw := json.RawMessage(`{"action":"vendor.mysteryAction"}`)
	decoded, err := tool.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	target, refusal := tool.ResolveTarget(context.Background(), decoded, &tools.ToolContext{})
	if refusal != nil {
		t.Fatalf("first resolution must succeed: %+v", refusal.Error)
	}
	if target.Risk != domain.RiskTerminal {
		t.Fatalf("gated risk = %q, want terminal", target.Risk)
	}
	// Dispatch stamps what it gated; the handler must compare against it.
	gated := target
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{GatedTarget: &gated})
	if res.Ok {
		t.Fatal("a call whose policy changed after authorization must not run")
	}
	if res.Error.Code != codePolicyDrift {
		t.Fatalf("expected %s, got %+v", codePolicyDrift, res.Error)
	}
	if m.callCount != 0 {
		t.Error("a drifted call must never reach the MCP layer")
	}
}

// The comparison must not fire on the normal path, or every dynamic call breaks.
func TestInvokeAcceptsStablePolicyAcrossGateAndExecution(t *testing.T) {
	m, deps := invokeDeps()
	tool := newInvokeTool(deps)
	decoded, err := tool.Decode(json.RawMessage(`{"action":"terminal.list"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	target, refusal := tool.ResolveTarget(context.Background(), decoded, &tools.ToolContext{})
	if refusal != nil {
		t.Fatalf("resolution failed: %+v", refusal.Error)
	}
	gated := target
	if res := tool.Handle(context.Background(), decoded, &tools.ToolContext{GatedTarget: &gated}); !res.Ok {
		t.Fatalf("a stable policy must run: %+v", res.Error)
	}
	if m.lastName != "terminal.list" {
		t.Errorf("forwarded %q", m.lastName)
	}
}

/* ------------------------------- discovery -------------------------------- */

// Search and listTools must carry a complete invocation contract per row, and
// must agree with each other and with tool.schema — one policyBlock, three
// callers.
func TestDiscoveryRowsCarryPolicy(t *testing.T) {
	_, deps := invokeDeps()
	search := newSearchTool(deps)
	dec, err := search.Decode(json.RawMessage(`{"query":"terminal"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := search.Handle(context.Background(), dec, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("search failed: %+v", res.Error)
	}
	rows := map[string]map[string]any{}
	for _, m := range res.Result.(map[string]any)["matches"].([]map[string]any) {
		rows[m["name"].(string)] = m
	}

	read := rows["terminal.list"]
	if read["risk"] != string(domain.RiskRead) || read["invocable"] != true {
		t.Errorf("terminal.list row = %+v", read)
	}
	if read["requiredTier"] != string(domain.TierSupervisor) {
		t.Errorf("a read must report the lowest tier, got %v", read["requiredTier"])
	}
	if read["confirms"] != false {
		t.Errorf("a read must not report confirms, got %v", read["confirms"])
	}

	mut := rows["terminal.new"]
	if mut["risk"] != string(domain.RiskTerminal) || mut["confirms"] != true {
		t.Errorf("terminal.new row = %+v", mut)
	}
	if mut["requiredTier"] != string(domain.TierOperator) {
		t.Errorf("terminal.new requiredTier = %v", mut["requiredTier"])
	}

	// A wrapped action names its wrapper AND is not invocable here.
	wrapped := rows["terminal.rename"]
	if wrapped["preferredTool"] != "terminal.rename" {
		t.Errorf("preferredTool = %v", wrapped["preferredTool"])
	}
	if wrapped["invocable"] != false || wrapped["unavailableReason"] == nil {
		t.Errorf("a wrapped action must be reported non-invocable with a reason: %+v", wrapped)
	}

	// An unclassified action is NAMED — diagnostics are useful — but never invocable.
	// It is fetched with its own query because the one above deliberately does not
	// match it, which is itself worth keeping: the policy block must not leak into
	// the ranking.
	unkDec, err := search.Decode(json.RawMessage(`{"query":"mystery"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	unkRes := search.Handle(context.Background(), unkDec, &tools.ToolContext{})
	unkMatches := unkRes.Result.(map[string]any)["matches"].([]map[string]any)
	if len(unkMatches) != 1 {
		t.Fatalf("an unclassified action must still be discoverable, got %d matches", len(unkMatches))
	}
	unknown := unkMatches[0]
	if unknown["policySource"] != "unknown" || unknown["invocable"] != false {
		t.Errorf("unclassified row = %+v", unknown)
	}
	if unknown["risk"] != nil {
		t.Errorf("an unclassified action must advertise no risk, got %v", unknown["risk"])
	}
}

// The three surfaces must describe an action IDENTICALLY. They share one
// policyBlock today, and this is what stops a future caller from growing its own
// slightly different answer — the failure mode where search says a thing is
// invocable and schema says it is not.
func TestDiscoverySurfacesAgree(t *testing.T) {
	_, deps := invokeDeps()
	policyFields := []string{"policySource", "risk", "requiredTier", "confirms", "danger", "preferredTool",
		"invocable", "unavailableReason"}

	list := newListToolsTool(deps)
	listDec, err := list.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("decode listTools: %v", err)
	}
	listRes := list.Handle(context.Background(), listDec, &tools.ToolContext{})
	listRows := map[string]map[string]any{}
	for _, r := range listRes.Result.(map[string]any)["tools"].([]map[string]any) {
		listRows[r["name"].(string)] = r
	}

	search := newSearchTool(deps)
	schemaTool := newSchemaTool(deps)
	for _, action := range []string{"terminal.list", "terminal.new", "terminal.rename", "vendor.mysteryAction"} {
		searchDec, err := search.Decode(json.RawMessage(`{"query":"` + action + `"}`))
		if err != nil {
			t.Fatalf("decode search %s: %v", action, err)
		}
		searchRes := search.Handle(context.Background(), searchDec, &tools.ToolContext{})
		matches := searchRes.Result.(map[string]any)["matches"].([]map[string]any)
		if len(matches) != 1 {
			t.Fatalf("%s: expected exactly one search match, got %d", action, len(matches))
		}
		schemaDec, err := schemaTool.Decode(json.RawMessage(`{"name":"` + action + `"}`))
		if err != nil {
			t.Fatalf("decode schema %s: %v", action, err)
		}
		schemaRes := schemaTool.Handle(context.Background(), schemaDec, &tools.ToolContext{})
		if !schemaRes.Ok {
			t.Fatalf("%s: tool.schema failed: %+v", action, schemaRes.Error)
		}
		schemaPolicy := schemaRes.Result.(map[string]any)["policy"].(map[string]any)

		for _, f := range policyFields {
			if got, want := matches[0][f], listRows[action][f]; got != want {
				t.Errorf("%s: tool.search %s = %v, daintree.listTools = %v", action, f, got, want)
			}
			if got, want := schemaPolicy[f], listRows[action][f]; got != want {
				t.Errorf("%s: tool.schema %s = %v, daintree.listTools = %v", action, f, got, want)
			}
		}
	}
}

// tool.schema is the same contract from the other side: the exact schema the
// invoker validates against, plus the identical policy block.
func TestSchemaLookupCarriesPolicyAndSchema(t *testing.T) {
	_, deps := invokeDeps()
	tool := newSchemaTool(deps)
	dec, err := tool.Decode(json.RawMessage(`{"name":"terminal.new"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), dec, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("tool.schema failed: %+v", res.Error)
	}
	out := res.Result.(map[string]any)
	schema, ok := out["inputSchema"].(map[string]any)
	if !ok || schema["required"] == nil {
		t.Fatalf("the exact target schema must be returned verbatim, got %v", out["inputSchema"])
	}
	policy, ok := out["policy"].(map[string]any)
	if !ok {
		t.Fatalf("policy block missing: %v", out)
	}
	if policy["risk"] != string(domain.RiskTerminal) || policy["invocable"] != true {
		t.Errorf("policy = %+v", policy)
	}
}
