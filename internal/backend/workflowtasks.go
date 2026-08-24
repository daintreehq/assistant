package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Workflow-intelligence task IDs (server-owned prompts/models; the CLI sends
// data only). Availability is gated by DAINTREE_WORKFLOW_INTELLIGENCE and, at
// runtime, by Capabilities.Tasks — a backend without these tasks rejects them
// and the caller degrades gracefully.
const (
	TaskWorkflowPlan         = "workflow_plan"
	TaskWorkflowReconcile    = "workflow_reconcile"
	TaskWorkflowResumeDigest = "workflow_resume_digest"
)

// workflowTaskIDs is the flag-gated half of the frozen task manifest (see
// coreTaskIDs in tasks.go). CheckTasks requires these ONLY when
// DAINTREE_WORKFLOW_INTELLIGENCE is on — a backend that predates the workflow
// tasks is perfectly healthy for a CLI that will never call them. Kept in
// lockstep with the const block above by the AST scan in taskcheck_test.go.
var workflowTaskIDs = []string{
	TaskWorkflowPlan,
	TaskWorkflowReconcile,
	TaskWorkflowResumeDigest,
}

// WorkflowTaskIDs returns the workflow-intelligence task ids (a copy).
func WorkflowTaskIDs() []string { return append([]string(nil), workflowTaskIDs...) }

// Input size caps (mirror the backend pydantic max_length constraints; clamped
// client-side so a large goal/message can never 422 the task).
const (
	maxWorkflowGoalRunes    = 8_000
	maxWorkflowScopeRunes   = 4_000
	maxWorkflowMessageRunes = 8_000
)

// The wire DTOs below mirror the backend pydantic models in
// contracts/tasks.py EXACTLY (field names, optionality, types) — that file is
// the canonical schema, and the golden fixtures under testdata/workflow/ pin
// the byte shapes on both sides. Local graph structures keep their own naming;
// only this layer must match the backend.

// WorkflowToolInfo is one tool-inventory entry for planning/reconciliation:
// just enough for the backend to recommend REAL, callable tools (never the
// full schema — the plan prompt doesn't need it and the payload stays small).
type WorkflowToolInfo struct {
	Name string `json:"name"`
	Risk string `json:"risk,omitempty"`
}

// WorkflowPlanInput asks the backend to turn a goal + context into an
// executable graph. Field names mirror the backend WorkflowPlanInput pydantic
// model (contracts/tasks.py).
type WorkflowPlanInput struct {
	Goal             string             `json:"goal"`
	Scope            string             `json:"scope,omitempty"`
	RuntimeSummary   map[string]any     `json:"runtime_summary,omitempty"`
	ToolInventory    []WorkflowToolInfo `json:"tool_inventory,omitempty"`
	ActiveRunbookIDs   []string           `json:"active_runbook_ids,omitempty"`
	ExistingWorkflow *WorkflowSnapshot  `json:"existing_workflow,omitempty"`
	RelevantMemories []string           `json:"relevant_memories,omitempty"`
	OpenResources    []map[string]any   `json:"open_resources,omitempty"`
	Constraints      []string           `json:"constraints,omitempty"`
}

// WorkflowNodeOut is one planned node as the backend emits it (snake_case
// wire form; converted + validated locally before storage).
type WorkflowNodeOut struct {
	ID               string         `json:"id"`
	Title            string         `json:"title"`
	Kind             string         `json:"kind"`
	Status           string         `json:"status,omitempty"`
	DependsOn        []string       `json:"depends_on,omitempty"`
	ToolName         string         `json:"tool_name,omitempty"`
	ToolArgs         map[string]any `json:"tool_args,omitempty"`
	Risk             string         `json:"risk,omitempty"`
	RequiresConfirm  bool           `json:"requires_confirm,omitempty"`
	ExpectedEvidence []string       `json:"expected_evidence,omitempty"`
	AsyncPolicy      string         `json:"async_policy,omitempty"`
	Owner            string         `json:"owner,omitempty"`
}

// WorkflowEdgeOut is one dependency edge as the backend emits it: source →
// target ("target depends on source"). NEVER from/to — that spelling drifted
// once and silently dropped every edge.
type WorkflowEdgeOut struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// RecommendedActionOut is the backend's suggested next tool call. It carries
// ZERO execution authority — the client validates the tool exists and dispatch
// still gates every mutation.
type RecommendedActionOut struct {
	Label                string         `json:"label"`
	ToolName             string         `json:"tool_name,omitempty"`
	Args                 map[string]any `json:"args,omitempty"`
	Risk                 string         `json:"risk,omitempty"`
	RequiresConfirmation bool           `json:"requires_confirmation,omitempty"`
	NodeID               string         `json:"node_id,omitempty"`
}

// MultipleChoiceQuestionOut is a finite user decision the backend recommends
// asking (rendered through user.askMultipleChoice by the model, never directly).
type MultipleChoiceQuestionOut struct {
	Question   string   `json:"question"`
	Options    []string `json:"options,omitempty"`
	AllowOther bool     `json:"allow_other,omitempty"`
}

// WorkflowPlanOutput is the typed plan the backend returns.
type WorkflowPlanOutput struct {
	Goal            string                     `json:"goal"`
	Status          string                     `json:"status,omitempty"`
	SuccessCriteria []string                   `json:"success_criteria,omitempty"`
	Assumptions     []string                   `json:"assumptions,omitempty"`
	Nodes           []WorkflowNodeOut          `json:"nodes"`
	Edges           []WorkflowEdgeOut          `json:"edges,omitempty"`
	NextAction      *RecommendedActionOut      `json:"next_action,omitempty"`
	UserQuestion    *MultipleChoiceQuestionOut `json:"user_question,omitempty"`
	Warnings        []string                   `json:"warnings,omitempty"`
	Confidence      float64                    `json:"confidence,omitempty"`
}

// --------------------------------------------------------------------------
// Reconcile input snapshot (the canonical shape the client SENDS).
// --------------------------------------------------------------------------

// WorkflowSnapshot is the snake_case graph snapshot the reconcile task input
// carries (and the plan task's existing_workflow). Built explicitly from the
// local Graph — never a JSON round-trip of the camelCase local shape — because
// the backend's cycle detection and terminal-status guard read exactly these
// keys (node id/status/depends_on, edge source/target); drifted keys are
// silently NOT read and those safety checks go dark.
type WorkflowSnapshot struct {
	ID        string                     `json:"id"`
	Revision  int64                      `json:"revision"`
	Goal      string                     `json:"goal"`
	Status    string                     `json:"status"`
	Nodes     []WorkflowSnapshotNode     `json:"nodes"`
	Edges     []WorkflowEdgeOut          `json:"edges"`
	Resources []WorkflowSnapshotResource `json:"resources"`
	Blockers  []WorkflowSnapshotBlocker  `json:"blockers"`
}

// WorkflowSnapshotNode is one node in the reconcile snapshot. DependsOn must
// carry EVERY pre-existing dependency (the local layer unions its depends_on
// lists with its explicit edges) so backend cycle detection sees the full
// picture.
type WorkflowSnapshotNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Kind      string   `json:"kind"`
	Status    string   `json:"status"`
	DependsOn []string `json:"depends_on"`
	ToolName  string   `json:"tool_name,omitempty"`
}

// WorkflowSnapshotResource is one tracked resource in the reconcile snapshot.
// Its id is the LOCAL resource id — the backend's resource_updates patches
// reference it back as resource_id.
type WorkflowSnapshotResource struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	NodeID string `json:"node_id"`
	Label  string `json:"label"`
}

// WorkflowSnapshotBlocker is one OPEN blocker in the reconcile snapshot. Its
// id is what the backend's resolve_blockers references back.
type WorkflowSnapshotBlocker struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	NodeID  string `json:"node_id"`
	Kind    string `json:"kind"`
}

// WorkflowReconcileInput hands the backend the CURRENT graph + recent evidence
// and asks for a safe patch.
type WorkflowReconcileInput struct {
	Workflow          *WorkflowSnapshot  `json:"workflow"`
	RecentEvents      []map[string]any   `json:"recent_events,omitempty"`
	RuntimeSummary    map[string]any     `json:"runtime_summary,omitempty"`
	QueueEvents       []map[string]any   `json:"queue_events,omitempty"`
	LatestUserMessage string             `json:"latest_user_message,omitempty"`
	Reason            string             `json:"reason"`
	ToolInventory     []WorkflowToolInfo `json:"tool_inventory,omitempty"`
}

// --------------------------------------------------------------------------
// Reconcile output.
// --------------------------------------------------------------------------

// NodePatchOut is one node mutation in a backend patch (identity: node_id).
// The mutable fields are POINTERS because the backend model declares them
// `... | None` (contracts/tasks.py NodePatchOut): JSON null / omitted means
// "leave unchanged" and must stay distinguishable from an explicit "" — a
// returned last_error:"" CLEARS a previous error, which a value string would
// silently swallow.
type NodePatchOut struct {
	NodeID    string  `json:"node_id"`
	Status    *string `json:"status,omitempty"`
	Title     *string `json:"title,omitempty"`
	LastError *string `json:"last_error,omitempty"`
	Note      *string `json:"note,omitempty"`
}

// ResourcePatchOut is one resource mutation in a backend patch (identity:
// resource_id — the id the client supplied in the reconcile snapshot). Same
// pointer-nullability contract as NodePatchOut (backend ResourcePatchOut).
type ResourcePatchOut struct {
	ResourceID string  `json:"resource_id"`
	Status     *string `json:"status,omitempty"`
	NodeID     *string `json:"node_id,omitempty"`
	Label      *string `json:"label,omitempty"`
}

// WorkflowBlockerOut is one blocker addition in a backend patch. Summary is
// the required field; ID may be null (the client mints one on apply).
type WorkflowBlockerOut struct {
	ID      string `json:"id,omitempty"`
	Summary string `json:"summary"`
	NodeID  string `json:"node_id,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// WorkflowPatchOut is the backend's proposed graph mutation set. UNTRUSTED:
// the client converts it to a local Patch, applies it to a copy, and validates
// the result before anything commits. Mirrors the backend WorkflowPatchOut
// exactly — the backend patch model has NO add_resources/add_evidence ops
// (resources/evidence are client-observed facts, not model suggestions).
type WorkflowPatchOut struct {
	BaseRevision    *int64               `json:"base_revision,omitempty"`
	NewStatus       string               `json:"new_status,omitempty"`
	NodeUpdates     []NodePatchOut       `json:"node_updates,omitempty"`
	AddNodes        []WorkflowNodeOut    `json:"add_nodes,omitempty"`
	AddEdges        []WorkflowEdgeOut    `json:"add_edges,omitempty"`
	ResourceUpdates []ResourcePatchOut   `json:"resource_updates,omitempty"`
	AddBlockers     []WorkflowBlockerOut `json:"add_blockers,omitempty"`
	ResolveBlockers []string             `json:"resolve_blockers,omitempty"`
	Rationale       string               `json:"rationale,omitempty"`
}

// WorkflowReconcileOutput is the typed reconcile result.
type WorkflowReconcileOutput struct {
	Patch        WorkflowPatchOut           `json:"patch"`
	NextAction   *RecommendedActionOut      `json:"next_action,omitempty"`
	UserQuestion *MultipleChoiceQuestionOut `json:"user_question,omitempty"`
	Summary      string                     `json:"summary,omitempty"`
	Warnings     []string                   `json:"warnings,omitempty"`
	Confidence   float64                    `json:"confidence,omitempty"`
}

// --------------------------------------------------------------------------
// Resume digest.
// --------------------------------------------------------------------------

// WorkflowResumeDigestInput asks the backend to rank the active workflows and
// produce a compact resume package (the "where were we?" surface).
type WorkflowResumeDigestInput struct {
	Workflows      []map[string]any `json:"workflows,omitempty"`
	RuntimeSummary map[string]any   `json:"runtime_summary,omitempty"`
	QueueDigest    []map[string]any `json:"queue_digest,omitempty"`
	UserMessage    string           `json:"user_message,omitempty"`
}

// WorkflowResumeItem is one ranked resume entry.
type WorkflowResumeItem struct {
	WorkflowID        string                `json:"workflow_id"`
	Rank              int                   `json:"rank"`
	Headline          string                `json:"headline"`
	Status            string                `json:"status,omitempty"`
	Summary           string                `json:"summary,omitempty"`
	Blockers          []string              `json:"blockers,omitempty"`
	RecommendedAction *RecommendedActionOut `json:"recommended_action,omitempty"`
}

// WorkflowResumeDigestOutput is the typed resume package.
type WorkflowResumeDigestOutput struct {
	Ranked                   []WorkflowResumeItem `json:"ranked"`
	SuggestedFocusWorkflowID string               `json:"suggested_focus_workflow_id,omitempty"`
	AssistantContextLines    []string             `json:"assistant_context_lines,omitempty"`
}

// --------------------------------------------------------------------------
// Semantic decode validation. A zero-value identity (the shape a drifted key
// decodes to) must surface as a typed protocol error — never as silent data
// the graph layer then chases.
// --------------------------------------------------------------------------

// validatePlanOutput rejects plans with empty/duplicate node ids or edges
// referencing unknown/empty endpoints.
func validatePlanOutput(out *WorkflowPlanOutput) error {
	ids := make(map[string]bool, len(out.Nodes))
	for i := range out.Nodes {
		id := strings.TrimSpace(out.Nodes[i].ID)
		if id == "" {
			return fmt.Errorf("plan node %d has an empty id", i)
		}
		if ids[id] {
			return fmt.Errorf("plan contains duplicate node id %q", id)
		}
		ids[id] = true
	}
	for _, e := range out.Edges {
		if strings.TrimSpace(e.Source) == "" || strings.TrimSpace(e.Target) == "" {
			return fmt.Errorf("plan edge %q -> %q has an empty endpoint", e.Source, e.Target)
		}
		if !ids[e.Source] || !ids[e.Target] {
			return fmt.Errorf("plan edge %q -> %q references an unknown node", e.Source, e.Target)
		}
	}
	return nil
}

// validateReconcileOutput rejects patches whose mutations carry empty
// identities (node_updates.node_id, resource_updates.resource_id, add_nodes
// ids, add_edges endpoints). Unknown-node references cannot be checked here —
// only the local graph knows the existing ids; ApplyPatch covers that.
func validateReconcileOutput(out *WorkflowReconcileOutput) error {
	p := &out.Patch
	for i := range p.NodeUpdates {
		if strings.TrimSpace(p.NodeUpdates[i].NodeID) == "" {
			return fmt.Errorf("patch node_updates[%d] has an empty node_id", i)
		}
	}
	for i := range p.ResourceUpdates {
		if strings.TrimSpace(p.ResourceUpdates[i].ResourceID) == "" {
			return fmt.Errorf("patch resource_updates[%d] has an empty resource_id", i)
		}
	}
	seen := make(map[string]bool, len(p.AddNodes))
	for i := range p.AddNodes {
		id := strings.TrimSpace(p.AddNodes[i].ID)
		if id == "" {
			return fmt.Errorf("patch add_nodes[%d] has an empty id", i)
		}
		if seen[id] {
			return fmt.Errorf("patch add_nodes contains duplicate node id %q", id)
		}
		seen[id] = true
	}
	for _, e := range p.AddEdges {
		if strings.TrimSpace(e.Source) == "" || strings.TrimSpace(e.Target) == "" {
			return fmt.Errorf("patch add_edges %q -> %q has an empty endpoint", e.Source, e.Target)
		}
	}
	return nil
}

// validateResumeOutput rejects resume items with an empty workflow_id (an
// unaddressable resume entry is a decode of nothing).
func validateResumeOutput(out *WorkflowResumeDigestOutput) error {
	for i := range out.Ranked {
		if strings.TrimSpace(out.Ranked[i].WorkflowID) == "" {
			return fmt.Errorf("resume ranked[%d] has an empty workflow_id", i)
		}
	}
	return nil
}

// RunWorkflowPlan runs workflow_plan.
func RunWorkflowPlan(ctx context.Context, r TaskRunner, in WorkflowPlanInput) (WorkflowPlanOutput, error) {
	in.Goal = clampHeadRunes(in.Goal, maxWorkflowGoalRunes)
	in.Scope = clampHeadRunes(in.Scope, maxWorkflowScopeRunes)
	var out WorkflowPlanOutput
	err := runTypedValidated(ctx, r, TaskWorkflowPlan, in, nil, &out, func() error {
		return validatePlanOutput(&out)
	})
	return out, err
}

// RunWorkflowReconcile runs workflow_reconcile.
func RunWorkflowReconcile(ctx context.Context, r TaskRunner, in WorkflowReconcileInput) (WorkflowReconcileOutput, error) {
	in.LatestUserMessage = clampHeadRunes(in.LatestUserMessage, maxWorkflowMessageRunes)
	var out WorkflowReconcileOutput
	err := runTypedValidated(ctx, r, TaskWorkflowReconcile, in, nil, &out, func() error {
		return validateReconcileOutput(&out)
	})
	return out, err
}

// RunWorkflowResumeDigest runs workflow_resume_digest.
func RunWorkflowResumeDigest(ctx context.Context, r TaskRunner, in WorkflowResumeDigestInput) (WorkflowResumeDigestOutput, error) {
	in.UserMessage = clampHeadRunes(in.UserMessage, maxWorkflowMessageRunes)
	var out WorkflowResumeDigestOutput
	err := runTypedValidated(ctx, r, TaskWorkflowResumeDigest, in, nil, &out, func() error {
		return validateResumeOutput(&out)
	})
	return out, err
}

// AnyToMap converts an arbitrary JSON-serializable value (e.g. a digest) into
// the map[string]any a task input carries. nil on failure — task inputs are
// best-effort context, never worth failing the call over.
func AnyToMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
