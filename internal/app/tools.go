package app

import (
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/tools"

	"github.com/daintreehq/daintree-assistant/internal/tools/agenttaskx"
	"github.com/daintreehq/daintree-assistant/internal/tools/artifactx"
	"github.com/daintreehq/daintree-assistant/internal/tools/asyncx"
	"github.com/daintreehq/daintree-assistant/internal/tools/auditx"
	"github.com/daintreehq/daintree-assistant/internal/tools/contextx"
	"github.com/daintreehq/daintree-assistant/internal/tools/docsx"
	"github.com/daintreehq/daintree-assistant/internal/tools/extractionx"
	"github.com/daintreehq/daintree-assistant/internal/tools/fsx"
	"github.com/daintreehq/daintree-assistant/internal/tools/grant"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpwrap"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpx"
	"github.com/daintreehq/daintree-assistant/internal/tools/memory"
	"github.com/daintreehq/daintree-assistant/internal/tools/questionx"
	queuetools "github.com/daintreehq/daintree-assistant/internal/tools/queue"
	"github.com/daintreehq/daintree-assistant/internal/tools/scratchx"
	"github.com/daintreehq/daintree-assistant/internal/tools/skill"
	"github.com/daintreehq/daintree-assistant/internal/tools/timer"
	"github.com/daintreehq/daintree-assistant/internal/tools/watcher"
	"github.com/daintreehq/daintree-assistant/internal/tools/workflow"
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
	all = append(all, addr(agenttaskx.Tools(agenttaskx.Deps{
		MCP:          agentTaskMCPAdapter{c: a.MCP},
		DB:           a.Store,
		Config:       a.Config,
		DaemonActive: func() bool { return a.scheduler != nil },
		// The builder runs once during App.Create, before any turn can spawn — so
		// "now" is an exact lower bound for this session's launches.
		SessionStartedAt: domain.NowMS(),
	}))...)
	all = append(all, addr(artifactx.Tools(artifactx.Deps{
		Store: artifactStoreAdapter{app: a},
	}))...)
	all = append(all, addr(asyncx.Tools(asyncx.Deps{
		Reader:      terminalReaderAdapter{c: a.MCP},
		Sender:      asyncCommandSenderAdapter{c: a.MCP},
		Coordinator: a.asyncCoordinator,
		Store:       a.Store,
		SessionID:   a.SessionID,
		Observer:    a.terminalObs,
	}))...)
	all = append(all, addr(auditx.Tools(auditx.Deps{
		Store: auditStoreAdapter{s: a.Store},
	}))...)
	all = append(all, addr(contextx.Tools(contextx.Deps{
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
	}))...)
	all = append(all, addr(extractionx.Tools(extractionx.Deps{
		Reader:       terminalReaderAdapter{c: a.MCP},
		Router:       extractionRouterAdapter{tasks: a.Backend},
		Supervisors:  supervisorRetireAdapter{app: a},
		Observations: a.terminalObs,
	}))...)
	all = append(all, addr(fsx.Tools(fsx.Deps{}))...)
	all = append(all, addr(mcpx.Tools(mcpx.Deps{
		MCP:      mcpxMCPAdapter{c: a.MCP},
		Observer: a.terminalObs,
	}))...)
	all = append(all, addr(scratchx.Tools(scratchx.Deps{
		Store: a.scratchStore,
	}))...)

	// Families already returning []*tools.Tool — pass through.
	all = append(all, grant.Tools(grant.Deps{
		Store: grantStoreAdapter{s: a.Store},
	})...)
	all = append(all, mcpwrap.Tools(mcpwrap.Deps{
		Store:         mcpwrapWatcherStoreAdapter{s: a.Store},
		WorkflowStore: mcpwrapWorkflowStoreAdapter{s: a.Store},
	})...)
	// The docs family reaches the SECOND, public no-auth docs MCP (a.DocsMCP) — for
	// answering "how do I use Daintree" help questions — not the primary control plane.
	all = append(all, docsx.Tools(docsx.Deps{
		MCP: docsMCPAdapter{c: a.DocsMCP},
	})...)
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
	all = append(all, questionx.Tools(questionx.Deps{})...)
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

	return all, nil
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
