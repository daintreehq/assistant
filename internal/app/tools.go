package app

import (
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
	"github.com/daintreehq/assistant/internal/tools/docsx"
	"github.com/daintreehq/assistant/internal/tools/extractionx"
	"github.com/daintreehq/assistant/internal/tools/fsx"
	"github.com/daintreehq/assistant/internal/tools/grant"
	"github.com/daintreehq/assistant/internal/tools/mcpwrap"
	"github.com/daintreehq/assistant/internal/tools/mcpx"
	"github.com/daintreehq/assistant/internal/tools/memory"
	"github.com/daintreehq/assistant/internal/tools/questionx"
	queuetools "github.com/daintreehq/assistant/internal/tools/queue"
	"github.com/daintreehq/assistant/internal/tools/scratchx"
	"github.com/daintreehq/assistant/internal/tools/skill"
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
	all = append(all, addr(tools.SetRequires(agenttaskx.Tools(agenttaskx.Deps{
		MCP:          agentTaskMCPAdapter{c: a.MCP},
		DB:           a.Store,
		Config:       a.Config,
		DaemonActive: func() bool { return a.scheduler != nil },
		// The builder runs once during App.Create, before any turn can spawn — so
		// "now" is an exact lower bound for this session's launches.
		SessionStartedAt: domain.NowMS(),
	}), tools.RequiresDaintreeMCP))...)
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
		// Read through the Swappable every call so a /login endpoint change is reflected
		// immediately, rather than pinning whatever URL was current at wiring time.
		// Sanitized like any other model-visible endpoint: a custom backend URL comes
		// from the trusted env / stored sign-in, which never passes through
		// credentials.NormalizeBaseURL when it arrives as an override, so it can carry
		// userinfo straight into a tool result the model may quote back.
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
	}), tools.RequiresDaintreeMCP))...)
	all = append(all, addr(scratchx.Tools(scratchx.Deps{
		Store: a.scratchStore,
	}))...)

	// Families already returning []*tools.Tool — pass through.
	all = append(all, grant.Tools(grant.Deps{
		Store: grantStoreAdapter{s: a.Store},
	})...)
	all = append(all, tools.SetRequiresPtr(mcpwrap.Tools(mcpwrap.Deps{
		Store:         mcpwrapWatcherStoreAdapter{s: a.Store},
		WorkflowStore: mcpwrapWorkflowStoreAdapter{s: a.Store},
	}), tools.RequiresDaintreeMCP)...)
	// The docs family reaches the SECOND, public no-auth docs MCP (a.DocsMCP) — for
	// answering "how do I use Daintree" help questions — not the primary control plane.
	all = append(all, tools.SetRequiresPtr(docsx.Tools(docsx.Deps{
		MCP: docsMCPAdapter{c: a.DocsMCP},
	}), tools.RequiresDocsMCP)...)
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
	all = append(all, skill.Tools(skill.Deps{
		Store:            skillStoreAdapter{s: a.Store},
		CheckConsistency: a.checkSkillStepConsistency,
	})...)
	all = append(all, timer.Tools(timer.Deps{
		Store: timerStoreAdapter{s: a.Store},
	})...)
	all = append(all, watcher.Tools(watcher.Deps{
		Store: watcherStoreAdapter{s: a.Store},
	})...)
	all = append(all, workflow.Tools(workflow.Deps{
		Store: workflowStoreAdapter{s: a.Store},
		// Graph is nil unless DAINTREE_WORKFLOW_INTELLIGENCE=1, in which case the
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
