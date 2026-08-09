package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
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
// The case that surfaced this was copyTree.generate's then-opaque `options` bag
// (since typed locally), but the gap it exposed is general and still open: every
// wrapper that forwards an `arguments` record verbatim — recipe.run,
// forge.getPR, workflow.*, worktree.* — advertises "additionalProperties": true
// and no keys, so the only written-down source for those keys is the raw MCP
// schema this tool returns.
//
// The schema comes from the catalog ListTools already maintains, read
// CACHE-FIRST (force=false): normally a warm-cache hit costing nothing, falling
// back to the same catalog fetch the sibling discovery tools make when the cache
// is cold. That source is wire-authoritative — literally what the server
// advertised — and covers every connected tool rather than just Daintree's
// action registry. We deliberately do NOT reach for Daintree's actions.getSchema
// when the cached schema is the empty default: the cache cannot distinguish "the
// server omitted a schema" from "the server advertised an empty object", so such
// a fallback would fire unpredictably.
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

// maxOversizeKeys caps how many argument names the over-cap failure lists. A
// hard count bound on top of the character budget, so a schema with thousands of
// one-character keys can't produce an unreadable wall of names.
const maxOversizeKeys = 60

// minCandidateOverlap is how many characters the SHORTER side of a near-miss
// must have before a reverse prefix/substring relation counts as a signal. Below
// it, a stubby name matches almost anything and the suggestion list turns into
// noise the model has to re-check.
const minCandidateOverlap = 4

// Lookups are RAW MCP names only — there is deliberately no local-wrapper alias
// table. A wrapper that renames or restructures arguments (terminal.focus takes
// terminalId where panel.focus takes panelId; terminal.close adds a plural
// terminalIds batch; recipe.run splits fields between the root and a nested
// arguments object) would have its raw schema returned under the local name, and
// a model reading that STRUCTURED schema would build a call the wrapper's strict
// decoder rejects. A prose caveat does not undo a structured schema. Local
// wrappers already declare their own schemas in the turn's tool spec, so there
// is nothing to discover; what needs discovering is the raw shape behind values
// a wrapper forwards opaquely, which the raw name resolves directly.

// localWrapperNames is the set of tool names this family registers locally,
// used to flag a raw schema whose same-named typed wrapper actually governs the
// call. Derived from the family's own registration rather than a hand-kept list
// so a newly added wrapper is covered automatically — the daintree.call denylist
// looks like the natural index but is NOT one (copyTree.generate has a wrapper
// yet no denylist entry, so using it would have silently skipped the annotation
// for the very tool that motivated this feature).
//
// Computed at runtime (not package init) to avoid a static initialization cycle:
// the closure would call Tools(), which constructs this tool, which closes over
// schemaResult, which would then call localWrapperNames again. Instead, we
// compute it lazily on first access and cache it. Deps are irrelevant — only
// names are read.
var (
	localWrapperNamesOnce sync.Once
	localWrapperNamesMap  map[string]bool
)

func getLocalWrapperNames() map[string]bool {
	localWrapperNamesOnce.Do(func() {
		localWrapperNamesMap = make(map[string]bool)
		for _, t := range Tools(Deps{}) {
			localWrapperNamesMap[t.Name] = true
		}
	})
	return localWrapperNamesMap
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
	// Reject padding rather than trimming it. The tool promises exact matching
	// and no auto-correction; silently trimming would make that promise false in
	// one direction, and the audit record would then disagree with the name
	// actually looked up.
	if a.Name != strings.TrimSpace(a.Name) {
		return fmt.Errorf("name has leading or trailing whitespace; pass it exactly, e.g. {\"name\":\"copyTree.generate\"}")
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
const schemaPointer = "To see a tool's ARGUMENT SHAPE (its input schema), call `tool.schema` with the literal argument object {\"name\":\"recipe.run\"}, substituting the exact name you want. Do that instead of guessing argument keys or paging a listTools artifact — neither of these results ever contains a schema."

func newSchemaTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "tool.schema",
		Description: "Look up ONE Daintree MCP tool's input schema — its exact argument shape — without invoking it. Call with " +
			"the literal argument object {\"name\":\"recipe.run\"}, using an exact name from tool.search or " +
			"daintree.listTools (names are never auto-corrected; a miss returns close candidates). Reach for it whenever you " +
			"would otherwise guess an argument key — above all for a wrapper's opaque `arguments` record, whose keys are " +
			"Daintree's and are documented nowhere else.",
		Risk:   domain.RiskRead,
		Schema: schemaSchema,
		Decode: tools.StrictDecoder(func() any { return &schemaArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a schemaArgs
			_ = json.Unmarshal(raw, &a)
			// No trimming here: Validate already rejected a padded name, so the
			// name looked up is byte-for-byte the one the model sent (and the one
			// the audit records).
			requested := a.Name

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

			match, found, ambiguous := resolveSchemaTool(list, requested)
			if ambiguous {
				// Two catalog entries share this name with DIFFERENT schemas, so
				// we cannot know which one the server would dispatch. Returning
				// either would be a confident guess at a contract — the one thing
				// this tool exists not to do.
				return tools.Fail(codeSchemaInvalid, fmt.Sprintf(
					"The Daintree MCP catalog advertises %q more than once with different schemas, so its argument shape is ambiguous. Report this rather than guessing the arguments.", requested),
					tools.Unrecoverable())
			}
			if !found {
				return schemaNotFound(list, requested)
			}
			return schemaResult(requested, match.InputSchema)
		},
	}
}

// resolveSchemaTool finds the catalog entry for requested. Matching is EXACT and
// case-sensitive: a near match is reported as a suggestion, never silently
// resolved, because returning the wrong tool's contract is worse than a failure
// the model can correct. Reports ambiguous=true when the catalog advertises the
// name twice with DIFFERING schemas — silently taking one would be the same
// confident guess the exact-match rule exists to prevent. Duplicates that agree
// are harmless and resolve normally.
func resolveSchemaTool(list []MCPToolInfo, requested string) (match MCPToolInfo, found, ambiguous bool) {
	for _, t := range list {
		if t.Name != requested {
			continue
		}
		if !found {
			match, found = t, true
			continue
		}
		if !sameSchema(match.InputSchema, t.InputSchema) {
			return MCPToolInfo{}, false, true
		}
	}
	return match, found, false
}

// sameSchema compares two schemas by their canonical JSON encoding. Go maps have
// no ordering, but encoding/json sorts object keys, so equal encodings mean equal
// schemas. An encoding failure counts as "different" so an unreadable duplicate
// is reported as ambiguous rather than silently accepted.
func sameSchema(a, b map[string]any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return string(ab) == string(bb)
}

// serializedEnvelope mirrors the shape agent.SerializeToolResult actually
// marshals and measures ({ok,summary,result,error}, result/error omitempty). We
// re-declare it rather than import it because the cap we must respect is the
// SERIALIZER's, not our own: measuring the bare result map would under-count by
// the wrapper and summary, letting a schema land in the gap where our guard says
// "fine" and the serializer then converts it to a paged artifact stub — the exact
// outcome this tool exists to prevent. mcpx cannot import internal/agent (the
// dependency runs the other way), so the shape is duplicated and pinned by test.
type serializedEnvelope struct {
	Ok      bool   `json:"ok"`
	Summary string `json:"summary"`
	Result  any    `json:"result,omitempty"`
}

// schemaResult builds the success envelope, then enforces the inline size cap on
// the WHOLE serialized envelope rather than the schema alone.
func schemaResult(mcpName string, inputSchema map[string]any) tools.ToolResult {
	result := map[string]any{
		"name":        mcpName,
		"inputSchema": inputSchema,
	}
	// When a typed wrapper of the same name exists, the schema below is NOT the
	// shape to call it with: wrappers variously make optional args required
	// (terminal.rename), add batch forms (terminal.close), or nest raw fields
	// under `arguments` (recipe.run). Without this the model reads a raw schema
	// and builds a call the wrapper's strict decoder rejects — a new version of
	// the very bug this tool fixes. The denylist is already the authoritative
	// wrapper index, so it doubles as the annotation source.
	if getLocalWrapperNames()[mcpName] {
		result["localWrapper"] = mcpName
		result["note"] = fmt.Sprintf(
			"A local typed tool named %s governs the actual call — invoke THAT with its own declared parameters, which differ from the raw schema below (a wrapper may make optional arguments required, add a batch form, or nest raw fields under `arguments`). Use the raw schema only to fill in values the wrapper forwards opaquely, such as the contents of an `arguments` record.",
			mcpName)
	}

	summary := fmt.Sprintf("Input schema for the %s MCP tool.", mcpName)
	encoded, err := json.Marshal(serializedEnvelope{Ok: true, Summary: summary, Result: result})
	if err != nil {
		// A schema that cannot round-trip through JSON is a broken catalog entry,
		// not something the model can fix by retrying with different arguments.
		return tools.Fail(codeSchemaInvalid, fmt.Sprintf(
			"The cached input schema for %q could not be encoded as JSON (%v); it cannot be shown.", mcpName, err),
			tools.Unrecoverable())
	}
	// Rune count, matching the serializer's charLen — a multibyte-heavy schema
	// whose BYTE length exceeds the cap but whose character count does not must
	// still be returned inline.
	if n := utf8.RuneCount(encoded); n > domain.MaxToolResultChars {
		return schemaTooLarge(mcpName, inputSchema, n)
	}
	return tools.Ok(summary, result)
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
	all := topLevelPropertyNames(inputSchema)
	// Shrink the key index until the ENCODED failure fits. A character budget
	// computed from raw name lengths is not a proof: the names appear three times
	// (summary, message, details) and JSON escaping can expand a name several-fold
	// (quotes, backslashes, control characters). So we build the real failure and
	// measure it, dropping keys until it fits — otherwise the "too large" report
	// itself overflows and the serializer pages IT into an artifact, with the
	// failure path re-creating the exact problem the success path just refused to
	// cause.
	kept := all
	if len(kept) > maxOversizeKeys {
		kept = kept[:maxOversizeKeys]
	}
	for {
		res := oversizeFailure(mcpName, resultChars, kept, len(all)-len(kept))
		if encoded, err := json.Marshal(res); err == nil && utf8.RuneCount(encoded) <= domain.MaxToolResultChars {
			return res
		}
		if len(kept) == 0 {
			// Even the bare report doesn't fit (or won't encode): fall back to a
			// fixed minimal failure that cannot overflow.
			return tools.Fail(codeSchemaTooLarge, fmt.Sprintf(
				"The input schema for %q is too large to return inline. No partial schema was returned. Do not guess its arguments — say the schema could not be retrieved.", mcpName),
				tools.Unrecoverable())
		}
		// Halve rather than step: a pathological catalog shrinks in log time.
		kept = kept[:len(kept)/2]
	}
}

// oversizeFailure renders one candidate over-cap report. Split out so the size
// loop above can build and measure it repeatedly.
func oversizeFailure(mcpName string, resultChars int, keys []string, omitted int) tools.ToolResult {
	msg := fmt.Sprintf(
		"The input schema for %q is too large to return inline (%d characters, limit %d). No partial schema was returned — a clipped JSON Schema would be invalid rather than merely shorter.",
		mcpName, resultChars, domain.MaxToolResultChars)
	if len(keys) > 0 {
		msg += fmt.Sprintf(" Its top-level argument keys are: %s", strings.Join(keys, ", "))
		if omitted > 0 {
			msg += fmt.Sprintf(" (and %d more, omitted to keep this message inline)", omitted)
		}
		// Name the actual next step. "Confirm it another way" would be a dead end:
		// by construction no other tool can supply this schema, so the honest
		// instruction is to stop rather than to keep looking.
		msg += ". These are NAMES ONLY — no types, nesting, or required flags. Do NOT build a call from them: state that the schema is too large to retrieve and ask how to proceed."
	}
	return tools.Fail(codeSchemaTooLarge, msg, tools.Unrecoverable(), tools.WithDetails(map[string]any{
		"name":                  mcpName,
		"resultChars":           resultChars,
		"maxToolResultChars":    domain.MaxToolResultChars,
		"topLevelPropertyNames": keys,
		"omittedPropertyNames":  omitted,
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
	switch {
	case len(candidates) > 0:
		msg += fmt.Sprintf(" Did you mean: %s? Retry tool.schema with one of those exact names.", strings.Join(candidates, ", "))
	case len(list) == 0:
		// Pointing at tool.search would be a dead end — it reads the same empty
		// catalog and can only return nothing.
		msg += " The Daintree MCP catalog is currently empty, so no tool schema can be looked up; report that rather than searching."
	default:
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
	// Deduplicate: a catalog that advertises a name twice must not suggest it
	// twice, which would waste half a short candidate list on one answer.
	names := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, t := range list {
		if !seen[t.Name] {
			seen[t.Name] = true
			names = append(names, t.Name)
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
		// Forward prefix (a name truncated mid-word → the full name) is the
		// truncation case and always meaningful. The REVERSE relations (a
		// catalog name contained in the request) only mean something once the
		// shorter side is long enough to be distinctive — otherwise a
		// one-character tool name would be suggested for nearly every miss.
		case strings.HasPrefix(low, want):
			ranked = append(ranked, scored{n, 1})
		case len(low) >= minCandidateOverlap && strings.HasPrefix(want, low):
			ranked = append(ranked, scored{n, 1})
		case len(want) >= minCandidateOverlap && strings.Contains(low, want):
			ranked = append(ranked, scored{n, 2})
		case len(low) >= minCandidateOverlap && strings.Contains(want, low):
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
