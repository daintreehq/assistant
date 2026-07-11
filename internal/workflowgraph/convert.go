package workflowgraph

import (
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// This file converts the backend's snake_case task outputs into local typed
// graph structures. Conversion CLAMPS free text and normalizes shapes; it does
// not validate structure — every converted graph/patch still flows through
// Validate/ApplyPatch before storage (backend output is untrusted).

// GraphFromPlan builds a fresh graph from a workflow_plan output. Returned
// warnings surface non-fatal repairs (e.g. a next action dropped because its
// tool doesn't exist locally); the error covers unrecoverable plans only —
// structural problems are left for Validate to reject with precision.
func GraphFromPlan(out backend.WorkflowPlanOutput, goal string, source Source, hasTool func(string) bool, now int64) (*Graph, []string, error) {
	var warnings []string
	warnings = append(warnings, out.Warnings...)

	if len(out.Nodes) == 0 {
		return nil, warnings, fmt.Errorf("plan contains no nodes")
	}

	status := StatusActive
	switch Status(out.Status) {
	case StatusPlanned, StatusActive, StatusBlocked:
		status = Status(out.Status)
	case "":
		// default active
	default:
		warnings = append(warnings, fmt.Sprintf("plan status %q normalized to active", out.Status))
	}

	planGoal := strings.TrimSpace(out.Goal)
	if planGoal == "" {
		planGoal = goal
	}

	g := &Graph{
		ID:            domain.NewID(domain.PrefixWorkflowGraph),
		Goal:          clampRunes(planGoal, MaxGoalRunes),
		Status:        status,
		Source:        source,
		SchemaVersion: SnapshotSchemaVersion,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, c := range out.SuccessCriteria {
		if c = strings.TrimSpace(c); c != "" && len(g.SuccessCriteria) < MaxCriteria {
			g.SuccessCriteria = append(g.SuccessCriteria, clampRunes(c, MaxCriteriaRunes))
		}
	}
	for _, a := range out.Assumptions {
		if a = strings.TrimSpace(a); a != "" && len(g.Assumptions) < MaxCriteria {
			g.Assumptions = append(g.Assumptions, clampRunes(a, MaxCriteriaRunes))
		}
	}

	for _, n := range out.Nodes {
		g.Nodes = append(g.Nodes, nodeFromWire(n, now))
	}
	for _, e := range out.Edges {
		g.Edges = append(g.Edges, Edge{From: e.Source, To: e.Target})
	}

	if na, warn := actionFromWire(out.NextAction, hasTool); na != nil {
		g.NextAction = na
	} else if warn != "" {
		warnings = append(warnings, warn)
	}
	// A node binding that names no node in THIS plan is a hallucinated hint:
	// drop the binding (the action itself is still useful), warn.
	if g.NextAction != nil && g.NextAction.NodeID != "" && g.NodeByID(g.NextAction.NodeID) == nil {
		warnings = append(warnings, fmt.Sprintf(
			"next action node binding dropped: plan has no node %q", g.NextAction.NodeID))
		g.NextAction.NodeID = ""
	}

	RecomputeDerived(g, now)
	return g, warnings, nil
}

// PatchFromWire converts a workflow_reconcile patch (plus its top-level
// next action) into the local Patch shape, clamped. Warnings report non-fatal
// repairs; structural validity is ApplyPatch's job.
func PatchFromWire(out backend.WorkflowReconcileOutput, workflowID string, baseRevision int64, hasTool func(string) bool, now int64) (*Patch, []string) {
	var warnings []string
	warnings = append(warnings, out.Warnings...)
	wp := out.Patch

	p := &Patch{
		WorkflowID:   workflowID,
		BaseRevision: baseRevision,
		Rationale:    clampRunes(wp.Rationale, MaxReasonRunes),
	}
	// NOTE: a non-null wp.BaseRevision that disagrees with the local revision is
	// REJECTED by Service.Reconcile BEFORE conversion — a patch reasoned against
	// a stale/replayed snapshot must never be applied to a newer revision. This
	// converter only ever sees a null or matching echo.
	if wp.NewStatus != "" {
		s := Status(wp.NewStatus)
		p.NewStatus = &s
	}
	// Nullable wire fields (pointers) carry three-way semantics: nil = leave
	// unchanged, non-nil = apply — INCLUDING an explicit "" (e.g. last_error:""
	// clears a previous error). Structural validity remains ApplyPatch's job.
	for _, nu := range wp.NodeUpdates {
		np := NodePatch{ID: nu.NodeID}
		if nu.Status != nil {
			s := NodeStatus(*nu.Status)
			np.Status = &s
		}
		np.Title = nu.Title
		np.LastError = nu.LastError
		// nu.Note is advisory prose for the operator; the local graph keeps no
		// per-node note field, so it is intentionally not persisted.
		p.NodeUpdates = append(p.NodeUpdates, np)
	}
	for _, n := range wp.AddNodes {
		p.AddNodes = append(p.AddNodes, nodeFromWire(n, now))
	}
	for _, e := range wp.AddEdges {
		p.AddEdges = append(p.AddEdges, Edge{From: e.Source, To: e.Target})
	}
	for _, ru := range wp.ResourceUpdates {
		p.ResourceUpdates = append(p.ResourceUpdates, ResourcePatch{
			ID: ru.ResourceID, Status: ru.Status, NodeID: ru.NodeID, Label: ru.Label,
		})
	}
	for _, b := range wp.AddBlockers {
		p.AddBlockers = append(p.AddBlockers, Blocker{
			ID: strings.TrimSpace(b.ID), NodeID: b.NodeID,
			Reason: b.Summary, Kind: b.Kind,
		})
	}
	p.ResolveBlockers = append(p.ResolveBlockers, wp.ResolveBlockers...)

	if na, warn := actionFromWire(out.NextAction, hasTool); na != nil {
		p.NextAction = na
	} else if warn != "" {
		warnings = append(warnings, warn)
	}
	return p, warnings
}

// nodeFromWire maps one wire node to the local shape, clamped. Status defaults
// to pending; readiness is recomputed after application, so a plan that omits
// statuses starts every root node ready automatically.
func nodeFromWire(n backend.WorkflowNodeOut, now int64) Node {
	status := NodeStatus(n.Status)
	if n.Status == "" {
		status = NodePending
	}
	id := strings.TrimSpace(n.ID)
	if id == "" {
		id = domain.NewID(domain.PrefixWorkflowNode)
	}
	expected := n.ExpectedEvidence
	if len(expected) > 10 {
		expected = expected[:10]
	}
	for i, e := range expected {
		expected[i] = clampRunes(e, 200)
	}
	return Node{
		ID:               id,
		Title:            clampRunes(strings.TrimSpace(n.Title), MaxTitleRunes),
		Kind:             NodeKind(n.Kind),
		Status:           status,
		DependsOn:        n.DependsOn,
		ToolName:         n.ToolName,
		ToolArgs:         n.ToolArgs,
		Risk:             n.Risk,
		RequiresConfirm:  n.RequiresConfirm,
		ExpectedEvidence: expected,
		AsyncPolicy:      AsyncPolicy(n.AsyncPolicy),
		Owner:            n.Owner,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// actionFromWire converts a wire recommended action, DROPPING it (with a
// warning) when it names a tool that doesn't exist locally — a recommendation
// the model can't execute is worse than none. The RequiresConfirmation flag is
// informational only; dispatch remains authoritative.
func actionFromWire(a *backend.RecommendedActionOut, hasTool func(string) bool) (*domain.RecommendedAction, string) {
	if a == nil {
		return nil, ""
	}
	tool := strings.TrimSpace(a.ToolName)
	if tool == "" {
		return nil, "next action dropped: no tool name"
	}
	if hasTool != nil && !hasTool(tool) {
		return nil, fmt.Sprintf("next action dropped: unknown tool %q", tool)
	}
	return &domain.RecommendedAction{
		Label:                clampRunes(a.Label, MaxTitleRunes),
		ToolName:             tool,
		Args:                 a.Args,
		Risk:                 domain.RiskClass(a.Risk),
		RequiresConfirmation: a.RequiresConfirmation,
		// The node binding disambiguates WHICH ready node this action advances
		// when several share a tool. Validated against the graph where the
		// action lands (GraphFromPlan above / ApplyPatch): an unknown node
		// drops the binding, never the action.
		NodeID: strings.TrimSpace(a.NodeID),
	}, ""
}

// SnapshotFromGraph projects the local graph into the canonical snake_case
// wire snapshot the reconcile task (and plan's existing_workflow) carries —
// built field-by-field, never via a JSON round-trip of the camelCase local
// shape, because the backend's cycle detection and terminal-status guard read
// exactly these keys. Each node's depends_on is unioned with its incoming
// explicit edges so the backend sees EVERY pre-existing dependency; only open
// blockers travel (resolved ones are history, not state).
func SnapshotFromGraph(g *Graph, revision int64) *backend.WorkflowSnapshot {
	snap := &backend.WorkflowSnapshot{
		ID:        g.ID,
		Revision:  revision,
		Goal:      g.Goal,
		Status:    string(g.Status),
		Nodes:     make([]backend.WorkflowSnapshotNode, 0, len(g.Nodes)),
		Edges:     make([]backend.WorkflowEdgeOut, 0, len(g.Edges)),
		Resources: make([]backend.WorkflowSnapshotResource, 0, len(g.Resources)),
		Blockers:  []backend.WorkflowSnapshotBlocker{},
	}
	incoming := make(map[string][]string, len(g.Nodes))
	for _, e := range g.Edges {
		incoming[e.To] = append(incoming[e.To], e.From)
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		deps := make([]string, 0, len(n.DependsOn)+len(incoming[n.ID]))
		seen := make(map[string]bool, cap(deps))
		for _, d := range n.DependsOn {
			if d != "" && !seen[d] {
				seen[d] = true
				deps = append(deps, d)
			}
		}
		for _, d := range incoming[n.ID] {
			if d != "" && !seen[d] {
				seen[d] = true
				deps = append(deps, d)
			}
		}
		snap.Nodes = append(snap.Nodes, backend.WorkflowSnapshotNode{
			ID:        n.ID,
			Title:     n.Title,
			Kind:      string(n.Kind),
			Status:    string(n.Status),
			DependsOn: deps,
			ToolName:  n.ToolName,
		})
	}
	for _, e := range g.Edges {
		snap.Edges = append(snap.Edges, backend.WorkflowEdgeOut{Source: e.From, Target: e.To})
	}
	for i := range g.Resources {
		r := &g.Resources[i]
		snap.Resources = append(snap.Resources, backend.WorkflowSnapshotResource{
			ID:     r.ID,
			Kind:   r.Type,
			Status: r.Status,
			NodeID: r.NodeID,
			Label:  r.Label,
		})
	}
	for _, b := range g.OpenBlockers() {
		kind := b.Kind
		if kind == "" {
			kind = "other"
		}
		snap.Blockers = append(snap.Blockers, backend.WorkflowSnapshotBlocker{
			ID:      b.ID,
			Summary: b.Reason,
			NodeID:  b.NodeID,
			Kind:    kind,
		})
	}
	return snap
}
