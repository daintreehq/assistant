package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// callableNote explains the per-result `callable` flag so the model treats a
// callable:false entry as "known but not in this turn's tool spec", not "broken".
// Loaded skills never narrow the toolset, so in normal operation every registered
// tool is callable.
const callableNote = "`callable: false` means the tool exists but is not in this turn's tool spec — only `callable: true` tools can be invoked directly. (Loaded skills do NOT restrict this; the full toolset is normally callable.) An unwrapped tool may still be reachable via `daintree.call` when that escape hatch is offered."

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
		Description: "Report the Daintree MCP link: connected, transport, tool count, and the error text when it is down. Works even while disconnected — it never fails on a broken link. The same summary ALREADY rides every round's runtime context, so call this only when diagnosing an MCP_UNAVAILABLE failure or when the user asks whether Daintree is reachable — not to orient yourself each turn.",
		Risk:        domain.RiskRead,
		Schema:      noArgs,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			status := deps.MCP.Status()
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
				summary = fmt.Sprintf("Daintree MCP connected via %s%s.", transport, count)
			} else if status.Error != "" {
				summary = fmt.Sprintf("Daintree MCP disconnected: %s", status.Error)
			} else {
				summary = "Daintree MCP disconnected."
			}
			return tools.Ok(summary, status)
		},
	}
}

/* --------------------------- daintree.listTools --------------------------- */

func newListToolsTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "daintree.listTools",
		Description: "List the Daintree MCP tools, with their names and descriptions. Each entry carries a `callable` flag: tools " +
			"marked `callable: false` are known to exist but are not offered in this turn's tool spec (e.g. an active skill " +
			"narrowed the toolset), so calling them would do nothing.",
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
				out = append(out, map[string]any{
					"name": t.Name, "description": t.Description, "callable": callableOf(t.Name),
				})
			}
			return tools.Ok(fmt.Sprintf("Found %d Daintree MCP tool(s).", len(out)),
				map[string]any{"tools": out, "note": callableNote})
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
		Description: "Search Daintree MCP tools by keyword. The query is split on spaces and a tool matches when EVERY word " +
			"appears in its name or description (order and filler words don't matter), so prefer a couple of plain keywords " +
			"(e.g. `rename terminal`) over a long phrase — name matches rank first. Each match carries a `callable` " +
			"flag: tools marked `callable: false` exist but are not offered in this turn's tool spec, so calling them would do " +
			"nothing — only `callable: true` results can be invoked now.",
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
					map[string]any{"query": a.Query, "matches": []map[string]any{}, "note": callableNote})
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
					row:     map[string]any{"name": t.Name, "description": t.Description, "callable": callableOf(t.Name)},
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
				map[string]any{"query": a.Query, "matches": matches, "note": callableNote})
		},
	}
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
	"agent.launch":                 `agentTask.spawnForEdits (set mode:"explore" for a read-only investigation, mode:"edit" to change files)`,
	"terminal.getOutput":           "terminal.summarize (model gist of the tail — DEFAULT for relaying what an agent said), terminal.read (raw scrollback VERBATIM — only when the exact literal text is needed), terminal.extract (pull a specific value as plain text, optionally waiting for a condition), or terminal.extract.json (structured fields — requires instruction + jsonSchema)",
	"panel.focus":                  "terminal.focus",
	"terminal.rename":              "terminal.rename (typed wrapper — pass terminalId and a non-empty name; UI-only, no confirmation)",
	"terminal.sendCommand":         "terminal.sendCommand (typed wrapper — pass terminalId and command)",
	"terminal.close":               `terminal.close (typed wrapper — pass terminalId, or terminalIds:["...","..."] to close several in one call; ONLY at the user's explicit request, never your own cleanup/recovery)`,
	"terminal.arm":                 "terminal.arm (typed wrapper — pass terminalId)",
	"terminal.disarm":              "terminal.disarm (typed wrapper — pass terminalId)",
	"terminal.disarmAll":           "terminal.disarmAll (typed wrapper — no args needed)",
	"copyTree.injectToTerminal":    `copyTree.injectToTerminal (typed wrapper — pass terminalId, plus options.includePaths to curate which files go in)`,
	"copyTree.generate":            `copyTree.generate (typed wrapper — returns the bundle's file PATH, not its text; pass options.includePaths to curate which files go in)`,
	"copyTree.generateAndCopyFile": `copyTree.generateAndCopyFile (typed wrapper — REQUIRES an explicit worktreeId or worktreePath, and takes options.includePaths to curate which files go in)`,
	"git.getProjectPulse":          "git.getProjectPulse (typed read wrapper — pass an optional arguments object, e.g. {arguments:{worktreeId:\"...\"}}; read tier, no confirmation)",

	// Typed wrappers in internal/tools/mcpwrap — forwarding their raw MCP action
	// through daintree.call skips the wrapper's strict-decoded validation.
	"recipe.list":                  "recipe.list (typed wrapper — pass an optional arguments object)",
	"recipe.run":                   "recipe.run (typed wrapper — pass recipeId and an optional arguments object)",
	"worktree.createWithRecipe":    "worktree.createWithRecipe (typed wrapper — pass arguments)",
	"workflow.startWorkOnIssue":    "workflow.startWorkOnIssue (typed wrapper — pass arguments; it also attaches a supervisor watcher)",
	"workflow.prepBranchForReview": "workflow.prepBranchForReview (typed wrapper — pass arguments)",

	// The forge reads are strictly typed on BOTH sides now (issue #299): the host
	// validates them with closed schemas and the wrapper mirrors that contract, so
	// forwarding the raw action through daintree.call trades a local, self-correcting
	// INVALID_ARGS for an opaque host refusal.
	"forge.listIssues": `forge.listIssues (typed wrapper — pass its fields, e.g. state/search/perPage/view, as TOP-LEVEL arguments rather than an arguments object; see that tool's schema for the full set)`,
	"forge.listPRs":    `forge.listPRs (typed wrapper — pass its fields, e.g. state/perPage/view, as TOP-LEVEL arguments; it has NO search field. See that tool's schema for the full set)`,
	"forge.getIssue":   "forge.getIssue (typed wrapper — pass issueNumber as a top-level positive integer; see that tool's schema for the optional worktree-location fields)",
	"forge.getPR":      "forge.getPR (typed wrapper — pass prNumber as a top-level positive integer; see that tool's schema for the optional worktree-location fields)",
}

// directMCPDependencies are raw Daintree MCP tools this build calls by name WITHOUT
// going through a typed wrapper — the runtime's core reads (boot identity, the
// terminal poll/read path the watcher and await loops live on, the worktree/agent
// probes). They are as load-bearing as the wrapped ones: if the host renames
// terminal.getStatus, supervision stops working, so drift must warn about them too.
//
// Kept here beside the wrapper names so the whole dependency set has ONE home.
// Unlike the deleted DocumentedMcpToolNames, this is NOT a transcription of the
// host's surface — it is a statement about OUR call sites, which is ours to own, and
// TestMCPDependenciesCoverWrapperCallSites keeps it honest against the source.
var directMCPDependencies = []string{
	"actions.getContext",
	"agent.launch",
	"agent.listAvailable",
	"project.getCurrent",
	"terminal.getOutput",
	"terminal.getStatus",
	"terminal.list",
}

// WrappedMCPToolNames returns every raw Daintree MCP tool name this build depends
// on — the typed wrappers' targets plus the direct call sites — sorted and
// de-duplicated for a stable order.
//
// It is the assistant's DRIFT BASELINE (wired in as mcp.Options.DriftBaseline):
// exactly the host tools whose disappearance or rename would silently break
// something here, so a missing one is a real, actionable breakage rather than a
// documentation nit. That is the whole reason it replaced the old hand-transcribed
// 59-name baseline, which described the SERVER's surface (including tools nothing
// here ever called) and went stale on every host change.
//
// NOTE: this is deliberately NOT just the keys of wrappedMCPTools. That map is a
// daintree.call REDIRECT denylist, a different job — some typed wrappers
// (worktree.list/getCurrent) have no entry because nothing needs redirecting for
// them. Using it alone silently under-covered the dependency set, so the two are
// unioned here and a test scans the wrapper sources to catch a new wrapper whose
// target was never declared.
//
// Returned as a fresh slice so a caller cannot mutate internal state through it.
func WrappedMCPToolNames() []string {
	seen := make(map[string]struct{}, len(wrappedMCPTools)+len(directMCPDependencies))
	for name := range wrappedMCPTools {
		seen[name] = struct{}{}
	}
	for _, name := range wrapperMCPTargets {
		seen[name] = struct{}{}
	}
	for _, name := range directMCPDependencies {
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// wrapperMCPTargets are raw MCP names that a typed wrapper calls but that
// wrappedMCPTools does not redirect. Split out (rather than folded into the
// denylist) because adding them there would change daintree.call's behaviour —
// it would start refusing raw calls that are currently allowed — which is a
// separate decision from drift coverage.
// Two former residents are deliberately absent, both for the same reason: they
// gained a denylist entry above, so the union already covers them from there and
// keeping them here too would contradict this list's own definition.
// copyTree.generate moved out when its curation options became strict-decoded (a
// raw daintree.call must be redirected so they are validated); the forge reads
// moved out when their contract became strictly typed on both sides (issue #299).
var wrapperMCPTargets = []string{
	"worktree.getCurrent",
	"worktree.list",
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
		Description: "Raw passthrough to ANY Daintree MCP tool. Escape hatch — highest risk ('system'), always confirmed, requires " +
			"the 'system' tier. Prefer purpose-built tools; use this only when no wrapper exists. Tools that already have a wrapper " +
			"(e.g. agent.launch, terminal.getOutput, panel.focus) are refused here and redirected to the wrapper.",
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
