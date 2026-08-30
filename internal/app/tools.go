package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/tools"

	"github.com/daintreehq/assistant/internal/tools/agenttaskx"
	"github.com/daintreehq/assistant/internal/tools/artifactx"
	"github.com/daintreehq/assistant/internal/tools/asyncx"
	"github.com/daintreehq/assistant/internal/tools/auditx"
	"github.com/daintreehq/assistant/internal/tools/contextx"
	"github.com/daintreehq/assistant/internal/tools/extractionx"
	"github.com/daintreehq/assistant/internal/tools/fsx"
	"github.com/daintreehq/assistant/internal/tools/grant"
	"github.com/daintreehq/assistant/internal/tools/mcpwrap"
	"github.com/daintreehq/assistant/internal/tools/mcpx"
	"github.com/daintreehq/assistant/internal/tools/memory"
	"github.com/daintreehq/assistant/internal/tools/questionx"
	queuetools "github.com/daintreehq/assistant/internal/tools/queue"
	"github.com/daintreehq/assistant/internal/tools/runbook"
	"github.com/daintreehq/assistant/internal/tools/scratchx"
	"github.com/daintreehq/assistant/internal/tools/subagentx"
	"github.com/daintreehq/assistant/internal/tools/timer"
	"github.com/daintreehq/assistant/internal/tools/watcher"
	"github.com/daintreehq/assistant/internal/tools/workflow"
)

// DefaultToolBuilder is the real ToolBuilder: it constructs every tool family's
// Deps from the App's already-built providers (Store, MCP client, backend client,
// Queue, agent session) — bridging each consumer interface through the
// adapters in toolfamilies.go / toolterminal.go — and aggregates all their tools
// into the []*tools.Tool the registry consumes. Families that return []tools.Tool
// (value slices) are normalized to []*tools.Tool by addr(); families that already
// return []*tools.Tool pass straight through. The builder runs after the registry
// exists but before AssertSafe, which then enforces the no-file-edit invariant over
// the whole wired set.
//
// SEAM: CreateOptions.BuildTools still overrides this (tests inject a stripped set).
func DefaultToolBuilder(a *App) ([]*tools.Tool, error) {
	var all []*tools.Tool

	// Families returning value slices ([]tools.Tool) — take element addresses.
	all = append(all, addr(tools.SetRequires(agenttaskx.Tools(a.agentTaskDeps()), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(artifactx.Tools(artifactx.Deps{
		Store: artifactStoreAdapter{app: a},
	}))...)
	all = append(all, addr(tools.SetRequires(asyncx.Tools(asyncx.Deps{
		Reader:      terminalReaderAdapter{c: a.MCP},
		Sender:      asyncCommandSenderAdapter{c: a.MCP},
		Coordinator: a.asyncCoordinator,
		Store:       a.Store,
		SessionID:   a.SessionID,
		Observer:    a.terminalObs,
	}), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(auditx.Tools(auditx.Deps{
		Store: auditStoreAdapter{s: a.Store},
	}))...)
	all = append(all, addr(tools.SetRequires(contextx.Tools(contextx.Deps{
		MCP:    contextMCPAdapter{c: a.MCP},
		Router: contextRouterAdapter{tasks: a.Backend},
		Queue:  contextQueueAdapter{app: a},
		// Read through the Swappable every call so a replaced client is reflected
		// immediately, rather than pinning whatever URL was current at wiring time.
		// Sanitized like any other model-visible endpoint — defence in depth. Every
		// endpoint source now passes backend.NormalizeBaseURL at startup, which refuses
		// userinfo outright, so a credential no longer reaches this far. It stays
		// sanitized because the value lands in a tool result the model may quote back,
		// and that is not a place to depend on a check made somewhere else.
		BackendURL: func() string {
			if a.Backend == nil {
				return ""
			}
			return mcp.SanitizeURL(a.Backend.BaseURL())
		},
	}), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(tools.SetRequires(extractionx.Tools(extractionx.Deps{
		Reader:       terminalReaderAdapter{c: a.MCP},
		Router:       extractionRouterAdapter{tasks: a.Backend},
		Supervisors:  supervisorRetireAdapter{app: a},
		Observations: a.terminalObs,
	}), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(fsx.Tools(fsx.Deps{}))...)
	all = append(all, addr(tools.SetRequires(mcpx.Tools(mcpx.Deps{
		MCP:      mcpxMCPAdapter{c: a.MCP},
		Observer: a.terminalObs,
		// mcpwrap's wrappers are named for the raw MCP actions they govern, so
		// tool.schema must know about them to warn that the local tool — not the raw
		// schema it is handing back — is what the model actually calls. mcpx cannot
		// import mcpwrap, so the names are injected here, where both are already held.
		WrapperNames: mcpwrapToolNames(),
	}), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(scratchx.Tools(scratchx.Deps{
		Store: a.scratchStore,
	}))...)
	// Delegation. RequiresBackend, not RequiresNothing: a sub-agent's rounds ARE
	// backend generations, so with the backend unreachable this tool can project and
	// dispatch but cannot do its job — exactly what that declaration is for. Its
	// local tool calls want Daintree MCP too, but the backend is the primary
	// dependency: without it there is no loop at all to make them from.
	all = append(all, addr(tools.SetRequires(subagentx.Tools(subagentx.Deps{
		Runner: a.newSubagentRunner(),
	}), tools.RequiresBackend))...)

	// Families already returning []*tools.Tool — pass through.
	all = append(all, grant.Tools(grant.Deps{
		Store: grantStoreAdapter{s: a.Store},
	})...)
	all = append(all, tools.SetRequiresPtr(mcpwrap.Tools(mcpwrap.Deps{
		Store:         mcpwrapWatcherStoreAdapter{s: a.Store},
		WorkflowStore: mcpwrapWorkflowStoreAdapter{s: a.Store},
	}), tools.RequiresDaintreeMCP)...)
	all = append(all, memory.Tools(memory.Deps{
		Store: memoryStoreAdapter{s: a.Store},
		// No OnChange: pinned memories live in the uncached turn footer now (issue #263),
		// re-read every round, so a model-driven pin/unpin/forget surfaces on the next round
		// automatically — no RefreshRuntimeContext rewrite of message[1] is needed.
	})...)
	all = append(all, queuetools.Tools(queuetools.Deps{
		Queue: queueToolAdapter{app: a},
	})...)
	// user.askMultipleChoice reaches the user purely through ToolContext.AskChoice, so
	// the family takes no deps. It is always registered (the interactive-only guard lives
	// in the handler + buildContext) so the projected toolset stays stable across runs.
	all = append(all, tools.SetRequiresPtr(questionx.Tools(questionx.Deps{}), tools.RequiresInteractive)...)
	all = append(all, runbook.Tools(runbook.Deps{
		Store:            runbookStoreAdapter{s: a.Store},
		CheckConsistency: a.checkRunbookStepConsistency,
	})...)
	all = append(all, timer.Tools(timer.Deps{
		Store:                timerStoreAdapter{Store: a.Store},
		PrepareScheduledCall: a.prepareScheduledCall,
	})...)
	all = append(all, watcher.Tools(watcher.Deps{
		Store: watcherStoreAdapter{s: a.Store},
	})...)
	all = append(all, workflow.Tools(workflow.Deps{
		Store: workflowStoreAdapter{s: a.Store},
		// Graph is nil when DAINTREE_WORKFLOW_INTELLIGENCE=0, in which case the
		// seven execution-graph tools register alongside the flat ledger tools.
		Graph: a.workflowGraphToolService(),
	})...)

	// Per-tool overrides, for the families whose members do NOT share one dependency.
	//
	// The watcher family is store-only to CREATE, so it registers as local — but a
	// terminal or PR watcher that can never observe anything is not "working", it is
	// bookkeeping. The engine polls through the Daintree control plane, so without MCP
	// these two produce a durable row and then nothing. Saying so here is what stops the
	// degraded-mode banner from repeating the old, false claim that watchers work
	// normally offline. watcher.list / watcher.cancel really are local and stay so.
	if err := markRequires(all, tools.RequiresDaintreeMCP, "watcher.terminal.create", "watcher.watchPR"); err != nil {
		return nil, err
	}

	// Tools whose FAMILY reaches Daintree but which themselves do not, so they must stay
	// listed as available in degraded mode. Every one of these is something a
	// disconnected user actively needs:
	//
	//   async.list / async.cancel   read and cancel the LOCAL ledger. Stopping
	//                               supervision you can no longer perform is the whole
	//                               point of being able to call them while offline.
	//   agentTask.status / .list    read spawn sagas straight out of SQLite.
	//   context.snapshot            explicitly best-effort: it reports the outage as
	//                               part of its answer instead of failing.
	//   daintree.status             the diagnostic FOR the outage. Marking it MCP-bound
	//                               would tell a disconnected user their only link
	//                               diagnostic is unavailable.
	if err := markRequires(all, tools.RequiresNothing,
		"async.list", "async.cancel",
		"agentTask.status", "agentTask.list",
		"context.snapshot", "daintree.status",
	); err != nil {
		return nil, err
	}

	// The graph planner and reconciler are the two workflow tools that call a
	// server-owned backend task; the rest of the family is a local ledger.
	if a.Config.WorkflowIntelligence {
		if err := markRequires(all, tools.RequiresBackend, "workflow.plan", "workflow.reconcile"); err != nil {
			return nil, err
		}
	}

	return all, nil
}

// markRequires stamps a connection dependency onto named tools, and ERRORS on a name
// that is not registered.
//
// Erroring matters: a silent no-op would make a typo here indistinguishable from a
// correct override, leaving the tool with its family's default forever. Since the
// defaults are what these calls exist to correct, a typo produces exactly the wrong
// answer in the generated reference and the degraded-mode banner — and nothing would
// ever fail. The builder already returns an error, so boot surfaces it immediately.
func markRequires(all []*tools.Tool, c tools.Connection, names ...string) error {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	for _, t := range all {
		if want[t.Name] {
			t.Requires = c
			delete(want, t.Name)
		}
	}
	if len(want) == 0 {
		return nil
	}
	missing := make([]string, 0, len(want))
	for n := range want {
		missing = append(missing, n)
	}
	sort.Strings(missing)
	return fmt.Errorf("markRequires(%q): no such registered tool(s): %s — the override is a no-op, fix or remove it",
		c, strings.Join(missing, ", "))
}

// addr normalizes a family's []tools.Tool (value slice) into []*tools.Tool by
// taking the address of each element. The loop var is re-bound per iteration so the
// pointers don't all alias the final element.
func addr(in []tools.Tool) []*tools.Tool {
	out := make([]*tools.Tool, 0, len(in))
	for i := range in {
		out = append(out, &in[i])
	}
	return out
}

// mcpwrapToolNames lists the mcpwrap family's tool names, for the tool.schema wrapper
// annotation (see the mcpx.Deps.WrapperNames comment). Built from the family's own
// registration rather than a hand-kept list, so a wrapper added later is covered without
// anyone remembering this exists. Deps are irrelevant — only names are read.
func mcpwrapToolNames() []string {
	list := mcpwrap.Tools(mcpwrap.Deps{})
	names := make([]string, 0, len(list))
	for _, t := range list {
		names = append(names, t.Name)
	}
	return names
}

// agentTaskDeps builds the spawn family's dependencies. Extracted from the builder so
// a test can assert the WIRING rather than only the logic: every agenttaskx unit test
// constructs its own Deps, so a field left unset here is invisible to all of them.
// One field has already been missed that way (WorktreePin), and the consequence was
// not a degraded default but a hard rejection at the host.
func (a *App) agentTaskDeps() agenttaskx.Deps {
	return agenttaskx.Deps{
		MCP:          agentTaskMCPAdapter{c: a.MCP},
		DB:           a.Store,
		Config:       a.Config,
		DaemonActive: func() bool { return a.scheduler != nil },
		// The SAME pin object the Session drives (BeginTurn/Offer), not a copy: two
		// pins would mean the spawn tool defaulting from a binding nothing ever set.
		WorktreePin: a.worktreePin,
		// Read live, not captured: the roster is re-read on reconnect and on a project
		// switch, and a spawn late in the session must default to the agent the user has
		// configured NOW, not the one cached when the tool table was built.
		DefaultAgent: a.DefaultDirectAgentID,
		// The builder runs once during App.Create, before any turn can spawn — so
		// "now" is an exact lower bound for this session's launches.
		SessionStartedAt: domain.NowMS(),
	}
}

// prepareScheduledCall resolves a scheduled tool call to the name fire-time dispatch
// will look up, and asks the tool whether these arguments could ever work with nobody
// present. Returns ("", reason) to refuse, or (canonicalName, "") to allow.
//
// It runs THREE checks, in the order the fire-time path runs them, because each one is
// a way the old single check let a doomed timer through:
//
//  1. The name resolves to a registered tool. Dispatch looks up the exact internal
//     name and resolves nothing, so a wire spelling or a stray space is a stored timer
//     that dies with UNKNOWN_TOOL — and an unknown name used to schedule cleanly,
//     since a tool nobody could find raised no objection.
//  2. The tool's own decoder accepts the arguments. A spawn with no taskPrompt, or a
//     bad mode enum, is refused at fire time by the same Decode; running it here moves
//     that refusal to where it can be acted on.
//  3. The tool's unattended preflight, on the DECODED args, so it sees defaults and
//     coercions exactly as the handler will.
//
// Resolved through the registry at CALL time, not at construction: this closure is
// handed to the timer family while the registry is still being filled with the very
// tools it will later look up.
func (a *App) prepareScheduledCall(toolName string, args json.RawMessage) (string, string) {
	if a == nil || a.Registry == nil {
		return toolName, ""
	}
	name := strings.TrimSpace(toolName)
	tool := a.Registry.Get(name)
	if tool == nil {
		// The model may have written the WIRE spelling. Resolving it here is not
		// leniency — the resolved name is what gets STORED, so the payload dispatch
		// later looks up is the one that exists.
		if resolved := a.Registry.ResolveWireName(name); resolved != "" {
			name = resolved
			tool = a.Registry.Get(name)
		}
	}
	if tool == nil {
		return "", fmt.Sprintf("no tool named %q is registered, so nothing would run when it fires", toolName)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if tool.Decode != nil {
		parsed, err := tool.Decode(args)
		if err != nil {
			return "", fmt.Sprintf("its arguments are not valid for %s: %s", name, err.Error())
		}
		args = parsed
	}
	if tool.PreflightUnattended != nil {
		if why := tool.PreflightUnattended(args); why != "" {
			return "", why
		}
	}
	return name, ""
}
