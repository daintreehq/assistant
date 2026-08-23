package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

// The workflow-intelligence graph tools (workflow.plan / getGraph / next /
// attachResource / recordEvidence / reconcile / cancel). They extend — never
// replace — the flat workflow.create/get/list/update ledger tools, and are
// registered ONLY when Deps.Graph is wired (unless DAINTREE_WORKFLOW_INTELLIGENCE=0),
// so a disabled build's toolset is byte-identical to before.

const codeGraphNotFound = "WORKFLOW_GRAPH_NOT_FOUND"

// GraphService is the slice of the workflowgraph service the tools call
// (satisfied by *workflowgraph.Service).
type GraphService interface {
	Plan(ctx context.Context, req workflowgraph.PlanRequest) (workflowgraph.PlanResult, error)
	Get(id string) (*workflowgraph.Graph, int64, error)
	Next(id string) (workflowgraph.NextResult, error)
	AttachResource(workflowID, nodeID string, res workflowgraph.Resource) (*workflowgraph.Graph, int64, error)
	RecordEvidence(workflowID, nodeID string, ev workflowgraph.EvidenceRef) (*workflowgraph.Graph, int64, error)
	Reconcile(ctx context.Context, req workflowgraph.ReconcileRequest) (workflowgraph.ReconcileResult, error)
	Cancel(workflowID, nodeID, reason string) (*workflowgraph.Graph, int64, error)
}

// graphTools returns the seven graph tools bound to svc.
func graphTools(svc GraphService) []*tools.Tool {
	return []*tools.Tool{
		newPlanTool(svc),
		newGetGraphTool(svc),
		newNextTool(svc),
		newAttachResourceTool(svc),
		newRecordEvidenceTool(svc),
		newReconcileTool(svc),
		newCancelGraphTool(svc),
	}
}

/* ------------------------------ workflow.plan ----------------------------- */

type planArgs struct {
	Goal               string   `json:"goal"`
	Scope              string   `json:"scope,omitempty"`
	ExistingWorkflowID string   `json:"existingWorkflowId,omitempty"`
	ForceReplan        bool     `json:"forceReplan,omitempty"`
	ActiveSkillIDs     []string `json:"activeSkillIds,omitempty"`
	Notes              []string `json:"notes,omitempty"`
}

func (a *planArgs) Validate() error {
	if strings.TrimSpace(a.Goal) == "" {
		return fmt.Errorf("goal is required")
	}
	return nil
}

var planSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["goal"],
  "properties": {
    "goal": { "type": "string", "description": "The user's goal, verbatim or lightly summarized." },
    "scope": { "type": "string", "description": "Optional bounds/constraints on the work." },
    "existingWorkflowId": { "type": "string", "description": "wfg_… id of a prior graph this goal relates to." },
    "forceReplan": { "type": "boolean", "description": "Replace the existing (non-terminal) workflow with a fresh plan. Without it, planning over a live workflow is refused — reconcile instead." },
    "activeSkillIds": { "type": "array", "items": { "type": "string" }, "description": "Currently-loaded skill ids, so the plan follows their playbooks." },
    "notes": { "type": "array", "items": { "type": "string" }, "description": "Extra constraints or facts the planner should honour." }
  }
}`)

func newPlanTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.plan",
		Description: "Create a durable workflow execution graph for a multi-step goal. The backend plans it; the plan is validated and stored locally, survives restarts, and its progress digest is shown to you every round. Use for any task with several dependent steps (delegate → supervise → verify → report). Example: {\"goal\": \"Fix the watcher tests and report when the branch is clean\"}. Returns the workflowId (wfg_…), the active nodes, and a recommended next action — which you still execute through normal tools.",
		Risk:        domain.RiskLocal,
		Schema:      planSchema,
		Decode:      tools.StrictDecoder(func() any { return &planArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a planArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.plan: "+err.Error())
			}
			res, err := svc.Plan(ctx, workflowgraph.PlanRequest{
				Goal:               a.Goal,
				Scope:              a.Scope,
				ExistingWorkflowID: a.ExistingWorkflowID,
				ForceReplan:        a.ForceReplan,
				ActiveSkillIDs:     a.ActiveSkillIDs,
				Notes:              a.Notes,
				Source:             workflowgraph.SourceUser,
			})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.plan: "+err.Error())
			}
			out := map[string]any{
				"workflowId":    res.Graph.ID,
				"status":        res.Graph.Status,
				"summary":       planSummary(res.Graph),
				"activeNodeIds": res.Graph.ActiveNodeIDs,
				"nodes":         nodeViews(res.Graph),
			}
			if res.Graph.NextAction != nil {
				out["nextAction"] = res.Graph.NextAction
			}
			if len(res.Warnings) > 0 {
				out["warnings"] = res.Warnings
			}
			if res.UserQuestion != nil {
				out["userQuestion"] = res.UserQuestion
			}
			if res.SupersededID != "" {
				out["supersededWorkflowId"] = res.SupersededID
			}
			return tools.Ok(fmt.Sprintf("Planned workflow %s with %d nodes.", res.Graph.ID, len(res.Graph.Nodes)), out)
		},
	}
}

func planSummary(g *workflowgraph.Graph) string {
	return fmt.Sprintf("%s — %d nodes, %d ready", g.Goal, len(g.Nodes), len(workflowgraph.ReadyNodes(g)))
}

/* ---------------------------- workflow.getGraph --------------------------- */

type getGraphArgs struct {
	ID   string `json:"id"`
	View string `json:"view,omitempty"`
}

func (a *getGraphArgs) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("id is required")
	}
	switch a.View {
	case "", "compact", "full":
	default:
		return fmt.Errorf("view must be \"compact\" or \"full\"")
	}
	return nil
}

var getGraphSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string", "description": "The wfg_… workflow graph id." },
    "view": { "type": "string", "enum": ["compact", "full"], "description": "compact (default): progress, active nodes, blockers, next action. full: every node, edge, resource, and evidence item." }
  }
}`)

func newGetGraphTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.getGraph",
		Description: "Read a workflow execution graph (wfg_… id). Use view \"compact\" for a quick progress check, \"full\" when you need exact node/edge/resource state. Never guess graph state — read it here when it matters.",
		Risk:        domain.RiskRead,
		Schema:      getGraphSchema,
		Decode:      tools.StrictDecoder(func() any { return &getGraphArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a getGraphArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.getGraph: "+err.Error())
			}
			g, revision, err := svc.Get(a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.getGraph: "+err.Error())
			}
			if g == nil {
				return tools.Fail(codeGraphNotFound, "workflow.getGraph: no such workflow graph: "+a.ID, tools.Unrecoverable())
			}
			if a.View == "full" {
				return tools.Ok("Workflow graph "+a.ID+" (full).",
					map[string]any{"graph": g, "revision": revision})
			}
			return tools.Ok("Workflow graph "+a.ID+".", compactGraphView(g, revision))
		},
	}
}

/* ------------------------------ workflow.next ----------------------------- */

var nextSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string", "description": "The wfg_… workflow graph id." }
  }
}`)

func newNextTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.next",
		Description: "Compute what can run NOW for a workflow graph: the ready nodes (dependencies satisfied), the nodes still waiting on async work, open blockers, and the stored recommended next action. Call it when unsure what to do next in a tracked workflow.",
		Risk:        domain.RiskRead,
		Schema:      nextSchema,
		Decode:      tools.StrictDecoder(func() any { return &getGraphArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a getGraphArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.next: "+err.Error())
			}
			res, err := svc.Next(a.ID)
			if err != nil {
				return tools.Fail(codeGraphNotFound, "workflow.next: "+err.Error(), tools.Unrecoverable())
			}
			out := map[string]any{
				"workflowId":   res.WorkflowID,
				"status":       res.Status,
				"readyNodes":   nodeSummaries(res.ReadyNodes),
				"waitingNodes": nodeSummaries(res.WaitingNodes),
			}
			if res.NextAction != nil {
				out["nextAction"] = res.NextAction
			}
			if len(res.OpenBlockers) > 0 {
				out["openBlockers"] = res.OpenBlockers
			}
			return tools.Ok(fmt.Sprintf("%d node(s) ready, %d waiting.", len(res.ReadyNodes), len(res.WaitingNodes)), out)
		},
	}
}

/* ------------------------- workflow.attachResource ------------------------ */

type attachArgs struct {
	WorkflowID string `json:"workflowId"`
	NodeID     string `json:"nodeId,omitempty"`
	Type       string `json:"type"`
	Ref        string `json:"ref"`
	Label      string `json:"label,omitempty"`
	Status     string `json:"status,omitempty"`
}

func (a *attachArgs) Validate() error {
	if strings.TrimSpace(a.WorkflowID) == "" {
		return fmt.Errorf("workflowId is required")
	}
	if !workflowgraph.ValidResourceTypes[a.Type] {
		return fmt.Errorf("invalid resource type %q", a.Type)
	}
	if strings.TrimSpace(a.Ref) == "" {
		return fmt.Errorf("ref is required")
	}
	return nil
}

var attachSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["workflowId", "type", "ref"],
  "properties": {
    "workflowId": { "type": "string", "description": "The wfg_… workflow graph id." },
    "nodeId": { "type": "string", "description": "Node to link the resource to (omit for graph-level)." },
    "type": { "type": "string", "enum": ["terminal","agent","worktree","branch","pr","issue","watcher","timer","async","queue_event","artifact","memory","grant"] },
    "ref": { "type": "string", "description": "The resource's identifier, e.g. a terminal-… id, wch_… id, asy_… handle, branch name, or PR URL." },
    "label": { "type": "string", "description": "Short human label." },
    "status": { "type": "string", "description": "Resource state, e.g. active, done." }
  }
}`)

func newAttachResourceTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.attachResource",
		Description: "Link a terminal, watcher, async handle, worktree, branch, PR, or other resource to a workflow graph node, so later events on that resource route back to the right step. Resources created while a single workflow is active are usually linked automatically — attach manually when you spawned something for a specific node. Example: {\"workflowId\": \"wfg_1a2b3c4d\", \"nodeId\": \"n_repair\", \"type\": \"terminal\", \"ref\": \"terminal-…\", \"label\": \"Claude: repair watcher tests\"}.",
		Risk:        domain.RiskLocal,
		Schema:      attachSchema,
		Decode:      tools.StrictDecoder(func() any { return &attachArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a attachArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.attachResource: "+err.Error())
			}
			g, _, err := svc.AttachResource(a.WorkflowID, a.NodeID, workflowgraph.Resource{
				Type: a.Type, Ref: a.Ref, Label: a.Label, Status: a.Status,
			})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.attachResource: "+err.Error())
			}
			return tools.Ok(fmt.Sprintf("Linked %s %s to %s.", a.Type, a.Ref, a.WorkflowID),
				map[string]any{"workflowId": g.ID, "resources": len(g.Resources)})
		},
	}
}

/* ------------------------- workflow.recordEvidence ------------------------ */

type evidenceArgs struct {
	WorkflowID string `json:"workflowId"`
	NodeID     string `json:"nodeId,omitempty"`
	Kind       string `json:"kind"`
	Summary    string `json:"summary"`
	RefID      string `json:"refId,omitempty"`
}

func (a *evidenceArgs) Validate() error {
	if strings.TrimSpace(a.WorkflowID) == "" {
		return fmt.Errorf("workflowId is required")
	}
	if !workflowgraph.ValidEvidenceKinds[a.Kind] {
		return fmt.Errorf("invalid evidence kind %q", a.Kind)
	}
	if strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	return nil
}

var evidenceSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["workflowId", "kind", "summary"],
  "properties": {
    "workflowId": { "type": "string", "description": "The wfg_… workflow graph id." },
    "nodeId": { "type": "string", "description": "Node the evidence belongs to (omit for graph-level)." },
    "kind": { "type": "string", "enum": ["tool_call","tool_result","queue_event","watcher_event","async_completion","user_message","assistant_message","manual_note"] },
    "summary": { "type": "string", "description": "One bounded sentence of what happened / was learned." },
    "refId": { "type": "string", "description": "Id of the source object (tool call id, evt_…, asy_…)." }
  }
}`)

func newRecordEvidenceTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.recordEvidence",
		Description: "Record one bounded evidence item against a workflow graph node — an observation, outcome, or decision-relevant fact the graph should remember (mutating tool results are captured automatically; use this for things only you observed). Keep the summary to one sentence.",
		Risk:        domain.RiskLocal,
		Schema:      evidenceSchema,
		Decode:      tools.StrictDecoder(func() any { return &evidenceArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a evidenceArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.recordEvidence: "+err.Error())
			}
			g, _, err := svc.RecordEvidence(a.WorkflowID, a.NodeID, workflowgraph.EvidenceRef{
				Kind: a.Kind, Summary: a.Summary, RefID: a.RefID,
			})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.recordEvidence: "+err.Error())
			}
			return tools.Ok("Recorded evidence on "+a.WorkflowID+".",
				map[string]any{"workflowId": g.ID, "evidenceCount": len(g.Evidence)})
		},
	}
}

/* --------------------------- workflow.reconcile --------------------------- */

type reconcileArgs struct {
	WorkflowID        string `json:"workflowId"`
	Reason            string `json:"reason"`
	RecentEventLimit  int    `json:"recentEventLimit,omitempty"`
	LatestUserMessage string `json:"latestUserMessage,omitempty"`
	Apply             *bool  `json:"apply,omitempty"`
}

func (a *reconcileArgs) Validate() error {
	if strings.TrimSpace(a.WorkflowID) == "" {
		return fmt.Errorf("workflowId is required")
	}
	if !workflowgraph.ValidReconcileReasons[a.Reason] {
		return fmt.Errorf("invalid reason %q (tool_result|queue_event|wake|manual|resume|failure|user_interjection)", a.Reason)
	}
	if a.RecentEventLimit < 0 {
		return fmt.Errorf("recentEventLimit must be ≥ 0")
	}
	return nil
}

var reconcileSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["workflowId", "reason"],
  "properties": {
    "workflowId": { "type": "string", "description": "The wfg_… workflow graph id." },
    "reason": { "type": "string", "enum": ["tool_result","queue_event","wake","manual","resume","failure","user_interjection"] },
    "recentEventLimit": { "type": "integer", "description": "How many recent workflow events to include (default 20, max 50)." },
    "latestUserMessage": { "type": "string", "description": "The most recent user message, when it should steer the reconcile." },
    "apply": { "type": "boolean", "description": "Default true. false previews the patch without committing it." }
  }
}`)

func newReconcileTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.reconcile",
		Description: "Reconcile a workflow graph against what actually happened: recent tool results, async completions, failures, and blockers are read by the backend, which returns a validated PATCH (node transitions, blockers, next action) that is applied locally. Call it after a meaningful change — a failed tool result, an async completion, a wake, resuming stale work — instead of improvising or repeating calls. It patches the plan; it never re-plans (use workflow.plan with forceReplan for a goal change) and never executes anything itself.",
		Risk:        domain.RiskLocal,
		Schema:      reconcileSchema,
		Decode:      tools.StrictDecoder(func() any { return &reconcileArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a reconcileArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.reconcile: "+err.Error())
			}
			apply := true
			if a.Apply != nil {
				apply = *a.Apply
			}
			res, err := svc.Reconcile(ctx, workflowgraph.ReconcileRequest{
				WorkflowID:        a.WorkflowID,
				Reason:            a.Reason,
				RecentEventLimit:  a.RecentEventLimit,
				LatestUserMessage: a.LatestUserMessage,
				Apply:             apply,
			})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.reconcile: "+err.Error())
			}
			out := compactGraphView(res.Graph, res.Revision)
			out["applied"] = res.Applied
			if res.Summary != "" {
				out["reconcileSummary"] = res.Summary
			}
			if len(res.Warnings) > 0 {
				out["warnings"] = res.Warnings
			}
			if res.NextAction != nil {
				out["nextAction"] = res.NextAction
			}
			if res.UserQuestion != nil {
				out["userQuestion"] = res.UserQuestion
			}
			verb := "Reconciled"
			if !res.Applied {
				verb = "Previewed reconcile for"
			}
			return tools.Ok(fmt.Sprintf("%s workflow %s.", verb, a.WorkflowID), out)
		},
	}
}

/* ----------------------------- workflow.cancel ---------------------------- */

type cancelArgs struct {
	WorkflowID string `json:"workflowId"`
	NodeID     string `json:"nodeId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (a *cancelArgs) Validate() error {
	if strings.TrimSpace(a.WorkflowID) == "" {
		return fmt.Errorf("workflowId is required")
	}
	return nil
}

var cancelSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["workflowId"],
  "properties": {
    "workflowId": { "type": "string", "description": "The wfg_… workflow graph id." },
    "nodeId": { "type": "string", "description": "Cancel just this node (omit to cancel the whole workflow)." },
    "reason": { "type": "string", "description": "Why it is being cancelled." }
  }
}`)

func newCancelGraphTool(svc GraphService) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.cancel",
		Description: "Cancel a workflow graph (or one node of it). LOCAL STATE ONLY: this never closes terminals, deletes worktrees, or cancels external work — those remain explicit, user-approved actions. Suggest any needed cleanup separately.",
		Risk:        domain.RiskLocal,
		Schema:      cancelSchema,
		Decode:      tools.StrictDecoder(func() any { return &cancelArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a cancelArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.cancel: "+err.Error())
			}
			g, revision, err := svc.Cancel(a.WorkflowID, a.NodeID, a.Reason)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.cancel: "+err.Error())
			}
			what := "workflow " + a.WorkflowID
			if a.NodeID != "" {
				what = "node " + a.NodeID + " of " + a.WorkflowID
			}
			return tools.Ok("Cancelled "+what+" (graph state only — no terminals or external work were touched).",
				compactGraphView(g, revision))
		},
	}
}

/* --------------------------------- views ---------------------------------- */

// compactGraphView is the bounded default read shape: enough to act on, never
// the whole evidence log.
func compactGraphView(g *workflowgraph.Graph, revision int64) map[string]any {
	view := map[string]any{
		"workflowId": g.ID,
		"goal":       g.Goal,
		"status":     g.Status,
		"revision":   revision,
		"progress":   fmt.Sprintf("%d/%d nodes done", g.DoneNodeCount(), len(g.Nodes)),
		"nodes":      nodeViews(g),
	}
	if cur := g.CurrentNode(); cur != nil {
		view["currentNode"] = map[string]any{"id": cur.ID, "title": cur.Title, "status": cur.Status}
	}
	if g.NextAction != nil {
		view["nextAction"] = g.NextAction
	}
	if blockers := g.OpenBlockers(); len(blockers) > 0 {
		view["openBlockers"] = blockers
	}
	if len(g.Resources) > 0 {
		res := make([]map[string]any, 0, len(g.Resources))
		for i := range g.Resources {
			r := &g.Resources[i]
			m := map[string]any{"type": r.Type, "ref": r.Ref}
			if r.NodeID != "" {
				m["nodeId"] = r.NodeID
			}
			if r.Status != "" {
				m["status"] = r.Status
			}
			res = append(res, m)
		}
		view["resources"] = res
	}
	return view
}

// nodeViews is the per-node compact projection.
func nodeViews(g *workflowgraph.Graph) []map[string]any {
	out := make([]map[string]any, 0, len(g.Nodes))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		m := map[string]any{"id": n.ID, "title": n.Title, "kind": n.Kind, "status": n.Status}
		if len(n.DependsOn) > 0 {
			m["dependsOn"] = n.DependsOn
		}
		if n.ToolName != "" {
			m["toolName"] = n.ToolName
		}
		if n.LastError != "" {
			m["lastError"] = n.LastError
		}
		out = append(out, m)
	}
	return out
}

// nodeSummaries projects a node slice to compact maps.
func nodeSummaries(nodes []workflowgraph.Node) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		m := map[string]any{"id": n.ID, "title": n.Title, "kind": n.Kind}
		if n.ToolName != "" {
			m["toolName"] = n.ToolName
		}
		out = append(out, m)
	}
	return out
}
