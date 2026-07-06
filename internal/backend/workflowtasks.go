package backend

import (
	"context"
	"encoding/json"
)

// Workflow-intelligence task IDs (server-owned prompts/models; the CLI sends
// data only). Availability is gated by DAINTREE_WORKFLOW_INTELLIGENCE and, at
// runtime, by Capabilities.Tasks — a backend without these tasks rejects them
// and the caller degrades gracefully.
const (
	TaskWorkflowPlan         = "workflow_plan.v1"
	TaskWorkflowReconcile    = "workflow_reconcile.v1"
	TaskWorkflowResumeDigest = "workflow_resume_digest.v1"
)

// Input size caps (mirror the backend pydantic max_length constraints; clamped
// client-side so a large goal/message can never 422 the task).
const (
	maxWorkflowGoalRunes    = 8_000
	maxWorkflowScopeRunes   = 4_000
	maxWorkflowMessageRunes = 8_000
)

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
	ActiveSkillIDs   []string           `json:"active_skill_ids,omitempty"`
	ExistingWorkflow map[string]any     `json:"existing_workflow,omitempty"`
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

// WorkflowEdgeOut is one planned dependency edge.
type WorkflowEdgeOut struct {
	From string `json:"from"`
	To   string `json:"to"`
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
}

// MultipleChoiceQuestionOut is a finite user decision the backend recommends
// asking (rendered through user.askMultipleChoice by the model, never directly).
type MultipleChoiceQuestionOut struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
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

// WorkflowReconcileInput hands the backend the CURRENT graph + recent evidence
// and asks for a safe patch. The workflow snapshot travels as a generic map so
// the backend prompt renders it without a coupled Go/pydantic model.
type WorkflowReconcileInput struct {
	Workflow          map[string]any     `json:"workflow"`
	RecentEvents      []map[string]any   `json:"recent_events,omitempty"`
	RuntimeSummary    map[string]any     `json:"runtime_summary,omitempty"`
	QueueEvents       []map[string]any   `json:"queue_events,omitempty"`
	LatestUserMessage string             `json:"latest_user_message,omitempty"`
	Reason            string             `json:"reason"`
	ToolInventory     []WorkflowToolInfo `json:"tool_inventory,omitempty"`
}

// NodePatchOut is one node mutation in a backend patch.
type NodePatchOut struct {
	ID           string `json:"id"`
	Status       string `json:"status,omitempty"`
	Title        string `json:"title,omitempty"`
	Owner        string `json:"owner,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	BumpAttempts bool   `json:"bump_attempts,omitempty"`
}

// ResourcePatchOut is one resource mutation in a backend patch.
type ResourcePatchOut struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	NodeID string `json:"node_id,omitempty"`
	Label  string `json:"label,omitempty"`
}

// ResourceOut is one resource addition in a backend patch.
type ResourceOut struct {
	Type   string `json:"type"`
	Ref    string `json:"ref"`
	Label  string `json:"label,omitempty"`
	NodeID string `json:"node_id,omitempty"`
	Status string `json:"status,omitempty"`
}

// BlockerOut is one blocker addition in a backend patch.
type BlockerOut struct {
	NodeID string `json:"node_id,omitempty"`
	Reason string `json:"reason"`
}

// EvidenceOut is one evidence addition in a backend patch.
type EvidenceOut struct {
	NodeID  string `json:"node_id,omitempty"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	RefID   string `json:"ref_id,omitempty"`
}

// WorkflowPatchOut is the backend's proposed graph mutation set. UNTRUSTED:
// the client converts it to a local Patch, applies it to a copy, and validates
// the result before anything commits.
type WorkflowPatchOut struct {
	NewStatus       string             `json:"new_status,omitempty"`
	NodeUpdates     []NodePatchOut     `json:"node_updates,omitempty"`
	AddNodes        []WorkflowNodeOut  `json:"add_nodes,omitempty"`
	AddEdges        []WorkflowEdgeOut  `json:"add_edges,omitempty"`
	ResourceUpdates []ResourcePatchOut `json:"resource_updates,omitempty"`
	AddResources    []ResourceOut      `json:"add_resources,omitempty"`
	AddBlockers     []BlockerOut       `json:"add_blockers,omitempty"`
	ResolveBlockers []string           `json:"resolve_blockers,omitempty"`
	AddEvidence     []EvidenceOut      `json:"add_evidence,omitempty"`
	Rationale       string             `json:"rationale,omitempty"`
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
	WorkflowID    string `json:"workflow_id"`
	Goal          string `json:"goal,omitempty"`
	Status        string `json:"status,omitempty"`
	Summary       string `json:"summary,omitempty"`
	NextAction    string `json:"next_action,omitempty"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// WorkflowResumeDigestOutput is the typed resume package.
type WorkflowResumeDigestOutput struct {
	Ranked                   []WorkflowResumeItem `json:"ranked"`
	SuggestedFocusWorkflowID string               `json:"suggested_focus_workflow_id,omitempty"`
	AssistantContextLines    []string             `json:"assistant_context_lines,omitempty"`
}

// RunWorkflowPlan runs workflow_plan.v1.
func RunWorkflowPlan(ctx context.Context, r TaskRunner, in WorkflowPlanInput) (WorkflowPlanOutput, error) {
	in.Goal = clampHeadRunes(in.Goal, maxWorkflowGoalRunes)
	in.Scope = clampHeadRunes(in.Scope, maxWorkflowScopeRunes)
	var out WorkflowPlanOutput
	err := runTyped(ctx, r, TaskWorkflowPlan, in, nil, &out)
	return out, err
}

// RunWorkflowReconcile runs workflow_reconcile.v1.
func RunWorkflowReconcile(ctx context.Context, r TaskRunner, in WorkflowReconcileInput) (WorkflowReconcileOutput, error) {
	in.LatestUserMessage = clampHeadRunes(in.LatestUserMessage, maxWorkflowMessageRunes)
	var out WorkflowReconcileOutput
	err := runTyped(ctx, r, TaskWorkflowReconcile, in, nil, &out)
	return out, err
}

// RunWorkflowResumeDigest runs workflow_resume_digest.v1.
func RunWorkflowResumeDigest(ctx context.Context, r TaskRunner, in WorkflowResumeDigestInput) (WorkflowResumeDigestOutput, error) {
	in.UserMessage = clampHeadRunes(in.UserMessage, maxWorkflowMessageRunes)
	var out WorkflowResumeDigestOutput
	err := runTyped(ctx, r, TaskWorkflowResumeDigest, in, nil, &out)
	return out, err
}

// AnyToMap converts an arbitrary JSON-serializable value (e.g. a graph
// snapshot) into the map[string]any a task input carries. nil on failure —
// task inputs are best-effort context, never worth failing the call over.
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
