package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

/* ------------------------------- tool.schema ------------------------------ */

// tool.schema closes the gap that made #311 unrecoverable: `tool.search` and
// `daintree.listTools` report a tool's NAME and PROSE but never its argument
// shape, so a model that guessed an argument wrong had nowhere to look it up.
// Its only escape was daintree.listTools, whose result is large enough to become
// a paged artifact (3500 chars/page) — and the schema was never in it anyway.
//
// The schema comes from the LOCAL cached catalog (deps.MCP.ListTools with
// force=false), never from a network probe. That source is wire-authoritative
// (it is literally what the server advertised), covers every connected tool
// rather than just Daintree's action registry, and normally costs nothing. We
// deliberately do NOT fall back to Daintree's actions.getSchema when the cached
// schema is the empty default: the cache cannot distinguish "the server omitted
// a schema" from "the server advertised an empty object", so such a fallback
// would fire unpredictably.
//
// The schema is returned VERBATIM. We never flatten it to {key,type,required}:
// summarizing nested objects, oneOf/anyOf, or conditional requirements misleads
// the model about which combinations are legal — precisely the class of mistake
// this tool exists to prevent.

const (
	codeSchemaTooLarge = "MCP_SCHEMA_TOO_LARGE"
	codeSchemaInvalid  = "MCP_SCHEMA_INVALID"
	codeToolNotFound   = "MCP_TOOL_NOT_FOUND"
)

// maxSchemaNameLen bounds the requested name. MCP tool names are short dotted
// identifiers; anything longer is a malformed argument, not a real tool.
const maxSchemaNameLen = 128

// maxSchemaCandidates caps the near-miss suggestion list on a NOT_FOUND. Five is
// enough to cover a typo or truncation without turning a failure into a catalog
// dump (the bloat this family exists to avoid).
const maxSchemaCandidates = 5

// wrapperMCPAliases maps a LOCAL wrapper tool name onto the raw MCP tool it
// forwards to, for the cases where the two names DIFFER. Same-named wrappers
// (copyTree.generate, terminal.rename, terminal.close, recipe.run, …) need no
// entry — an exact catalog lookup already finds them, which is what makes the
// motivating copyTree.generate case work.
//
// Only thin passthroughs belong here. agentTask.spawnForEdits is deliberately
// ABSENT even though it forwards to agent.launch: its call contract is
// materially transformed, so handing back agent.launch's raw schema under the
// local name would describe arguments the wrapper does not accept.
var wrapperMCPAliases = map[string]string{
	"terminal.focus": mcpPanelFocus,
}

type schemaArgs struct {
	Name string `json:"name"`
}

// Validate enforces the bounds the declared JSON Schema advertises. StrictDecoder
// rejects unknown fields and type mismatches but runs no schema engine, so the
// length/emptiness rules must be re-stated here or they would be advisory only.
func (a schemaArgs) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("name is required — pass the exact MCP tool name, e.g. {\"name\":\"copyTree.generate\"}")
	}
	if utf8.RuneCountInString(a.Name) > maxSchemaNameLen {
		return fmt.Errorf("name is %d characters; MCP tool names are at most %d", utf8.RuneCountInString(a.Name), maxSchemaNameLen)
	}
	return nil
}

var schemaSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name": {
      "type": "string",
      "minLength": 1,
      "maxLength": 128,
      "description": "Exact MCP tool name, copied from tool.search or daintree.listTools. Not auto-corrected."
    }
  },
  "required": ["name"]
}`)

// schemaPointer is appended to the discovery tools' note so a model that just
// listed or searched tools is told, at the point of use, how to get the argument
// shape — rather than falling back to paging the listTools artifact out of habit.
const schemaPointer = "To see a tool's ARGUMENT SHAPE (its input schema), call `tool.schema` with the literal argument object {\"name\":\"copyTree.generate\"}, substituting the exact name you want. Do that instead of guessing argument keys or paging a listTools artifact — neither ever contains the schema."

func newSchemaTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "tool.schema",
		Description: "Look up ONE Daintree MCP tool's input schema (its exact argument shape) from the locally cached catalog. " +
			"Call it with the literal argument object {\"name\":\"copyTree.generate\"}, replacing the example with an exact name " +
			"from tool.search or daintree.listTools. Returns the server's real JSON Schema verbatim — never a flattened or " +
			"guessed summary — and does NOT invoke the tool. Use this before calling any tool whose arguments you are unsure " +
			"of, especially one whose own parameters are forwarded opaquely (e.g. copyTree.generate's `options`). Names are " +
			"matched exactly and never auto-corrected; a miss returns close candidates to retry with.",
		Risk:   domain.RiskRead,
		Schema: schemaSchema,
		Decode: tools.StrictDecoder(func() any { return &schemaArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a schemaArgs
			_ = json.Unmarshal(raw, &a)
			requested := strings.TrimSpace(a.Name)

			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected; cannot look up tool schemas. Use /reconnect to retry once Daintree is available.")
			}
			list, err := deps.MCP.ListTools(ctx, false)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while looking up an MCP tool schema.", tools.Unrecoverable())
				}
				return tools.Fail(codeMCPUnavailable, "Could not read the Daintree MCP tool catalog: "+err.Error()+" Use /reconnect to retry once Daintree is available.")
			}

			match, mcpName, found := resolveSchemaTool(list, requested)
			if !found {
				return schemaNotFound(list, requested)
			}
			return schemaResult(requested, mcpName, match.InputSchema)
		},
	}
}

// resolveSchemaTool finds the catalog entry for requested. Matching is EXACT and
// case-sensitive: a near match is reported as a suggestion, never silently
// resolved, because returning the wrong tool's contract is worse than a failure
// the model can correct. A local wrapper name is accepted only through the
// explicit wrapperMCPAliases table. Returns the entry, the raw MCP name it was
// found under, and whether it resolved.
func resolveSchemaTool(list []MCPToolInfo, requested string) (MCPToolInfo, string, bool) {
	byName := make(map[string]MCPToolInfo, len(list))
	for _, t := range list {
		byName[t.Name] = t
	}
	if t, ok := byName[requested]; ok {
		return t, t.Name, true
	}
	if alias, ok := wrapperMCPAliases[requested]; ok {
		if t, ok := byName[alias]; ok {
			return t, alias, true
		}
	}
	return MCPToolInfo{}, "", false
}

// schemaResult builds the success envelope, then enforces the inline size cap on
// the WHOLE serialized result rather than the schema alone — the cap that matters
// is the one the turn serializer applies (domain.MaxToolResultChars), and the
// wrapper fields count toward it.
func schemaResult(requested, mcpName string, inputSchema map[string]any) tools.ToolResult {
	result := map[string]any{
		"name":        mcpName,
		"inputSchema": inputSchema,
	}
	// Report the alias hop explicitly. Silently answering a terminal.focus lookup
	// with panel.focus's schema would tell the model to pass `panelId` to a
	// wrapper whose own parameter is `terminalId`.
	if requested != mcpName {
		result["requestedName"] = requested
		result["note"] = fmt.Sprintf(
			"%s is a local typed wrapper over the Daintree MCP tool %s. The schema below is %s's; call %s with its OWN declared parameters.",
			requested, mcpName, mcpName, requested)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		// A schema that cannot round-trip through JSON is a broken catalog entry,
		// not something the model can fix by retrying with different arguments.
		return tools.Fail(codeSchemaInvalid, fmt.Sprintf(
			"The cached input schema for %q could not be encoded as JSON (%v); it cannot be shown.", mcpName, err),
			tools.Unrecoverable())
	}
	if n := utf8.RuneCount(encoded); n > domain.MaxToolResultChars {
		return schemaTooLarge(mcpName, inputSchema, n)
	}
	return tools.Ok(fmt.Sprintf("Input schema for the %s MCP tool.", mcpName), result)
}

// schemaTooLarge reports an over-cap schema as an honest failure rather than a
// truncated one. A clipped JSON Schema is not a smaller schema — it is a
// syntactically broken one whose missing half the model cannot see, and emitting
// it would invite exactly the confident-but-wrong call this tool prevents.
//
// The failure still carries the schema's TOP-LEVEL PROPERTY NAMES, because
// returning nothing at all would put the model straight back to guessing. That
// list is explicitly labelled as an index, not a schema: it states no types, no
// nesting, and no conditional requirements, so it cannot be mistaken for the
// real thing the way a flattened summary could.
func schemaTooLarge(mcpName string, inputSchema map[string]any, resultChars int) tools.ToolResult {
	keys := topLevelPropertyNames(inputSchema)
	msg := fmt.Sprintf(
		"The input schema for %q is too large to return inline (%d characters, limit %d). No partial schema was returned — a clipped JSON Schema would be invalid rather than merely shorter.",
		mcpName, resultChars, domain.MaxToolResultChars)
	if len(keys) > 0 {
		msg += fmt.Sprintf(" Its top-level argument keys are: %s. These are NAMES ONLY — no types, nesting, or required flags — so confirm the shape another way before relying on them.",
			strings.Join(keys, ", "))
	}
	return tools.Fail(codeSchemaTooLarge, msg, tools.Unrecoverable(), tools.WithDetails(map[string]any{
		"name":                  mcpName,
		"resultChars":           resultChars,
		"maxToolResultChars":    domain.MaxToolResultChars,
		"topLevelPropertyNames": keys,
	}))
}

// topLevelPropertyNames lists the keys of a schema's `properties` object, sorted
// for determinism. Nothing else is read: this is an index of argument names, NOT
// a projection of the schema (no types, no nesting, no oneOf/anyOf), so it cannot
// misrepresent which argument combinations are legal.
func topLevelPropertyNames(inputSchema map[string]any) []string {
	props, ok := inputSchema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaNotFound reports an unmatched name with up to maxSchemaCandidates close
// alternatives. Suggesting is deliberate: the codebase has repeatedly been bitten
// by the model truncating or case-shifting an identifier, and a candidate list
// lets it self-correct in one round without us ever guessing on its behalf.
func schemaNotFound(list []MCPToolInfo, requested string) tools.ToolResult {
	candidates := schemaCandidates(list, requested)
	msg := fmt.Sprintf("No Daintree MCP tool is named %q. Names are matched exactly and never auto-corrected.", requested)
	if len(candidates) > 0 {
		msg += fmt.Sprintf(" Did you mean: %s? Retry tool.schema with one of those exact names.", strings.Join(candidates, ", "))
	} else {
		msg += " Find the exact name with tool.search first, then retry tool.schema with it."
	}
	return tools.Fail(codeToolNotFound, msg, tools.WithDetails(map[string]any{
		"requestedName": requested,
		"candidates":    candidates,
	}))
}

// schemaCandidates ranks near misses: a case-only difference first (the model
// shifted case), then a prefix relation in either direction (it truncated the
// name, or supplied a namespace), then a plain substring hit. Deliberately no
// edit-distance scoring — these three cover the failure modes actually observed,
// and a fuzzier net would surface unrelated tools as plausible.
func schemaCandidates(list []MCPToolInfo, requested string) []string {
	want := strings.ToLower(requested)
	if want == "" {
		return nil
	}
	// Wrapper aliases are offerable names too, but only when the tool they
	// forward to is actually live — suggesting a name that then fails to resolve
	// would cost the model another wasted round.
	names := make([]string, 0, len(list)+len(wrapperMCPAliases))
	live := make(map[string]bool, len(list))
	for _, t := range list {
		names = append(names, t.Name)
		live[t.Name] = true
	}
	for local, target := range wrapperMCPAliases {
		if live[target] {
			names = append(names, local)
		}
	}

	type scored struct {
		name string
		rank int
	}
	ranked := make([]scored, 0)
	for _, n := range names {
		low := strings.ToLower(n)
		switch {
		case low == want:
			ranked = append(ranked, scored{n, 0})
		case strings.HasPrefix(low, want) || strings.HasPrefix(want, low):
			ranked = append(ranked, scored{n, 1})
		case strings.Contains(low, want) || strings.Contains(want, low):
			ranked = append(ranked, scored{n, 2})
		}
	}
	// Rank first, then name: a stable, fully-determined order so the same miss
	// always suggests the same list.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		return ranked[i].name < ranked[j].name
	})
	out := make([]string, 0, maxSchemaCandidates)
	for _, r := range ranked {
		if len(out) >= maxSchemaCandidates {
			break
		}
		out = append(out, r.name)
	}
	return out
}
