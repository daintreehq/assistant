package workflowgraph

import (
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Patch is one atomic set of graph mutations — the local form of the backend's
// WorkflowPatchOut, and the shape every local mutation (attach/evidence/cancel)
// also flows through so there is exactly ONE write path. BaseRevision is the
// graph revision the patch was computed against; the storage layer rejects the
// write when the stored revision moved (optimistic concurrency).
type Patch struct {
	WorkflowID   string `json:"workflowId"`
	BaseRevision int64  `json:"baseRevision"`

	NewStatus       *Status                   `json:"newStatus,omitempty"`
	NodeUpdates     []NodePatch               `json:"nodeUpdates,omitempty"`
	AddNodes        []Node                    `json:"addNodes,omitempty"`
	AddEdges        []Edge                    `json:"addEdges,omitempty"`
	ResourceUpdates []ResourcePatch           `json:"resourceUpdates,omitempty"`
	AddResources    []Resource                `json:"addResources,omitempty"`
	AddBlockers     []Blocker                 `json:"addBlockers,omitempty"`
	ResolveBlockers []string                  `json:"resolveBlockers,omitempty"`
	AddDecisions    []Decision                `json:"addDecisions,omitempty"`
	AddEvidence     []EvidenceRef             `json:"addEvidence,omitempty"`
	NextAction      *domain.RecommendedAction `json:"nextAction,omitempty"`
	ClearNextAction bool                      `json:"clearNextAction,omitempty"`
	CriteriaWaived  *string                   `json:"criteriaWaived,omitempty"`
	Rationale       string                    `json:"rationale,omitempty"`
}

// IsEmpty reports whether the patch mutates nothing.
func (p *Patch) IsEmpty() bool {
	return p.NewStatus == nil && len(p.NodeUpdates) == 0 && len(p.AddNodes) == 0 &&
		len(p.AddEdges) == 0 && len(p.ResourceUpdates) == 0 && len(p.AddResources) == 0 &&
		len(p.AddBlockers) == 0 && len(p.ResolveBlockers) == 0 && len(p.AddDecisions) == 0 &&
		len(p.AddEvidence) == 0 && p.NextAction == nil && !p.ClearNextAction &&
		p.CriteriaWaived == nil
}

// NodePatch mutates one existing node. Pointer fields distinguish "omitted"
// from "set". A Status change is validated against the transition table.
type NodePatch struct {
	ID        string      `json:"id"`
	Status    *NodeStatus `json:"status,omitempty"`
	Title     *string     `json:"title,omitempty"`
	Owner     *string     `json:"owner,omitempty"`
	LastError *string     `json:"lastError,omitempty"`
	// BumpAttempts increments the node's attempt counter (a retry marker).
	BumpAttempts bool `json:"bumpAttempts,omitempty"`
}

// ResourcePatch mutates one existing resource (matched by resource ID).
type ResourcePatch struct {
	ID     string  `json:"id"`
	Status *string `json:"status,omitempty"`
	NodeID *string `json:"nodeId,omitempty"`
	Label  *string `json:"label,omitempty"`
}

// ApplyPatch applies p to a DEEP COPY of g, validates the result, and returns
// the new graph. The input graph is never mutated, so a validation failure
// leaves the caller's state untouched — commit-or-nothing. now stamps every
// touched timestamp (the caller owns the clock).
func ApplyPatch(g *Graph, p *Patch, now int64, opts ValidateOptions) (*Graph, error) {
	if g == nil || p == nil {
		return nil, fmt.Errorf("nil graph or patch")
	}
	out, err := cloneGraph(g)
	if err != nil {
		return nil, fmt.Errorf("clone graph: %w", err)
	}

	// 1. Node updates (before AddNodes, so a patch cannot "update" a node it is
	//    adding in the same breath — additions arrive in their declared state).
	for _, np := range p.NodeUpdates {
		n := out.NodeByID(np.ID)
		if n == nil {
			return nil, fmt.Errorf("nodeUpdates references unknown node %q", np.ID)
		}
		if np.Status != nil && *np.Status != n.Status {
			if !ValidNodeStatuses[*np.Status] {
				return nil, fmt.Errorf("node %q: invalid status %q", np.ID, *np.Status)
			}
			if !NodeTransitionLegal(n.Status, *np.Status) {
				return nil, fmt.Errorf("node %q: illegal transition %s → %s", np.ID, n.Status, *np.Status)
			}
			applyNodeStatus(n, *np.Status, now)
		}
		if np.Title != nil {
			n.Title = clampRunes(*np.Title, MaxTitleRunes)
		}
		if np.Owner != nil {
			n.Owner = *np.Owner
		}
		if np.LastError != nil {
			n.LastError = clampRunes(*np.LastError, MaxSummaryRunes)
		}
		if np.BumpAttempts {
			n.Attempts++
		}
		n.UpdatedAt = now
	}

	// 2. Additions.
	for _, n := range p.AddNodes {
		node := n
		if node.Status == "" {
			node.Status = NodePending
		}
		node.Title = clampRunes(node.Title, MaxTitleRunes)
		if node.CreatedAt == 0 {
			node.CreatedAt = now
		}
		node.UpdatedAt = now
		out.Nodes = append(out.Nodes, node)
	}
	out.Edges = append(out.Edges, p.AddEdges...)

	for _, rp := range p.ResourceUpdates {
		r := out.ResourceByID(rp.ID)
		if r == nil {
			return nil, fmt.Errorf("resourceUpdates references unknown resource %q", rp.ID)
		}
		if rp.Status != nil {
			r.Status = *rp.Status
		}
		if rp.NodeID != nil {
			r.NodeID = *rp.NodeID
		}
		if rp.Label != nil {
			r.Label = clampRunes(*rp.Label, MaxTitleRunes)
		}
		r.UpdatedAt = now
	}
	for _, r := range p.AddResources {
		res := r
		if res.ID == "" {
			res.ID = domain.NewID(domain.PrefixWorkflowResource)
		}
		res.Label = clampRunes(res.Label, MaxTitleRunes)
		if res.CreatedAt == 0 {
			res.CreatedAt = now
		}
		res.UpdatedAt = now
		// Re-linking an existing (type, ref) updates in place instead of duplicating.
		if existing := out.FindResource(res.Type, res.Ref); existing != nil {
			if res.NodeID != "" {
				existing.NodeID = res.NodeID
			}
			if res.Label != "" {
				existing.Label = res.Label
			}
			if res.Status != "" {
				existing.Status = res.Status
			}
			existing.UpdatedAt = now
			continue
		}
		out.Resources = append(out.Resources, res)
	}

	for _, b := range p.AddBlockers {
		blocker := b
		if blocker.ID == "" {
			blocker.ID = domain.NewID("blk_")
		}
		blocker.Reason = clampRunes(blocker.Reason, MaxReasonRunes)
		if blocker.CreatedAt == 0 {
			blocker.CreatedAt = now
		}
		out.Blockers = append(out.Blockers, blocker)
	}
	for _, id := range p.ResolveBlockers {
		resolved := false
		for i := range out.Blockers {
			if out.Blockers[i].ID == id {
				if out.Blockers[i].ResolvedAt == nil {
					ts := now
					out.Blockers[i].ResolvedAt = &ts
				}
				resolved = true
				break
			}
		}
		if !resolved {
			return nil, fmt.Errorf("resolveBlockers references unknown blocker %q", id)
		}
	}

	for _, d := range p.AddDecisions {
		dec := d
		if dec.ID == "" {
			dec.ID = domain.NewID("dcn_")
		}
		dec.Summary = clampRunes(dec.Summary, MaxSummaryRunes)
		dec.Rationale = clampRunes(dec.Rationale, MaxReasonRunes)
		if dec.CreatedAt == 0 {
			dec.CreatedAt = now
		}
		out.Decisions = append(out.Decisions, dec)
		if len(out.Decisions) > MaxDecisions {
			out.Decisions = out.Decisions[len(out.Decisions)-MaxDecisions:]
		}
	}

	for _, ev := range p.AddEvidence {
		item := ev
		if item.ID == "" {
			item.ID = domain.NewID("evd_")
		}
		item.Summary = clampRunes(item.Summary, MaxSummaryRunes)
		if item.CreatedAt == 0 {
			item.CreatedAt = now
		}
		out.Evidence = append(out.Evidence, item)
	}
	// Evidence is a bounded ring: prune the OLDEST entries past the cap (the
	// workflow_events table keeps the full history).
	if len(out.Evidence) > MaxEvidence {
		out.Evidence = out.Evidence[len(out.Evidence)-MaxEvidence:]
	}

	// 3. Next action / criteria waiver / status.
	if p.ClearNextAction {
		out.NextAction = nil
	}
	if p.NextAction != nil {
		na := *p.NextAction
		na.Label = clampRunes(na.Label, MaxTitleRunes)
		out.NextAction = &na
	}
	if p.CriteriaWaived != nil {
		out.CriteriaWaived = clampRunes(*p.CriteriaWaived, MaxReasonRunes)
	}
	if p.NewStatus != nil && *p.NewStatus != out.Status {
		if !ValidStatuses[*p.NewStatus] {
			return nil, fmt.Errorf("invalid graph status %q", *p.NewStatus)
		}
		if !graphTransitionLegal(out.Status, *p.NewStatus) {
			return nil, fmt.Errorf("illegal graph transition %s → %s", out.Status, *p.NewStatus)
		}
		out.Status = *p.NewStatus
		if out.Status.IsTerminal() && out.CompletedAt == nil {
			ts := now
			out.CompletedAt = &ts
		}
	}

	// 4. Recompute derived state, stamp, validate — commit-or-nothing.
	RecomputeDerived(out, now)
	out.UpdatedAt = now
	if err := Validate(out, opts); err != nil {
		return nil, fmt.Errorf("patch produces invalid graph: %w", err)
	}
	return out, nil
}

// applyNodeStatus performs one legal node transition with its timestamp
// side-effects (StartedAt on first run, CompletedAt on terminal).
func applyNodeStatus(n *Node, to NodeStatus, now int64) {
	n.Status = to
	if to == NodeRunning && n.StartedAt == nil {
		ts := now
		n.StartedAt = &ts
	}
	if to.IsTerminal() {
		if n.CompletedAt == nil {
			ts := now
			n.CompletedAt = &ts
		}
	} else {
		// A revived node (failed → ready) is no longer complete.
		n.CompletedAt = nil
	}
}

// RecomputeDerived refreshes the graph's mechanical state: pending nodes whose
// prerequisites are all satisfied become ready, and ActiveNodeIDs is rebuilt
// from node statuses (never hand-maintained, so it cannot drift).
func RecomputeDerived(g *Graph, now int64) {
	prereqs := prerequisiteIndex(g)
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Status != NodePending {
			continue
		}
		ok := true
		for _, dep := range prereqs[n.ID] {
			depNode := g.NodeByID(dep)
			if depNode == nil || !depNode.Status.satisfiesDependency() {
				ok = false
				break
			}
		}
		if ok {
			n.Status = NodeReady
			n.UpdatedAt = now
		}
	}
	active := make([]string, 0, len(g.Nodes))
	for i := range g.Nodes {
		if g.Nodes[i].Status.IsActive() {
			active = append(active, g.Nodes[i].ID)
		}
	}
	g.ActiveNodeIDs = active
}

// prerequisiteIndex maps node id → the ids that must complete first (union of
// DependsOn and incoming Edges, deduped).
func prerequisiteIndex(g *Graph) map[string][]string {
	out := make(map[string][]string, len(g.Nodes))
	seen := make(map[string]map[string]bool, len(g.Nodes))
	add := func(node, prereq string) {
		if seen[node] == nil {
			seen[node] = make(map[string]bool)
		}
		if seen[node][prereq] {
			return
		}
		seen[node][prereq] = true
		out[node] = append(out[node], prereq)
	}
	for i := range g.Nodes {
		for _, dep := range g.Nodes[i].DependsOn {
			add(g.Nodes[i].ID, dep)
		}
	}
	for _, e := range g.Edges {
		add(e.To, e.From)
	}
	return out
}

// ReadyNodes returns the nodes currently in status ready (the locally-computed
// "what can run now" answer behind workflow.next).
func ReadyNodes(g *Graph) []Node {
	var out []Node
	for i := range g.Nodes {
		if g.Nodes[i].Status == NodeReady {
			out = append(out, g.Nodes[i])
		}
	}
	return out
}

// cloneGraph deep-copies via a JSON round-trip. The graph is small by
// construction (see the caps), so the simplicity beats a hand-written copier
// that would silently miss a future field.
func cloneGraph(g *Graph) (*Graph, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	var out Graph
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// clampRunes truncates s to at most max runes (multibyte-safe).
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
