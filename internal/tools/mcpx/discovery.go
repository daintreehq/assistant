package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/tools"
)

// callableNote explains the per-result `callable` flag so the model treats a
// callable:false entry as "known but not in this turn's tool spec", not "broken".
// Loaded runbooks never narrow the toolset, so in normal operation every registered
// tool is callable.
const callableNote = "`callable: false` means the tool exists but is not in this turn's tool spec — only `callable: true` tools can be invoked directly. (Loaded runbooks do NOT restrict this; the full toolset is normally callable.) An unwrapped tool may still be reachable via `daintree.call` when that escape hatch is offered."

// discoveryNote is what list/search actually return: the callable explanation
// plus a pointer to tool.schema. Shipping the schema lookup without pointing at
// it here would leave the model doing what it did before — guessing arguments,
// or paging a listTools artifact that never contained a schema — simply because
// it never learned the lookup exists. Findability and the capability ship
// together (cf. the tool.search tokenization fix, which needed the same pairing).
var discoveryNote = callableNote + " " + schemaPointer

// makeCallable builds a predicate reporting whether a discovered MCP tool is
// offered in the current turn's projection. activeToolNames==nil ⇒ unconstrained
// (every tool callable).
func makeCallable(activeToolNames []string) func(string) bool {
	if activeToolNames == nil {
		return func(string) bool { return true }
	}
	offered := make(map[string]bool, len(activeToolNames))
	for _, n := range activeToolNames {
		offered[n] = true
	}
	return func(name string) bool { return offered[name] }
}

/* ----------------------------- daintree.status ---------------------------- */

func newStatusTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name:        "daintree.status",
		Description: "Report the Daintree MCP link: the endpoint URL it is connected to, connected, transport, tool count, and the error text when it is down. Works even while disconnected — it never fails on a broken link. The endpoint and the same summary ALREADY ride every round's context, so call this only when diagnosing an MCP_UNAVAILABLE failure or when the user asks whether Daintree is reachable — not to orient yourself each turn.",
		Risk:        domain.RiskRead,
		Schema:      noArgs,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			status := deps.MCP.Status()
			// The endpoint is reported on BOTH branches: "which server is it even trying?"
			// is exactly the question a disconnected link raises, and answering it from the
			// error text alone is guesswork.
			at := ""
			if status.URL != "" {
				at = fmt.Sprintf(" at %s", status.URL)
			}
			var summary string
			if status.Connected {
				transport := status.Transport
				if transport == "" {
					transport = "unknown"
				}
				count := ""
				if status.ToolCount != nil {
					count = fmt.Sprintf(" (%d tools)", *status.ToolCount)
				}
				summary = fmt.Sprintf("Daintree MCP connected%s via %s%s.", at, transport, count)
			} else if status.Error != "" {
				summary = fmt.Sprintf("Daintree MCP disconnected%s: %s", at, status.Error)
			} else {
				summary = fmt.Sprintf("Daintree MCP disconnected%s.", at)
			}
			return tools.Ok(summary, status)
		},
	}
}

/* --------------------------- daintree.listTools --------------------------- */

func newListToolsTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "daintree.listTools",
		Description: "List every Daintree MCP action with its description and policy (`risk`, `requiredTier`, `confirms`, " +
			"`preferredTool`, `invocable`). Prefer `tool.search` — this dumps the whole catalog — and `tool.schema` for one " +
			"action's arguments. `callable: false` means the action exists but is not in this turn's tool spec.",
		Risk:   domain.RiskRead,
		Schema: noArgs,
		Decode: tools.StrictDecoder(func() any { return &struct{}{} }),
		Handle: func(ctx context.Context, _ json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected; cannot list tools. Use /reconnect to retry once Daintree is available.")
			}
			list, err := deps.MCP.ListTools(ctx, false)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while listing MCP tools.", tools.Unrecoverable())
				}
				return tools.Fail(codeMCPUnavailable, "Could not list Daintree MCP tools: "+err.Error()+" Use /reconnect to retry once Daintree is available.")
			}
			callableOf := makeCallable(tctx.ActiveToolNames)
			out := make([]map[string]any, 0, len(list))
			for _, t := range list {
				out = append(out, discoveryRow(deps, t, callableOf))
			}
			return tools.Ok(fmt.Sprintf("Found %d Daintree MCP tool(s).", len(out)),
				map[string]any{"tools": out, "note": discoveryNote})
		},
	}
}

/* ------------------------------- tool.search ------------------------------ */

type searchArgs struct {
	Query string `json:"query"`
	Max   *int   `json:"max,omitempty"`
}

var searchSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "Keywords to match against MCP tool names/descriptions. Split on spaces; every word must appear (AND). Use a few plain words, not a long phrase." },
    "max": { "type": "number", "description": "Max results to return (default 20)." }
  },
  "required": ["query"]
}`)

func newSearchTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "tool.search",
		Description: "Search Daintree MCP actions by keyword. The query splits on spaces and an action matches when EVERY word " +
			"appears in its name or description, so prefer a couple of plain keywords (e.g. `rename terminal`) over a phrase — " +
			"name matches rank first. Each match carries its policy: `risk`, `requiredTier`, `confirms`, `preferredTool`, " +
			"and `invocable` — whether `daintree.invoke` can run it (with `unavailableReason` when not). Read its arguments " +
			"with `tool.schema` before invoking. `callable:false` means the tool exists but is not in this turn's tool spec.",
		Risk:   domain.RiskRead,
		Schema: searchSchema,
		Decode: tools.StrictDecoder(func() any { return &searchArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a searchArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected; cannot search MCP tools. Use /reconnect to retry once Daintree is available.")
			}
			max := 20
			if a.Max != nil {
				max = *a.Max
			}
			// Clamp max to a sane window: a non-positive max would silently return zero
			// matches, and an oversized one could dump the whole 100+ tool catalog back
			// into context (the exact bloat this discovery tool exists to avoid).
			if max < 1 {
				max = 20
			}
			if max > 50 {
				max = 50
			}
			list, err := deps.MCP.ListTools(ctx, false)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while searching MCP tools.", tools.Unrecoverable())
				}
				return tools.Fail(codeMCPUnavailable, "Could not search Daintree MCP tools: "+err.Error()+" Use /reconnect to retry once Daintree is available.")
			}
			callableOf := makeCallable(tctx.ActiveToolNames)
			// Tokenize the query: a tool matches when EVERY whitespace-separated term
			// is a substring of its name or description (AND semantics). The old code
			// matched the WHOLE query as one substring, so a natural multi-word query
			// like "terminal rename title" found nothing (no description contains that
			// exact phrase) even though "rename" alone hit terminal.rename — the model
			// then burned several rounds re-phrasing and dumping daintree.listTools.
			// Per-term substring matching makes word order and filler words irrelevant.
			terms := strings.Fields(strings.ToLower(a.Query))
			// An all-whitespace (or empty) query has no terms; an AND over zero terms is
			// vacuously true and would match EVERY tool, so treat it as "no matches"
			// rather than dumping the catalog.
			if len(terms) == 0 {
				return tools.Ok(fmt.Sprintf("Found 0 Daintree MCP tool(s) matching %q.", a.Query),
					map[string]any{"query": a.Query, "matches": []map[string]any{}, "note": discoveryNote})
			}
			// nameHit ranks a tool above description-only hits: a term landing in the
			// tool NAME is a far stronger signal than one buried in prose, so name
			// matches are surfaced first (the model usually wants the obvious tool).
			type scored struct {
				row     map[string]any
				nameHit bool
			}
			ranked := make([]scored, 0)
			for _, t := range list {
				name := strings.ToLower(t.Name)
				desc := strings.ToLower(t.Description)
				hay := name + " " + desc
				all, nameHit := true, false
				for _, term := range terms {
					if !strings.Contains(hay, term) {
						all = false
						break
					}
					if strings.Contains(name, term) {
						nameHit = true
					}
				}
				if !all {
					continue
				}
				ranked = append(ranked, scored{
					row:     discoveryRow(deps, t, callableOf),
					nameHit: nameHit,
				})
			}
			// Stable sort: name hits first, original (Daintree) order preserved within
			// each group. A stable sort keeps the result deterministic across calls.
			sort.SliceStable(ranked, func(i, j int) bool {
				return ranked[i].nameHit && !ranked[j].nameHit
			})
			matches := make([]map[string]any, 0, len(ranked))
			for _, r := range ranked {
				if len(matches) >= max {
					break
				}
				matches = append(matches, r.row)
			}
			return tools.Ok(fmt.Sprintf("Found %d Daintree MCP tool(s) matching %q.", len(matches), a.Query),
				map[string]any{"query": a.Query, "matches": matches, "note": discoveryNote + " " + invokeNote})
		},
	}
}

// invokeNote steers the model from a search result straight to the two-step it
// otherwise has to infer. Without it the observed behaviour is to reach for
// daintree.call the moment no wrapper name is recognised — which is exactly the
// blanket system-tier confirmation this family now exists to avoid.
const invokeNote = "For an action with `invocable: true`, read its argument shape with `tool.schema` and then run it with `daintree.invoke` — it is gated at the action's own `risk`, so a read needs no approval. `daintree.call` is only for an action reported `policySource: \"unknown\"`."

// discoveryRow renders one catalog entry for tool.search / daintree.listTools:
// the name, description and per-turn `callable` flag as before, plus the policy
// block that makes the result a complete invocation contract rather than a name.
//
// It merges rather than nests the policy fields so a row stays flat and cheap —
// these results are read by the model on nearly every orchestration turn, and an
// extra level of nesting per row costs tokens on all of them for no added meaning.
func discoveryRow(deps Deps, t MCPToolInfo, callableOf func(string) bool) map[string]any {
	row := map[string]any{
		"name": t.Name, "description": t.Description, "callable": callableOf(t.Name),
	}
	for k, v := range policyBlock(deps, t.Name, t.InputSchemaProvided) {
		row[k] = v
	}
	return row
}

/* ------------------------------ daintree.call ----------------------------- */

// wrappedMCPTools is the daintree.call DENYLIST: raw MCP tool name → the typed
// wrapper the model must use instead. Forwarding one of these raw through the
// escape hatch would bypass the wrapper's typed validation (a dropped/renamed
// required arg silently reaching Daintree), so we redirect. The first block is the
// original core set; the second block adds the remaining typed-wrapper MCP action
// names (recipe/worktree/workflow/forge passthroughs) that were registered with a
// typed wrapper but were NOT being denied here. Keys are matched case-insensitively
// after whitespace normalization (see normalizeMCPName) so a "Recipe.Run" or a
// padded " recipe.run " variant cannot bypass the redirect.
var wrappedMCPTools = map[string]string{
	"agent.launch":            `agentTask.spawnForEdits (set mode:"explore" for a read-only investigation, mode:"edit" to change files)`,
	"terminal.getOutput":      "terminal.summarize (model gist of the tail — DEFAULT for relaying what an agent said), terminal.read (raw scrollback VERBATIM — only when the exact literal text is needed), terminal.extract (pull a specific value as plain text, optionally waiting for a condition), or terminal.extract.json (structured fields — requires instruction + jsonSchema)",
	"panel.focus":             "terminal.focus",
	"terminal.rename":         "terminal.rename (typed wrapper — pass terminalId and a non-empty name; UI-only, no confirmation)",
	"terminal.sendCommand":    "terminal.sendCommand (typed wrapper — pass terminalId and command)",
	"terminal.close":          `terminal.close (typed wrapper — pass terminalId, or terminalIds:["...","..."] to close several in one call; ONLY at the user's explicit request, never your own cleanup/recovery)`,
	"terminal.moveToWorktree": `terminal.moveToWorktree (typed wrapper — pass terminalId, or terminalIds:["...","..."] to relocate a cohort in one call, plus worktreeId as the exact PATH from worktree.list; it does NOT restart the process, so still send each live agent "Please continue in the directory <worktreePath>")`,
	"terminal.arm":            "terminal.arm (typed wrapper — pass terminalId)",
	"terminal.disarm":         "terminal.disarm (typed wrapper — pass terminalId)",
	"terminal.disarmAll":      "terminal.disarmAll (typed wrapper — no args needed)",
	// copyTree.generate has had a typed wrapper since its `options` bag was
	// strict-decoded locally, but it was never denylisted, so the raw forward stayed
	// open beside it — the same drift the two worktree reads had.
	"copyTree.generate":            "copyTree.generate (typed wrapper — pass a typed options object; the raw form's opaque `options` bag is exactly what the wrapper exists to validate)",
	"copyTree.injectToTerminal":    "copyTree.injectToTerminal (typed wrapper — pass terminalId, an optional worktreeId, and an optional top-level name labelling the run)",
	"copyTree.generateAndCopyFile": "copyTree.generateAndCopyFile (typed wrapper — pass an optional worktreeId and an optional top-level name labelling the run)",
	"git.getProjectPulse":          "git.getProjectPulse (typed read wrapper — pass an optional arguments object, e.g. {arguments:{worktreeId:\"...\"}}; read tier, no confirmation)",

	// Typed wrappers in internal/tools/mcpwrap — forwarding their raw MCP action
	// through daintree.call skips the wrapper's strict-decoded validation.
	"recipe.list":                  "recipe.list (typed wrapper — pass an optional arguments object)",
	"recipe.run":                   "recipe.run (typed wrapper — pass recipeId and an optional arguments object)",
	"worktree.createWithRecipe":    "worktree.createWithRecipe (typed wrapper — pass arguments)",
	"workflow.startWorkOnIssue":    "workflow.startWorkOnIssue (typed wrapper — pass arguments; it also attaches a supervisor watcher)",
	"workflow.prepBranchForReview": "workflow.prepBranchForReview (typed wrapper — pass arguments)",
	"forge.getPR":                  "forge.getPR (typed wrapper — pass arguments)",
	// The other three forge READS are wrapped in internal/tools/mcpwrap too, and were
	// never denylisted — the raw forward stayed open beside each typed wrapper.
	"forge.listIssues": "forge.listIssues (typed read wrapper — pass an optional arguments object; read tier, parallelizable)",
	"forge.listPRs":    "forge.listPRs (typed read wrapper — pass an optional arguments object; read tier, parallelizable)",
	"forge.getIssue":   "forge.getIssue (typed read wrapper — pass arguments with the issue number under the key the forge expects)",
	// Both worktree reads have had typed mcpwrap wrappers since they were added, but
	// neither was ever denylisted here, so the raw forward stayed open beside them.
	// The two indexes answer different questions and drift apart exactly like this.
	"worktree.list":       "worktree.list (typed read wrapper — pass an optional arguments object; read tier, no confirmation)",
	"worktree.getCurrent": "worktree.getCurrent (typed read wrapper — pass an optional arguments object; read tier, no confirmation)",
	// The most-asked question in a CI fix-and-verify loop. Until forge.getChecks existed
	// this was the one common read with no wrapper, so the base prompt's "prefer the
	// typed wrapper" rule pointed the model at this confirmation-gated escape hatch on
	// nearly every turn — and two runbooks grew prose documenting the exception.
	"forge.getCIStatus": "forge.getChecks (typed wrapper — pass prNumber; it also flags the null-vs-no-checks and required-only-counts traps)",

	// Issue #367: the observation/verification actions that used to be reachable ONLY
	// through this escape hatch, so ordinary checking cost a system-tier confirmation.
	// Each now has a typed wrapper of the SAME name carrying the target action's real
	// risk, so the raw path here would only skip that wrapper's validation.
	"project.detectRunners":      "project.detectRunners (typed read wrapper — pass an optional projectId; read tier, no confirmation)",
	"project.runCheck":           "project.runCheck (typed wrapper — pass projectId and runnerId, plus an optional cwd and timeoutMs)",
	"forge.listIssueComments":    "forge.listIssueComments (typed read wrapper — pass issueNumber, plus an optional worktree locator, cursor and perPage)",
	"agentSessionHistory.list":   "agentSessionHistory.list (typed read wrapper — pass an optional worktreeId/projectId scope and limit/offset)",
	"browser.getConsoleMessages": "browser.getConsoleMessages (typed read wrapper — pass an optional dev-preview terminalId, level and limit)",
	"errors.recent":              "errors.recent (typed read wrapper — pass an optional limit and includesDismissed)",
	"notifications.recent":       "notifications.recent (typed read wrapper — pass an optional limit, type and unreadOnly)",
	"worktree.resource.status":   "worktree.resource.status (typed wrapper — pass an optional worktreeId; it runs the configured status command)",
}

// denylistLookup is wrappedMCPTools re-keyed on the lowercased name so the
// case-insensitive comparison is a single map hit. Built once at init.
var denylistLookup = func() map[string]string {
	m := make(map[string]string, len(wrappedMCPTools))
	for k, v := range wrappedMCPTools {
		m[strings.ToLower(k)] = v
	}
	return m
}()

// IsWrappedMCPName reports whether daintree.call refuses this raw MCP action because a
// typed wrapper governs it. Exported for the cross-package parity test in internal/app:
// the wrapper lives in mcpwrap and the denylist lives here, neither package may import
// the other, and a wrapper registered without an entry silently leaves the raw bypass
// open.
func IsWrappedMCPName(name string) bool {
	_, found := denylistLookup[normalizeMCPName(name)]
	return found
}

// WrappedMCPNames returns the raw action names daintree.call refuses, sorted for a
// deterministic test failure order.
func WrappedMCPNames() []string {
	names := make([]string, 0, len(wrappedMCPTools))
	for k := range wrappedMCPTools {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// WrappedMCPRedirect returns the wrapper a refused raw name points at, or "" when the
// name is not refused.
func WrappedMCPRedirect(name string) string {
	return denylistLookup[normalizeMCPName(name)]
}

// normalizeMCPName trims surrounding (and embedded control) whitespace from a
// requested MCP tool name so a padded/case-shifted variant ("  Recipe.Run\t")
// can't slip past the exact-match denylist. Internal whitespace is stripped too
// because a valid MCP action name never contains spaces — any is an evasion
// attempt. The result is lowercased for the case-insensitive denylist compare.
func normalizeMCPName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < ' ' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

type callArgs struct {
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RequestKey string         `json:"requestKey,omitempty"`
}

var callSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name": { "type": "string", "description": "Daintree MCP tool name to invoke." },
    "arguments": { "type": "object", "additionalProperties": true, "description": "Arguments object passed to the MCP tool." },
    "requestKey": { "type": "string", "description": "Optional idempotency / request key forwarded to the tool." }
  },
  "required": ["name"]
}`)

func newCallTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "daintree.call",
		Description: "Raw passthrough to ANY Daintree MCP tool. Escape hatch — highest risk ('system'), always confirmed, " +
			"'system' tier, never grantable. Prefer a purpose-built tool, then `daintree.invoke` (which gates an action at ITS " +
			"OWN risk instead of this blanket system-tier prompt). Use this only for an action `daintree.invoke` refuses as " +
			"policy-unknown. Wrapped tools (agent.launch, terminal.getOutput, panel.focus) are redirected to their wrapper.",
		Consequence: "Runs an arbitrary Daintree MCP tool with the arguments shown. Effect depends entirely on the named tool — inspect the args before approving.",
		Risk:        domain.RiskSystem,
		Schema:      callSchema,
		Decode:      tools.StrictDecoder(func() any { return &callArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a callArgs
			_ = json.Unmarshal(raw, &a)

			// 1. Typed-wrapper denylist. Compare on the NORMALIZED (whitespace-stripped,
			//    lowercased) name so a padded/case-shifted variant ("  Recipe.Run ")
			//    can't slip past the redirect into the raw forward below.
			if wrapper, found := denylistLookup[normalizeMCPName(a.Name)]; found {
				return tools.Fail(codeUseTypedWrapper, fmt.Sprintf(
					"Do not call %s through daintree.call — use the typed wrapper instead: %s. It takes named, validated parameters, "+
						"so you can't drop a required argument. Switch tools; do not retry this raw call.", a.Name, wrapper))
			}
			// 2. No-file-edit re-check on the RAW forwarded name (registration-time guard
			//    only covers local names; this is the runtime escape-hatch re-check).
			if safety.IsForbiddenToolName(a.Name) {
				return tools.Fail(safety.FileEditForbiddenCode, fmt.Sprintf(
					"Refusing to call %s via daintree.call — the assistant never edits files directly. Spawn a visible agent "+
						"(agentTask.spawnForEdits) to make changes.", a.Name), tools.Unrecoverable())
			}
			// 3. Connectivity.
			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, fmt.Sprintf("Daintree MCP is not connected; cannot call %s. Use /reconnect to retry once Daintree is available.", a.Name))
			}
			// 4. Forward.
			callArgsMap := make(map[string]any, len(a.Arguments)+1)
			for k, v := range a.Arguments {
				callArgsMap[k] = v
			}
			if a.RequestKey != "" {
				callArgsMap["requestKey"] = a.RequestKey
			}
			res, err := deps.MCP.CallTool(ctx, a.Name, callArgsMap)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", a.Name), tools.Unrecoverable())
				}
				return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree MCP call %s failed: %s", a.Name, err.Error()))
			}
			if res.IsError {
				msg := res.Text
				if msg == "" {
					msg = fmt.Sprintf("Daintree MCP tool %s returned an error.", a.Name)
				}
				return tools.Fail(codeMCPToolError, msg,
					tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent}))
			}
			return tools.Ok(fmt.Sprintf("Called %s.", a.Name), map[string]any{
				"text": res.Text, "structuredContent": res.StructuredContent, "isError": res.IsError,
			})
		},
	}
}
