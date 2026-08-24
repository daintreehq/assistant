package app

import (
	"context"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/tools"
	toolworkflow "github.com/daintreehq/assistant/internal/tools/workflow"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

// Workflow-intelligence wiring (on unless DAINTREE_WORKFLOW_INTELLIGENCE=0): the
// workflowgraph.Service is constructed once in Create and threaded into four
// seams — the workflow tool family (graph tools), the registry's dispatch
// observer (automatic evidence capture), the async coordinator's WorkflowSink
// (completion → node transition), and the session's turn-context digest
// lister. Every helper here is nil-safe: a disabled feature leaves all four
// seams nil and the runtime byte-identical to before.

// buildWorkflowService constructs the graph service when the feature flag is
// on ((nil, nil) when off). Registry-dependent closures deref a.Registry
// LAZILY: the service is built before the registry (the coordinator needs its
// sink at construction), but Plan/Reconcile only consult the registry long
// after boot completed.
func (a *App) buildWorkflowService() (*workflowgraph.Service, error) {
	if !a.Config.WorkflowIntelligence {
		return nil, nil
	}
	traceCfg := debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir}
	return workflowgraph.NewService(workflowgraph.Deps{
		Store: a.Store,
		Tasks: a.Backend,
		HasTool: func(name string) bool {
			if r := a.Registry; r != nil {
				return r.Has(name)
			}
			return false
		},
		ToolInventory:  a.workflowToolInventory,
		RuntimeSummary: a.workflowRuntimeSummary,
		Trace: func(event string, fields map[string]any) {
			debuglog.LogDebug(traceCfg, event, fields)
		},
	})
}

// workflowToolInventory projects the live registry to the (name, risk) list
// the planning tasks receive, in registration order (deterministic).
func (a *App) workflowToolInventory() []backend.WorkflowToolInfo {
	r := a.Registry
	if r == nil {
		return nil
	}
	list := r.List()
	out := make([]backend.WorkflowToolInfo, 0, len(list))
	for _, t := range list {
		out = append(out, backend.WorkflowToolInfo{Name: t.Name, Risk: string(t.Risk)})
	}
	return out
}

// workflowRuntimeSummary snapshots the runtime facts the planning tasks
// receive — a bounded map, never the full prompt context.
func (a *App) workflowRuntimeSummary() map[string]any {
	pc := a.PromptContext()
	return map[string]any{
		"project_id":       pc.ProjectID,
		"permission_tier":  string(pc.Tier),
		"mcp_connected":    pc.MCPConnected,
		"scheduler_active": pc.SchedulerActive,
		"active_worktree":  a.activeWorktreeForFooter(),
	}
}

// WorkflowGraphs exposes the workflow-intelligence service to the UI/commands
// surfaces (nil when the feature is off — callers must nil-check).
func (a *App) WorkflowGraphs() *workflowgraph.Service { return a.workflowService }

// workflowGraphToolService adapts the concrete service to the tool family's
// consumer interface, preserving nil (a typed-nil interface would defeat the
// family's Graph == nil registration gate).
func (a *App) workflowGraphToolService() toolworkflow.GraphService {
	if a.workflowService == nil {
		return nil
	}
	return a.workflowService
}

// workflowDigestLister exposes the service as the session's turn-context seam,
// preserving nil for the disabled case.
func (a *App) workflowDigestLister() agent.WorkflowDigestLister {
	if a.workflowService == nil {
		return nil
	}
	return a.workflowService
}

// workflowObserverAdapter maps the registry's DispatchObservation onto the
// workflowgraph package's ObservedCall (so workflowgraph never imports the
// tools package). Observation is synchronous but bounded to fast local writes;
// the registry already panic-guards the call.
type workflowObserverAdapter struct{ svc *workflowgraph.Service }

func (o workflowObserverAdapter) ObserveDispatch(_ context.Context, obs tools.DispatchObservation) {
	o.svc.ObserveDispatch(workflowgraph.ObservedCall{
		ToolName:   obs.ToolName,
		Args:       obs.Args,
		Result:     obs.Result,
		Risk:       string(obs.Risk),
		Outcome:    obs.Outcome,
		Actor:      string(obs.Actor),
		RunID:      obs.RunID,
		ToolCallID: obs.ToolCallID,
	})
}
