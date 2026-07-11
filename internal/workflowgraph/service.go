package workflowgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Store is the persistence seam (satisfied structurally by *storage.Store).
type Store interface {
	InsertWorkflowGraph(rec domain.WorkflowGraphRecord) (domain.WorkflowGraphRecord, error)
	GetWorkflowGraph(id string) (*domain.WorkflowGraphRecord, error)
	ListWorkflowGraphs(statuses []string, limit int) ([]domain.WorkflowGraphRecord, error)
	UpdateWorkflowGraphSnapshot(id string, expectedRevision int64, rec domain.WorkflowGraphRecord) (int64, error)
	InsertWorkflowGraphEvent(rec domain.WorkflowGraphEventRecord) (domain.WorkflowGraphEventRecord, error)
	ListWorkflowGraphEvents(workflowID string, limit int) ([]domain.WorkflowGraphEventRecord, error)
	UpsertWorkflowResourceLink(rec domain.WorkflowResourceLinkRecord) error
	ListWorkflowResourceLinks(workflowID string) ([]domain.WorkflowResourceLinkRecord, error)
	FindWorkflowResourceLinks(resourceType, resourceRef string) ([]domain.WorkflowResourceLinkRecord, error)
	InsertWorkflowReconcileRun(rec domain.WorkflowReconcileRunRecord) (domain.WorkflowReconcileRunRecord, error)
	UpdateWorkflowReconcileRun(id string, patch map[string]any) error
}

// OpenStatuses is the non-terminal graph filter shared by every "active
// graphs" read (digests, dashboards, resume).
var OpenStatuses = []string{string(StatusPlanned), string(StatusActive), string(StatusBlocked)}

// Deps wires a Service.
type Deps struct {
	Store Store
	// Tasks is the backend utility-task seam. nil ⇒ offline: Plan/Reconcile/
	// ResumeDigest fail gracefully; every local operation still works.
	Tasks backend.TaskRunner
	// HasTool reports whether an internal dotted tool name is registered — the
	// next-action existence gate. nil ⇒ the check is skipped.
	HasTool func(name string) bool
	// ToolInventory lists the registered tools (name + risk) for the backend's
	// plan/reconcile prompts. nil ⇒ no inventory is sent.
	ToolInventory func() []backend.WorkflowToolInfo
	// RuntimeSummary snapshots the runtime facts (project, MCP, tier, worktree)
	// the planning tasks receive. nil ⇒ empty.
	RuntimeSummary func() map[string]any
	// Now seams the clock; nil ⇒ domain.NowMS.
	Now func() int64
	// Trace is the diagnostic seam (debug log); nil ⇒ no-op.
	Trace func(event string, fields map[string]any)
}

// Service is the runtime owner of the workflow-intelligence layer: it loads,
// mutates (through the single ApplyPatch write path), validates, and commits
// graphs, and adapts the backend's stateless planning tasks. One instance per
// process; mu serializes local read-modify-write cycles (the store's revision
// guard additionally protects against any concurrent writer).
type Service struct {
	deps Deps
	mu   sync.Mutex
}

// NewService builds a Service. Store is required.
func NewService(deps Deps) (*Service, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("workflowgraph: Store is required")
	}
	if deps.Now == nil {
		deps.Now = domain.NowMS
	}
	return &Service{deps: deps}, nil
}

func (s *Service) now() int64 { return s.deps.Now() }

func (s *Service) trace(event string, fields map[string]any) {
	if s.deps.Trace == nil {
		return
	}
	defer func() { _ = recover() }()
	s.deps.Trace(event, fields)
}

func (s *Service) validateOpts() ValidateOptions {
	return ValidateOptions{HasTool: s.deps.HasTool}
}

/* --------------------------------- reads --------------------------------- */

// Get loads one graph and its current revision. (nil, 0, nil) when absent.
func (s *Service) Get(id string) (*Graph, int64, error) {
	rec, err := s.deps.Store.GetWorkflowGraph(id)
	if err != nil {
		return nil, 0, err
	}
	if rec == nil {
		return nil, 0, nil
	}
	g, err := DecodeSnapshot(rec)
	if err != nil {
		return nil, 0, err
	}
	return g, rec.Revision, nil
}

// List returns decoded graphs filtered by status (nil ⇒ all), newest first.
// A snapshot that fails to decode is skipped (surfaced via trace), so one
// corrupt row can never hide the rest.
func (s *Service) List(statuses []string, limit int) ([]*Graph, error) {
	recs, err := s.deps.Store.ListWorkflowGraphs(statuses, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*Graph, 0, len(recs))
	for i := range recs {
		g, derr := DecodeSnapshot(&recs[i])
		if derr != nil {
			s.trace("workflow.graph.decode_failed", map[string]any{"workflowId": recs[i].ID, "error": derr.Error()})
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

// NextResult is the locally-computed "what can run now" answer.
type NextResult struct {
	WorkflowID   string
	Status       Status
	ReadyNodes   []Node
	WaitingNodes []Node
	NextAction   *domain.RecommendedAction
	OpenBlockers []Blocker
}

// Next computes the ready/waiting nodes and stored next action for one graph.
func (s *Service) Next(id string) (NextResult, error) {
	g, _, err := s.Get(id)
	if err != nil {
		return NextResult{}, err
	}
	if g == nil {
		return NextResult{}, fmt.Errorf("no such workflow graph: %s", id)
	}
	res := NextResult{WorkflowID: g.ID, Status: g.Status, ReadyNodes: ReadyNodes(g),
		NextAction: g.NextAction, OpenBlockers: g.OpenBlockers()}
	for i := range g.Nodes {
		if g.Nodes[i].Status == NodeWaiting {
			res.WaitingNodes = append(res.WaitingNodes, g.Nodes[i])
		}
	}
	return res, nil
}

// WorkflowDigests renders the open graphs as bounded, prompt-ready digests
// (newest first, capped + clamped to the wire contract). Best-effort: any
// error yields nil — a digest failure must never break a turn. The method name
// matches the agent seam so *Service satisfies it structurally.
func (s *Service) WorkflowDigests(limit int) []backend.WorkflowDigest {
	defer func() { _ = recover() }()
	if limit <= 0 || limit > backend.MaxWorkflowDigests {
		limit = backend.MaxWorkflowDigests
	}
	graphs, err := s.List(OpenStatuses, limit)
	if err != nil || len(graphs) == 0 {
		return nil
	}
	digests := make([]backend.WorkflowDigest, 0, len(graphs))
	for _, g := range graphs {
		lastEvent := ""
		if evs, eerr := s.deps.Store.ListWorkflowGraphEvents(g.ID, 1); eerr == nil && len(evs) > 0 {
			lastEvent = evs[0].Summary
		}
		digests = append(digests, BuildDigest(g, lastEvent))
	}
	return backend.CapWorkflowDigests(digests)
}

/* ----------------------------- the write path ---------------------------- */

// commitRetries bounds the reload-and-retry loop on a revision conflict. The
// store is single-writer per process and mu serializes local writers, so a
// conflict is rare (another owner process mid-handover); more than a couple of
// retries means something is genuinely fighting us and should surface.
const commitRetries = 3

// mutate is THE local write path: load → build patch → ApplyPatch (validated,
// on a copy) → optimistic commit → event row + resource-link mirror. build
// receives the freshly-loaded graph so a retry after a conflict recomputes
// against current state. Returns the committed graph and its new revision.
func (s *Service) mutate(id string, eventKind string, build func(g *Graph, revision int64) (*Patch, string, error)) (*Graph, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < commitRetries; attempt++ {
		g, revision, err := s.Get(id)
		if err != nil {
			return nil, 0, err
		}
		if g == nil {
			return nil, 0, fmt.Errorf("no such workflow graph: %s", id)
		}
		patch, summary, err := build(g, revision)
		if err != nil {
			return nil, 0, err
		}
		if patch == nil || patch.IsEmpty() {
			return g, revision, nil // nothing to change — not an error
		}
		now := s.now()
		next, err := ApplyPatch(g, patch, now, s.validateOpts())
		if err != nil {
			return nil, 0, err
		}
		rec, err := EncodeSnapshot(next)
		if err != nil {
			return nil, 0, err
		}
		rec.CreatedAt = next.CreatedAt
		newRev, err := s.deps.Store.UpdateWorkflowGraphSnapshot(id, revision, rec)
		if errors.Is(err, domain.ErrWorkflowGraphRevisionConflict) {
			lastErr = err
			continue // reload and rebuild against the newer state
		}
		if err != nil {
			return nil, 0, err
		}
		s.recordEvent(next, newRev, eventKind, summary, patch)
		s.mirrorResourceLinks(next, patch)
		return next, newRev, nil
	}
	return nil, 0, fmt.Errorf("workflow graph %s: commit kept conflicting: %w", id, lastErr)
}

// recordEvent appends one projection event (best-effort — an event-log failure
// must never fail the committed write it describes).
func (s *Service) recordEvent(g *Graph, revision int64, kind, summary string, payload any) {
	defer func() { _ = recover() }()
	payloadJSON := "{}"
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			payloadJSON = string(b)
		}
	}
	var nodeID *string
	if p, ok := payload.(*Patch); ok && p != nil && len(p.NodeUpdates) == 1 {
		nid := p.NodeUpdates[0].ID
		nodeID = &nid
	}
	_, err := s.deps.Store.InsertWorkflowGraphEvent(domain.WorkflowGraphEventRecord{
		WorkflowID:  g.ID,
		Revision:    revision,
		Kind:        kind,
		NodeID:      nodeID,
		Summary:     clampRunes(summary, MaxSummaryRunes),
		PayloadJson: payloadJSON,
		PayloadHash: hashPayload(payloadJSON),
		CreatedAt:   s.now(),
	})
	if err != nil {
		s.trace("workflow.event.insert_failed", map[string]any{"workflowId": g.ID, "kind": kind, "error": err.Error()})
	}
}

// mirrorResourceLinks upserts the reverse-index rows for every resource the
// patch added or re-targeted (best-effort). The snapshot stays the source of
// truth; the index only accelerates reverse lookups.
func (s *Service) mirrorResourceLinks(g *Graph, p *Patch) {
	defer func() { _ = recover() }()
	touch := func(res *Resource) {
		if res == nil {
			return
		}
		var nodeID, label, status *string
		if res.NodeID != "" {
			nodeID = &res.NodeID
		}
		if res.Label != "" {
			label = &res.Label
		}
		if res.Status != "" {
			status = &res.Status
		}
		if err := s.deps.Store.UpsertWorkflowResourceLink(domain.WorkflowResourceLinkRecord{
			WorkflowID:   g.ID,
			ResourceType: res.Type,
			ResourceRef:  res.Ref,
			NodeID:       nodeID,
			Label:        label,
			Status:       status,
			MetadataJson: "{}",
		}); err != nil {
			s.trace("workflow.resource_link.upsert_failed", map[string]any{
				"workflowId": g.ID, "type": res.Type, "ref": res.Ref, "error": err.Error()})
		}
	}
	for i := range p.AddResources {
		// The committed copy carries the final IDs/timestamps; find it by (type, ref).
		touch(g.FindResource(p.AddResources[i].Type, p.AddResources[i].Ref))
	}
	for i := range p.ResourceUpdates {
		touch(g.ResourceByID(p.ResourceUpdates[i].ID))
	}
}

/* ---------------------------------- plan --------------------------------- */

// Backend WorkflowPlanInput list caps (contracts/tasks.py: active_skill_ids
// max_length 16, constraints max_length 25). maxPlanNotes leaves one constraint
// slot for the mandatory local no-file-edit constraint Plan always prepends.
const (
	maxPlanSkillIDs = 16
	maxPlanNotes    = 24
)

// PlanRequest is workflow.plan's input.
type PlanRequest struct {
	Goal               string
	Scope              string
	ExistingWorkflowID string
	ForceReplan        bool
	ActiveSkillIDs     []string
	Notes              []string
	Source             Source
}

// PlanResult is a committed plan.
type PlanResult struct {
	Graph        *Graph
	Revision     int64
	Warnings     []string
	UserQuestion *backend.MultipleChoiceQuestionOut
	// SupersededID is the prior graph cancelled by a forceReplan ("" otherwise).
	SupersededID string
}

// Plan asks the backend to turn a goal into an executable graph, validates it,
// and stores it. With ExistingWorkflowID set and ForceReplan false the call is
// refused (reconcile is the right tool for a live graph); with ForceReplan the
// prior graph is cancelled as superseded and its snapshot is handed to the
// planner as context.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if s.deps.Tasks == nil {
		return PlanResult{}, fmt.Errorf("workflow planning is unavailable: no backend task runner")
	}
	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		return PlanResult{}, fmt.Errorf("goal is required")
	}

	// Clamp the unbounded request lists to the backend's strict pydantic caps
	// (WorkflowPlanInput: active_skill_ids ≤ 16, constraints ≤ 25 — one of which
	// the mandatory local constraint below consumes). Head-truncate: earlier
	// entries are the caller's highest-signal ones. A clamped input degrades
	// gracefully where an over-cap list would 422 the whole task.
	skillIDs := req.ActiveSkillIDs
	if len(skillIDs) > maxPlanSkillIDs {
		s.trace("workflow.plan.clamped", map[string]any{
			"field": "active_skill_ids", "sent": maxPlanSkillIDs, "dropped": len(skillIDs) - maxPlanSkillIDs})
		skillIDs = skillIDs[:maxPlanSkillIDs]
	}
	notes := req.Notes
	if len(notes) > maxPlanNotes {
		s.trace("workflow.plan.clamped", map[string]any{
			"field": "notes", "sent": maxPlanNotes, "dropped": len(notes) - maxPlanNotes})
		notes = notes[:maxPlanNotes]
	}

	var existing *Graph
	var existingRevision int64
	if req.ExistingWorkflowID != "" {
		g, rev, err := s.Get(req.ExistingWorkflowID)
		if err != nil {
			return PlanResult{}, err
		}
		existingRevision = rev
		if g == nil {
			return PlanResult{}, fmt.Errorf("no such workflow graph: %s", req.ExistingWorkflowID)
		}
		if !g.Status.IsTerminal() && !req.ForceReplan {
			return PlanResult{}, fmt.Errorf("workflow %s is %s; use workflow.reconcile to adjust it, or pass forceReplan to replace it", g.ID, g.Status)
		}
		existing = g
	}

	input := backend.WorkflowPlanInput{
		Goal:           goal,
		Scope:          strings.TrimSpace(req.Scope),
		ActiveSkillIDs: skillIDs,
		Constraints: append([]string{
			"the assistant must never edit project files directly — file changes go through visible agents (agentTask.spawnForEdits)",
		}, notes...),
	}
	if s.deps.RuntimeSummary != nil {
		input.RuntimeSummary = s.deps.RuntimeSummary()
	}
	if s.deps.ToolInventory != nil {
		input.ToolInventory = s.deps.ToolInventory()
	}
	if existing != nil {
		input.ExistingWorkflow = SnapshotFromGraph(existing, existingRevision)
	}

	out, err := backend.RunWorkflowPlan(ctx, s.deps.Tasks, input)
	if err != nil {
		return PlanResult{}, fmt.Errorf("workflow_plan: %w", err)
	}

	source := req.Source
	if source == "" {
		source = SourceUser
	}
	now := s.now()
	g, warnings, err := GraphFromPlan(out, goal, source, s.deps.HasTool, now)
	if err != nil {
		return PlanResult{}, fmt.Errorf("plan rejected: %w", err)
	}
	if err := Validate(g, s.validateOpts()); err != nil {
		return PlanResult{}, fmt.Errorf("plan rejected: %w", err)
	}

	rec, err := EncodeSnapshot(g)
	if err != nil {
		return PlanResult{}, err
	}
	stored, err := s.deps.Store.InsertWorkflowGraph(rec)
	if err != nil {
		return PlanResult{}, err
	}
	s.recordEvent(g, stored.Revision, "planned", "Plan created: "+g.Goal, out)
	s.trace("workflow.planned", map[string]any{
		"workflowId": g.ID, "nodes": len(g.Nodes), "edges": len(g.Edges), "warnings": warnings})

	result := PlanResult{Graph: g, Revision: stored.Revision, Warnings: warnings, UserQuestion: out.UserQuestion}

	// Supersede the replaced graph AFTER the new one is safely stored.
	if existing != nil && !existing.Status.IsTerminal() && req.ForceReplan {
		cancelled := StatusCancelled
		if _, _, cerr := s.mutate(existing.ID, "cancelled", func(_ *Graph, rev int64) (*Patch, string, error) {
			return &Patch{WorkflowID: existing.ID, BaseRevision: rev, NewStatus: &cancelled,
				Rationale: "superseded by " + g.ID}, "Superseded by replan " + g.ID, nil
		}); cerr != nil {
			result.Warnings = append(result.Warnings, "could not cancel superseded workflow "+existing.ID+": "+cerr.Error())
		} else {
			result.SupersededID = existing.ID
		}
	}
	return result, nil
}

/* ------------------------------- reconcile -------------------------------- */

// ValidReconcileReasons is the closed reason set (mirrors the backend enum).
var ValidReconcileReasons = map[string]bool{
	"tool_result": true, "queue_event": true, "wake": true, "manual": true,
	"resume": true, "failure": true, "user_interjection": true,
}

// ReconcileRequest is workflow.reconcile's input.
type ReconcileRequest struct {
	WorkflowID        string
	Reason            string
	RecentEventLimit  int
	LatestUserMessage string
	// Apply false ⇒ preview: the validated patch is returned but not committed.
	Apply bool
}

// ReconcileResult is one reconcile outcome.
type ReconcileResult struct {
	Graph        *Graph
	Revision     int64
	Patch        *Patch
	Applied      bool
	Summary      string
	Warnings     []string
	NextAction   *domain.RecommendedAction
	UserQuestion *backend.MultipleChoiceQuestionOut
}

// defaultRecentEvents / maxRecentEvents bound the evidence window a reconcile
// call ships to the backend.
const (
	defaultRecentEvents = 20
	maxRecentEvents     = 50
)

// Reconcile hands the current graph + recent evidence to the backend's
// stateless reconciler and applies the returned patch — validated locally,
// committed under the optimistic revision it was computed against. It never
// re-plans; a goal change belongs to workflow.plan.
func (s *Service) Reconcile(ctx context.Context, req ReconcileRequest) (ReconcileResult, error) {
	if s.deps.Tasks == nil {
		return ReconcileResult{}, fmt.Errorf("workflow reconciliation is unavailable: no backend task runner")
	}
	if !ValidReconcileReasons[req.Reason] {
		return ReconcileResult{}, fmt.Errorf("invalid reconcile reason %q", req.Reason)
	}
	g, revision, err := s.Get(req.WorkflowID)
	if err != nil {
		return ReconcileResult{}, err
	}
	if g == nil {
		return ReconcileResult{}, fmt.Errorf("no such workflow graph: %s", req.WorkflowID)
	}
	if g.Status.IsTerminal() {
		return ReconcileResult{}, fmt.Errorf("workflow %s is %s; nothing to reconcile", g.ID, g.Status)
	}

	limit := req.RecentEventLimit
	if limit <= 0 {
		limit = defaultRecentEvents
	}
	if limit > maxRecentEvents {
		limit = maxRecentEvents
	}
	events, err := s.deps.Store.ListWorkflowGraphEvents(g.ID, limit)
	if err != nil {
		return ReconcileResult{}, err
	}

	// The snapshot travels in the CANONICAL snake_case wire shape (see
	// backend.WorkflowSnapshot) — never a JSON dump of the camelCase Graph,
	// which the backend's safety checks would silently fail to read.
	input := backend.WorkflowReconcileInput{
		Workflow:          SnapshotFromGraph(g, revision),
		RecentEvents:      eventMaps(events),
		LatestUserMessage: req.LatestUserMessage,
		Reason:            req.Reason,
	}
	if s.deps.RuntimeSummary != nil {
		input.RuntimeSummary = s.deps.RuntimeSummary()
	}
	if s.deps.ToolInventory != nil {
		input.ToolInventory = s.deps.ToolInventory()
	}

	inputJSON, _ := json.Marshal(input)
	runRec, _ := s.deps.Store.InsertWorkflowReconcileRun(domain.WorkflowReconcileRunRecord{
		WorkflowID:   g.ID,
		BaseRevision: revision,
		Status:       "started",
		InputHash:    hashPayload(string(inputJSON)),
	})
	finishRun := func(status string, appliedRev *int64, outputHash, warning string) {
		if runRec.ID == "" {
			return
		}
		patch := map[string]any{"status": status}
		if appliedRev != nil {
			patch["appliedRevision"] = *appliedRev
		}
		if outputHash != "" {
			patch["outputHash"] = outputHash
		}
		if warning != "" {
			patch["warning"] = clampRunes(warning, MaxReasonRunes)
		}
		_ = s.deps.Store.UpdateWorkflowReconcileRun(runRec.ID, patch)
	}

	out, err := backend.RunWorkflowReconcile(ctx, s.deps.Tasks, input)
	if err != nil {
		finishRun("failed", nil, "", err.Error())
		return ReconcileResult{}, fmt.Errorf("workflow_reconcile: %w", err)
	}
	outJSON, _ := json.Marshal(out)
	outputHash := hashPayload(string(outJSON))

	// A non-null base_revision echo that disagrees with the LOCAL revision means
	// the patch was reasoned against a stale (or replayed) snapshot: REJECT it
	// before any dry-run/apply — a patch for revision N must never mutate
	// revision N+1. A null echo stays allowed (the backend may omit it).
	if out.Patch.BaseRevision != nil && *out.Patch.BaseRevision != revision {
		verr := fmt.Errorf("patch base_revision %d does not match local revision %d",
			*out.Patch.BaseRevision, revision)
		finishRun("rejected", nil, outputHash, verr.Error())
		return ReconcileResult{}, fmt.Errorf("reconcile patch rejected: %w", verr)
	}

	patch, warnings := PatchFromWire(out, g.ID, revision, s.deps.HasTool, s.now())

	// Dry-run validation on a copy — a patch that would corrupt the graph is
	// REJECTED here regardless of Apply, so a hallucinated patch never lands.
	if _, verr := ApplyPatch(g, patch, s.now(), s.validateOpts()); verr != nil {
		finishRun("rejected", nil, outputHash, verr.Error())
		return ReconcileResult{}, fmt.Errorf("reconcile patch rejected: %w", verr)
	}

	result := ReconcileResult{
		Graph: g, Revision: revision, Patch: patch, Summary: out.Summary,
		Warnings: warnings, NextAction: patch.NextAction, UserQuestion: out.UserQuestion,
	}
	if !req.Apply {
		finishRun("preview", nil, outputHash, strings.Join(warnings, "; "))
		return result, nil
	}

	summary := out.Summary
	if summary == "" {
		summary = "Reconciled (" + req.Reason + ")"
	}
	next, newRev, err := s.mutate(g.ID, "reconciled", func(cur *Graph, rev int64) (*Patch, string, error) {
		// A concurrent write between our read and the commit means the patch was
		// computed against stale state: refuse rather than blind-apply.
		if rev != revision {
			return nil, "", fmt.Errorf("workflow %s moved from revision %d to %d during reconcile; re-run reconcile", g.ID, revision, rev)
		}
		return patch, summary, nil
	})
	if err != nil {
		finishRun("conflict", nil, outputHash, err.Error())
		return ReconcileResult{}, err
	}
	finishRun("applied", &newRev, outputHash, strings.Join(warnings, "; "))
	s.trace("workflow.reconciled", map[string]any{
		"workflowId": g.ID, "reason": req.Reason, "revision": newRev, "warnings": warnings})

	result.Graph = next
	result.Revision = newRev
	result.Applied = true
	return result, nil
}

// eventMaps projects event records to the bounded generic maps the reconcile
// input carries (canonical keys per the wire fixtures: type / node_id /
// summary / at, timestamps as RFC 3339 UTC).
func eventMaps(events []domain.WorkflowGraphEventRecord) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		m := map[string]any{
			"type":    e.Kind,
			"summary": e.Summary,
			"at":      time.UnixMilli(e.CreatedAt).UTC().Format(time.RFC3339),
		}
		if e.NodeID != nil {
			m["node_id"] = *e.NodeID
		}
		out = append(out, m)
	}
	return out
}

/* ------------------------- local graph operations ------------------------- */

// AttachResource links one external resource to a graph (and optionally a
// node) through the normal patch path.
func (s *Service) AttachResource(workflowID, nodeID string, res Resource) (*Graph, int64, error) {
	res.NodeID = nodeID
	label := res.Label
	if label == "" {
		label = res.Type + " " + res.Ref
	}
	return s.mutate(workflowID, "resource_linked", func(g *Graph, rev int64) (*Patch, string, error) {
		if nodeID != "" && g.NodeByID(nodeID) == nil {
			return nil, "", fmt.Errorf("no such node %q in workflow %s", nodeID, workflowID)
		}
		return &Patch{WorkflowID: workflowID, BaseRevision: rev, AddResources: []Resource{res}},
			"Linked " + label, nil
	})
}

// RecordEvidence appends one bounded evidence item through the patch path.
func (s *Service) RecordEvidence(workflowID, nodeID string, ev EvidenceRef) (*Graph, int64, error) {
	ev.NodeID = nodeID
	return s.mutate(workflowID, "evidence", func(g *Graph, rev int64) (*Patch, string, error) {
		if nodeID != "" && g.NodeByID(nodeID) == nil {
			return nil, "", fmt.Errorf("no such node %q in workflow %s", nodeID, workflowID)
		}
		return &Patch{WorkflowID: workflowID, BaseRevision: rev, AddEvidence: []EvidenceRef{ev}},
			clampRunes(ev.Summary, MaxSummaryRunes), nil
	})
}

// Cancel cancels one node (nodeID != "") or the whole graph (nodeID == "").
// LOCAL STATE ONLY: it never closes terminals, cancels external work, or
// touches Daintree — cleanup remains an explicit, user-approved action.
func (s *Service) Cancel(workflowID, nodeID, reason string) (*Graph, int64, error) {
	if reason == "" {
		reason = "cancelled"
	}
	return s.mutate(workflowID, "cancelled", func(g *Graph, rev int64) (*Patch, string, error) {
		p := &Patch{WorkflowID: workflowID, BaseRevision: rev, Rationale: clampRunes(reason, MaxReasonRunes)}
		if nodeID != "" {
			n := g.NodeByID(nodeID)
			if n == nil {
				return nil, "", fmt.Errorf("no such node %q in workflow %s", nodeID, workflowID)
			}
			if n.Status.IsTerminal() {
				return nil, "", fmt.Errorf("node %q is already %s", nodeID, n.Status)
			}
			st := NodeCancelled
			p.NodeUpdates = []NodePatch{{ID: nodeID, Status: &st}}
			return p, "Cancelled node " + nodeID + ": " + reason, nil
		}
		if g.Status.IsTerminal() {
			return nil, "", fmt.Errorf("workflow %s is already %s", workflowID, g.Status)
		}
		st := StatusCancelled
		p.NewStatus = &st
		return p, "Cancelled workflow: " + reason, nil
	})
}

/* ------------------------------ resume digest ----------------------------- */

// ResumeDigest ranks the active workflows into a compact resume package (the
// "where were we?" answer) via the stateless backend task.
func (s *Service) ResumeDigest(ctx context.Context, userMessage string) (backend.WorkflowResumeDigestOutput, error) {
	if s.deps.Tasks == nil {
		return backend.WorkflowResumeDigestOutput{}, fmt.Errorf("workflow resume digest is unavailable: no backend task runner")
	}
	graphs, err := s.List(OpenStatuses, 10)
	if err != nil {
		return backend.WorkflowResumeDigestOutput{}, err
	}
	if len(graphs) == 0 {
		return backend.WorkflowResumeDigestOutput{}, nil
	}
	workflows := make([]map[string]any, 0, len(graphs))
	for _, g := range graphs {
		lastEvent := ""
		if evs, eerr := s.deps.Store.ListWorkflowGraphEvents(g.ID, 1); eerr == nil && len(evs) > 0 {
			lastEvent = evs[0].Summary
		}
		workflows = append(workflows, backend.AnyToMap(BuildDigest(g, lastEvent)))
	}
	input := backend.WorkflowResumeDigestInput{Workflows: workflows, UserMessage: userMessage}
	if s.deps.RuntimeSummary != nil {
		input.RuntimeSummary = s.deps.RuntimeSummary()
	}
	out, err := backend.RunWorkflowResumeDigest(ctx, s.deps.Tasks, input)
	if err != nil {
		return backend.WorkflowResumeDigestOutput{}, fmt.Errorf("workflow_resume_digest: %w", err)
	}
	return out, nil
}

/* --------------------------------- hashing -------------------------------- */

// hashPayload returns the first 16 hex chars of the payload's sha256 — a
// stable, bounded content fingerprint for event/reconcile rows.
func hashPayload(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
