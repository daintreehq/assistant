package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// callableNote explains the per-result `callable` flag so the model treats a
// callable:false entry as "known but not offered this turn", not "broken".
const callableNote = "`callable: false` means the tool exists but is not offered in this turn's tool spec (e.g. an active skill narrowed the toolset) — only `callable: true` tools can be invoked directly now. An unwrapped tool may still be reachable via `daintree.call` when that escape hatch is offered."

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
		Description: "Report Daintree MCP connection status (connected, transport, tool count). Works even when disconnected.",
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
    "query": { "type": "string", "description": "Keyword to match against MCP tool names/descriptions." },
    "max": { "type": "number", "description": "Max results to return (default 20)." }
  },
  "required": ["query"]
}`)

func newSearchTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "tool.search",
		Description: "Search Daintree MCP tools by keyword (substring match on name/description). Each match carries a `callable` " +
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
			list, err := deps.MCP.ListTools(ctx, false)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while searching MCP tools.", tools.Unrecoverable())
				}
				return tools.Fail(codeMCPUnavailable, "Could not search Daintree MCP tools: "+err.Error()+" Use /reconnect to retry once Daintree is available.")
			}
			callableOf := makeCallable(tctx.ActiveToolNames)
			q := strings.ToLower(a.Query)
			matches := make([]map[string]any, 0)
			for _, t := range list {
				if len(matches) >= max {
					break
				}
				if strings.Contains(strings.ToLower(t.Name), q) ||
					strings.Contains(strings.ToLower(t.Description), q) {
					matches = append(matches, map[string]any{
						"name": t.Name, "description": t.Description, "callable": callableOf(t.Name),
					})
				}
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
	"terminal.getOutput":           "terminal.summarize (model gist of the tail — DEFAULT for relaying what an agent said), terminal.read (raw scrollback VERBATIM — only when the exact literal text is needed), or terminal.extract (pull specific text/JSON, optionally waiting for a condition)",
	"panel.focus":                  "terminal.focus",
	"terminal.sendCommand":         "terminal.sendCommand (typed wrapper — pass terminalId and command)",
	"terminal.close":               `terminal.close (typed wrapper — pass terminalId, or terminalIds:["...","..."] to close several in one confirmed call)`,
	"terminal.arm":                 "terminal.arm (typed wrapper — pass terminalId)",
	"terminal.disarm":              "terminal.disarm (typed wrapper — pass terminalId)",
	"terminal.disarmAll":           "terminal.disarmAll (typed wrapper — no args needed)",
	"copyTree.injectToTerminal":    "copyTree.injectToTerminal (typed wrapper — pass terminalId)",
	"copyTree.generateAndCopyFile": "copyTree.generateAndCopyFile (typed wrapper — pass an optional worktreeId)",
	"git.snapshotRevert":           "git.snapshotRevert (typed wrapper — pass worktreeId)",
	"git.snapshotDelete":           "git.snapshotDelete (typed wrapper — pass worktreeId)",
	"git.getProjectPulse":          "git.getProjectPulse (typed read wrapper — pass an optional arguments object, e.g. {arguments:{worktreeId:\"...\"}}; read tier, no confirmation)",

	// Typed wrappers in internal/tools/mcpwrap — forwarding their raw MCP action
	// through daintree.call skips the wrapper's strict-decoded validation.
	"recipe.list":                  "recipe.list (typed wrapper — pass an optional arguments object)",
	"recipe.run":                   "recipe.run (typed wrapper — pass recipeId and an optional arguments object)",
	"worktree.createWithRecipe":    "worktree.createWithRecipe (typed wrapper — pass arguments)",
	"workflow.startWorkOnIssue":    "workflow.startWorkOnIssue (typed wrapper — pass arguments; it also attaches a supervisor watcher)",
	"workflow.prepBranchForReview": "workflow.prepBranchForReview (typed wrapper — pass arguments)",
	"forge.getPR":                  "forge.getPR (typed wrapper — pass arguments)",
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
