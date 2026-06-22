package app

import (
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/tools"

	"github.com/daintreehq/daintree-assistant/internal/tools/agenttaskx"
	"github.com/daintreehq/daintree-assistant/internal/tools/artifactx"
	"github.com/daintreehq/daintree-assistant/internal/tools/auditx"
	"github.com/daintreehq/daintree-assistant/internal/tools/contextx"
	"github.com/daintreehq/daintree-assistant/internal/tools/extractionx"
	"github.com/daintreehq/daintree-assistant/internal/tools/fsx"
	"github.com/daintreehq/daintree-assistant/internal/tools/grant"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpwrap"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpx"
	"github.com/daintreehq/daintree-assistant/internal/tools/memory"
	queuetools "github.com/daintreehq/daintree-assistant/internal/tools/queue"
	"github.com/daintreehq/daintree-assistant/internal/tools/skill"
	"github.com/daintreehq/daintree-assistant/internal/tools/timer"
	"github.com/daintreehq/daintree-assistant/internal/tools/watcher"
	"github.com/daintreehq/daintree-assistant/internal/tools/workflow"
)

// DefaultToolBuilder is the real ToolBuilder: it constructs every tool family's
// Deps from the App's already-built providers (Store, MCP client, Router, Queue,
// agent session, skills registry) — bridging each consumer interface through the
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
	}))...)
	all = append(all, addr(artifactx.Tools(artifactx.Deps{
		Store: artifactStoreAdapter{app: a},
	}))...)
	all = append(all, addr(auditx.Tools(auditx.Deps{
		Store: auditStoreAdapter{s: a.Store},
	}))...)
	all = append(all, addr(contextx.Tools(contextx.Deps{
		MCP:    contextMCPAdapter{c: a.MCP},
		Router: contextRouterAdapter{router: a.Router},
		Queue:  contextQueueAdapter{app: a},
	}))...)
	all = append(all, addr(extractionx.Tools(extractionx.Deps{
		Reader:      terminalReaderAdapter{c: a.MCP},
		Router:      extractionRouterAdapter{router: a.Router},
		Queue:       a.Queue,
		BaseContext: a.baseCtx,
		DebugLog:    debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
	}))...)
	all = append(all, addr(fsx.Tools(fsx.Deps{}))...)
	all = append(all, addr(mcpx.Tools(mcpx.Deps{
		MCP: mcpxMCPAdapter{c: a.MCP},
	}))...)

	// Families already returning []*tools.Tool — pass through.
	all = append(all, grant.Tools(grant.Deps{
		Store: grantStoreAdapter{s: a.Store},
	})...)
	all = append(all, mcpwrap.Tools(mcpwrap.Deps{
		Store: mcpwrapWatcherStoreAdapter{s: a.Store},
	})...)
	all = append(all, memory.Tools(memory.Deps{
		Store: memoryStoreAdapter{s: a.Store},
		// After the model pins/unpins/forgets a memory, refresh message[1] so the
		// injected pinned-memory block reflects the change immediately (mirrors how the
		// /memory slash command refreshes). a.Session is read lazily — it is set after
		// the registry is built, but the tool only fires during a live turn, where it is
		// always present.
		OnChange: func() {
			if a.Session != nil {
				a.Session.RefreshRuntimeContext(a.PromptContext())
			}
		},
	})...)
	all = append(all, queuetools.Tools(queuetools.Deps{
		Queue: queueToolAdapter{app: a},
	})...)
	all = append(all, skill.Tools(skill.Deps{
		Store:      skillStoreAdapter{s: a.Store},
		Source:     skillSourceAdapter{app: a},
		LoadSkills: a.loadSkills,
		FindSkills: a.skillFind,
	})...)
	all = append(all, timer.Tools(timer.Deps{
		Store: timerStoreAdapter{s: a.Store},
	})...)
	all = append(all, watcher.Tools(watcher.Deps{
		Store: watcherStoreAdapter{s: a.Store},
	})...)
	all = append(all, workflow.Tools(workflow.Deps{
		Store: workflowStoreAdapter{s: a.Store},
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
