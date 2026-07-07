// Package workflowgraph is the client-owned workflow-intelligence layer: a
// typed, durable, patchable execution graph (DAG) per user goal. The assistant
// project is the single source of truth for plans, progress, resource links,
// blockers, and next actions; the backend contributes STATELESS intelligence
// tasks (workflow_plan / workflow_reconcile / workflow_resume_digest)
// whose outputs are validated locally before anything is committed.
//
// Hard boundaries (mirrors the safety invariants in CLAUDE.md):
//   - The graph never grants execution authority. NextAction is a
//     recommendation; every tool call still flows through tools.Dispatch with
//     the normal tier/confirmation/grant gates.
//   - Backend outputs are UNTRUSTED: every plan and patch is validated against
//     the invariants in validate.go before storage, and applied with optimistic
//     revision checks so a stale patch can never clobber newer local state.
//   - Graphs are stored ONLY in the local SQLite store (as JSON snapshots plus
//     event/resource-link index rows); the backend never persists graph state.
package workflowgraph

import (
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// SnapshotSchemaVersion is the serialized-graph shape version stamped on every
// snapshot. A reader that meets a NEWER version than it understands must treat
// the snapshot as unreadable rather than guess.
const SnapshotSchemaVersion = 1

// Status is a workflow graph's lifecycle state. done/cancelled/failed are
// terminal (failed may be revived to active — an explicit retry).
type Status string

const (
	StatusPlanned   Status = "planned"
	StatusActive    Status = "active"
	StatusBlocked   Status = "blocked"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
	StatusFailed    Status = "failed"
)

// ValidStatuses is the closed set of graph statuses.
var ValidStatuses = map[Status]bool{
	StatusPlanned: true, StatusActive: true, StatusBlocked: true,
	StatusDone: true, StatusCancelled: true, StatusFailed: true,
}

// IsTerminal reports whether a graph status is final.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusCancelled || s == StatusFailed
}

// NodeKind classifies what a node does. The set is closed so a backend plan
// inventing a kind is rejected at validation, not silently stored.
type NodeKind string

const (
	KindOrient     NodeKind = "orient"
	KindDelegate   NodeKind = "delegate"
	KindWait       NodeKind = "wait"
	KindVerify     NodeKind = "verify"
	KindSynthesize NodeKind = "synthesize"
	KindAskUser    NodeKind = "ask_user"
	KindApproval   NodeKind = "approval"
	KindCleanup    NodeKind = "cleanup"
	KindReport     NodeKind = "report"
)

// ValidNodeKinds is the closed set of node kinds.
var ValidNodeKinds = map[NodeKind]bool{
	KindOrient: true, KindDelegate: true, KindWait: true, KindVerify: true,
	KindSynthesize: true, KindAskUser: true, KindApproval: true,
	KindCleanup: true, KindReport: true,
}

// NodeStatus is one node's execution state. done/skipped/cancelled are hard
// terminal; failed may be retried (failed → ready) or parked (failed → blocked).
type NodeStatus string

const (
	NodePending   NodeStatus = "pending"
	NodeReady     NodeStatus = "ready"
	NodeRunning   NodeStatus = "running"
	NodeWaiting   NodeStatus = "waiting"
	NodeBlocked   NodeStatus = "blocked"
	NodeDone      NodeStatus = "done"
	NodeSkipped   NodeStatus = "skipped"
	NodeFailed    NodeStatus = "failed"
	NodeCancelled NodeStatus = "cancelled"
)

// ValidNodeStatuses is the closed set of node statuses.
var ValidNodeStatuses = map[NodeStatus]bool{
	NodePending: true, NodeReady: true, NodeRunning: true, NodeWaiting: true,
	NodeBlocked: true, NodeDone: true, NodeSkipped: true, NodeFailed: true,
	NodeCancelled: true,
}

// IsTerminal reports whether a node status is final (failed counts: it stays
// terminal until an explicit retry transition revives it).
func (s NodeStatus) IsTerminal() bool {
	return s == NodeDone || s == NodeSkipped || s == NodeFailed || s == NodeCancelled
}

// IsActive reports whether a node status belongs in ActiveNodeIDs.
func (s NodeStatus) IsActive() bool {
	return s == NodeReady || s == NodeRunning || s == NodeWaiting || s == NodeBlocked
}

// satisfiesDependency reports whether a dependency in this status unblocks its
// dependents (only genuinely-completed work counts — a failed or cancelled dep
// leaves the dependent pending until a reconcile decides what to do).
func (s NodeStatus) satisfiesDependency() bool {
	return s == NodeDone || s == NodeSkipped
}

// AsyncPolicy is a node's long-running-work posture (advisory; execution still
// belongs to the tools the model actually calls).
type AsyncPolicy string

const (
	AsyncBlocking AsyncPolicy = "blocking"
	AsyncOK       AsyncPolicy = "async_ok"
	AsyncRequired AsyncPolicy = "async_required"
	AsyncWakeOnly AsyncPolicy = "wake_only"
)

// ValidAsyncPolicies is the closed set (empty string = unspecified, allowed).
var ValidAsyncPolicies = map[AsyncPolicy]bool{
	AsyncBlocking: true, AsyncOK: true, AsyncRequired: true, AsyncWakeOnly: true,
}

// Source records what created a graph.
type Source string

const (
	SourceUser     Source = "user"
	SourceWake     Source = "wake"
	SourceResume   Source = "resume"
	SourceImported Source = "imported"
)

// Structural caps. The graph must stay compact, typed, and patchable — never
// another verbose scratchpad. Backend plans are capped tighter server-side
// (≤20 nodes / ≤40 edges per plan); these are the LOCAL ceilings a graph may
// grow to across its whole life via patches.
const (
	MaxNodes     = 64
	MaxEdges     = 128
	MaxResources = 100
	MaxBlockers  = 50
	MaxDecisions = 50
	// MaxEvidence bounds the in-snapshot evidence ring; older entries are pruned
	// (the workflow_events table keeps the full history).
	MaxEvidence = 200

	// Field clamps (runes). Applied at conversion/ingest so an oversized
	// backend- or model-authored string can never bloat the snapshot.
	MaxTitleRunes    = 120
	MaxGoalRunes     = 1024
	MaxSummaryRunes  = 500
	MaxReasonRunes   = 500
	MaxCriteriaRunes = 300
	MaxCriteria      = 12
)

// Graph is one durable workflow execution graph (the WorkflowGraph of the
// spec). Serialized verbatim as the snapshot JSON; keep field changes
// backward-readable or bump SnapshotSchemaVersion.
type Graph struct {
	ID            string `json:"id"` // wfg_<uuid8>
	Goal          string `json:"goal"`
	Status        Status `json:"status"`
	Source        Source `json:"source,omitempty"`
	SchemaVersion int    `json:"schemaVersion"`

	SuccessCriteria []string `json:"successCriteria,omitempty"`
	Assumptions     []string `json:"assumptions,omitempty"`
	// CriteriaWaived, when non-empty, is the explicit reason the success
	// criteria were waived — the only way a graph may be marked done while
	// nodes remain non-terminal.
	CriteriaWaived string `json:"criteriaWaived,omitempty"`

	Nodes     []Node        `json:"nodes"`
	Edges     []Edge        `json:"edges,omitempty"`
	Resources []Resource    `json:"resources,omitempty"`
	Blockers  []Blocker     `json:"blockers,omitempty"`
	Decisions []Decision    `json:"decisions,omitempty"`
	Evidence  []EvidenceRef `json:"evidence,omitempty"`

	// NextAction is a RECOMMENDATION only — dispatch remains the sole execution
	// authority (its RequiresConfirmation flag is informational).
	NextAction    *domain.RecommendedAction `json:"nextAction,omitempty"`
	ActiveNodeIDs []string                  `json:"activeNodeIds,omitempty"`

	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	CompletedAt *int64 `json:"completedAt,omitempty"`
}

// Node is one unit of the plan.
type Node struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Kind   NodeKind   `json:"kind"`
	Status NodeStatus `json:"status"`
	// DependsOn lists node ids that must be done/skipped before this node
	// becomes ready (unioned with Edges when computing readiness/acyclicity).
	DependsOn []string `json:"dependsOn,omitempty"`
	// ToolName/ToolArgs/Risk/RequiresConfirm describe the SUGGESTED call; they
	// carry no authority (dispatch decides).
	ToolName         string         `json:"toolName,omitempty"`
	ToolArgs         map[string]any `json:"toolArgs,omitempty"`
	Risk             string         `json:"risk,omitempty"`
	RequiresConfirm  bool           `json:"requiresConfirm,omitempty"`
	ExpectedEvidence []string       `json:"expectedEvidence,omitempty"`
	AsyncPolicy      AsyncPolicy    `json:"asyncPolicy,omitempty"`
	// Owner is who performs the node: assistant | agent:<id> | user | external.
	Owner       string   `json:"owner,omitempty"`
	ResourceIDs []string `json:"resourceIds,omitempty"`
	Attempts    int      `json:"attempts,omitempty"`
	LastError   string   `json:"lastError,omitempty"`
	CreatedAt   int64    `json:"createdAt,omitempty"`
	UpdatedAt   int64    `json:"updatedAt,omitempty"`
	StartedAt   *int64   `json:"startedAt,omitempty"`
	CompletedAt *int64   `json:"completedAt,omitempty"`
}

// Edge is an explicit dependency: To cannot start before From completes.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Resource is one external object the graph tracks (terminal, watcher, async
// handle, worktree, branch, PR, …). Ref is the external identifier.
type Resource struct {
	ID        string         `json:"id"` // wrs_<uuid8>
	Type      string         `json:"type"`
	Ref       string         `json:"ref"`
	Label     string         `json:"label,omitempty"`
	NodeID    string         `json:"nodeId,omitempty"`
	Status    string         `json:"status,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt int64          `json:"createdAt,omitempty"`
	UpdatedAt int64          `json:"updatedAt,omitempty"`
}

// Blocker is one open obstacle. ResolvedAt nil = still blocking.
type Blocker struct {
	ID         string `json:"id"`
	NodeID     string `json:"nodeId,omitempty"`
	Reason     string `json:"reason"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	ResolvedAt *int64 `json:"resolvedAt,omitempty"`
}

// Decision records one committed choice and why.
type Decision struct {
	ID        string `json:"id"`
	NodeID    string `json:"nodeId,omitempty"`
	Summary   string `json:"summary"`
	Rationale string `json:"rationale,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// EvidenceRef is one bounded evidence item tied to the graph (NodeID "" =
// graph-level). RefID points at the source object (toolCallId, queue event id,
// async id, …); Digest optionally hashes an archived raw payload.
type EvidenceRef struct {
	ID        string `json:"id"`
	NodeID    string `json:"nodeId,omitempty"`
	Kind      string `json:"kind"` // tool_call|tool_result|queue_event|watcher_event|async_completion|user_message|assistant_message|manual_note
	Summary   string `json:"summary"`
	RefID     string `json:"refId,omitempty"`
	Digest    string `json:"digest,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// ValidEvidenceKinds is the closed evidence-kind set.
var ValidEvidenceKinds = map[string]bool{
	"tool_call": true, "tool_result": true, "queue_event": true,
	"watcher_event": true, "async_completion": true, "user_message": true,
	"assistant_message": true, "manual_note": true,
}

// ValidResourceTypes is the closed resource-type set.
var ValidResourceTypes = map[string]bool{
	"terminal": true, "agent": true, "worktree": true, "branch": true,
	"pr": true, "issue": true, "watcher": true, "timer": true, "async": true,
	"queue_event": true, "artifact": true, "memory": true, "grant": true,
}

// NodeByID returns a pointer to the node with the given id, or nil.
func (g *Graph) NodeByID(id string) *Node {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// ResourceByID returns a pointer to the resource with the given id, or nil.
func (g *Graph) ResourceByID(id string) *Resource {
	for i := range g.Resources {
		if g.Resources[i].ID == id {
			return &g.Resources[i]
		}
	}
	return nil
}

// FindResource returns the resource matching (type, ref), or nil.
func (g *Graph) FindResource(resourceType, ref string) *Resource {
	for i := range g.Resources {
		if g.Resources[i].Type == resourceType && g.Resources[i].Ref == ref {
			return &g.Resources[i]
		}
	}
	return nil
}

// OpenBlockers returns the unresolved blockers.
func (g *Graph) OpenBlockers() []Blocker {
	var out []Blocker
	for _, b := range g.Blockers {
		if b.ResolvedAt == nil {
			out = append(out, b)
		}
	}
	return out
}

// DoneNodeCount returns how many nodes are done or skipped (progress numerator).
func (g *Graph) DoneNodeCount() int {
	n := 0
	for _, node := range g.Nodes {
		if node.Status == NodeDone || node.Status == NodeSkipped {
			n++
		}
	}
	return n
}

// CurrentNode returns the most-advanced in-flight node for display purposes:
// the first running node, else the first waiting node, else the first ready
// node, else nil. Deterministic (node order breaks ties).
func (g *Graph) CurrentNode() *Node {
	for _, want := range []NodeStatus{NodeRunning, NodeWaiting, NodeReady} {
		for i := range g.Nodes {
			if g.Nodes[i].Status == want {
				return &g.Nodes[i]
			}
		}
	}
	return nil
}
