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
		g.Edges = append(g.Edges, Edge{From: e.From, To: e.To})
	}

	if na, warn := actionFromWire(out.NextAction, hasTool); na != nil {
		g.NextAction = na
	} else if warn != "" {
		warnings = append(warnings, warn)
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
	if wp.NewStatus != "" {
		s := Status(wp.NewStatus)
		p.NewStatus = &s
	}
	for _, nu := range wp.NodeUpdates {
		np := NodePatch{ID: nu.ID, BumpAttempts: nu.BumpAttempts}
		if nu.Status != "" {
			s := NodeStatus(nu.Status)
			np.Status = &s
		}
		if nu.Title != "" {
			t := nu.Title
			np.Title = &t
		}
		if nu.Owner != "" {
			o := nu.Owner
			np.Owner = &o
		}
		if nu.LastError != "" {
			e := nu.LastError
			np.LastError = &e
		}
		p.NodeUpdates = append(p.NodeUpdates, np)
	}
	for _, n := range wp.AddNodes {
		p.AddNodes = append(p.AddNodes, nodeFromWire(n, now))
	}
	for _, e := range wp.AddEdges {
		p.AddEdges = append(p.AddEdges, Edge{From: e.From, To: e.To})
	}
	for _, ru := range wp.ResourceUpdates {
		rp := ResourcePatch{ID: ru.ID}
		if ru.Status != "" {
			s := ru.Status
			rp.Status = &s
		}
		if ru.NodeID != "" {
			nid := ru.NodeID
			rp.NodeID = &nid
		}
		if ru.Label != "" {
			l := ru.Label
			rp.Label = &l
		}
		p.ResourceUpdates = append(p.ResourceUpdates, rp)
	}
	for _, r := range wp.AddResources {
		p.AddResources = append(p.AddResources, Resource{
			Type: r.Type, Ref: r.Ref, Label: r.Label, NodeID: r.NodeID, Status: r.Status,
		})
	}
	for _, b := range wp.AddBlockers {
		p.AddBlockers = append(p.AddBlockers, Blocker{NodeID: b.NodeID, Reason: b.Reason})
	}
	p.ResolveBlockers = append(p.ResolveBlockers, wp.ResolveBlockers...)
	for _, ev := range wp.AddEvidence {
		p.AddEvidence = append(p.AddEvidence, EvidenceRef{
			NodeID: ev.NodeID, Kind: ev.Kind, Summary: ev.Summary, RefID: ev.RefID,
		})
	}

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
	}, ""
}
