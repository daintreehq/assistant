package workflowgraph

import (
	"fmt"
	"strings"
)

// ValidateOptions tunes graph validation. HasTool, when set, verifies that
// NextAction.ToolName names a tool registered THIS process (a recommendation
// naming a nonexistent tool is worse than none — the model would chase it).
// nil ⇒ the tool-existence check is skipped (offline validation, tests).
type ValidateOptions struct {
	HasTool func(name string) bool
}

// Validate enforces every hard graph invariant. It returns the FIRST violation
// as an error (nil = valid). Called on every ingest path — a backend plan, a
// backend patch's post-application result, a local mutation — so no path can
// commit a corrupt graph.
func Validate(g *Graph, opts ValidateOptions) error {
	if g == nil {
		return fmt.Errorf("graph is nil")
	}
	if strings.TrimSpace(g.ID) == "" {
		return fmt.Errorf("graph id is empty")
	}
	if strings.TrimSpace(g.Goal) == "" {
		return fmt.Errorf("graph goal is empty")
	}
	if !ValidStatuses[g.Status] {
		return fmt.Errorf("invalid graph status %q", g.Status)
	}
	if len(g.Nodes) == 0 {
		return fmt.Errorf("graph has no nodes")
	}
	if len(g.Nodes) > MaxNodes {
		return fmt.Errorf("graph exceeds %d nodes (%d)", MaxNodes, len(g.Nodes))
	}
	if len(g.Edges) > MaxEdges {
		return fmt.Errorf("graph exceeds %d edges (%d)", MaxEdges, len(g.Edges))
	}
	if len(g.Resources) > MaxResources {
		return fmt.Errorf("graph exceeds %d resources (%d)", MaxResources, len(g.Resources))
	}
	if len(g.Blockers) > MaxBlockers {
		return fmt.Errorf("graph exceeds %d blockers (%d)", MaxBlockers, len(g.Blockers))
	}
	if len(g.Evidence) > MaxEvidence {
		return fmt.Errorf("graph exceeds %d evidence entries (%d)", MaxEvidence, len(g.Evidence))
	}

	// Node identity + field validity.
	nodeIDs := make(map[string]bool, len(g.Nodes))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("node %d has an empty id", i)
		}
		if nodeIDs[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		nodeIDs[n.ID] = true
		if strings.TrimSpace(n.Title) == "" {
			return fmt.Errorf("node %q has an empty title", n.ID)
		}
		if !ValidNodeKinds[n.Kind] {
			return fmt.Errorf("node %q has invalid kind %q", n.ID, n.Kind)
		}
		if !ValidNodeStatuses[n.Status] {
			return fmt.Errorf("node %q has invalid status %q", n.ID, n.Status)
		}
		if n.AsyncPolicy != "" && !ValidAsyncPolicies[n.AsyncPolicy] {
			return fmt.Errorf("node %q has invalid asyncPolicy %q", n.ID, n.AsyncPolicy)
		}
	}

	// Dependency references + edges.
	for i := range g.Nodes {
		n := &g.Nodes[i]
		for _, dep := range n.DependsOn {
			if !nodeIDs[dep] {
				return fmt.Errorf("node %q depends on unknown node %q", n.ID, dep)
			}
			if dep == n.ID {
				return fmt.Errorf("node %q depends on itself", n.ID)
			}
		}
	}
	edgeSeen := make(map[Edge]bool, len(g.Edges))
	for _, e := range g.Edges {
		if !nodeIDs[e.From] || !nodeIDs[e.To] {
			return fmt.Errorf("edge %s→%s references an unknown node", e.From, e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("edge %s→%s is a self-loop", e.From, e.To)
		}
		if edgeSeen[e] {
			return fmt.Errorf("duplicate edge %s→%s", e.From, e.To)
		}
		edgeSeen[e] = true
	}

	// Acyclicity over the UNION of DependsOn and explicit Edges — both express
	// ordering, so a cycle through either (or a mix) is equally fatal.
	if cyc := findCycle(g); cyc != "" {
		return fmt.Errorf("graph has a dependency cycle through %s", cyc)
	}

	// ActiveNodeIDs must reference existing nodes in an active status.
	for _, id := range g.ActiveNodeIDs {
		if !nodeIDs[id] {
			return fmt.Errorf("activeNodeIds references unknown node %q", id)
		}
		if !g.NodeByID(id).Status.IsActive() {
			return fmt.Errorf("activeNodeIds includes node %q with non-active status %q", id, g.NodeByID(id).Status)
		}
	}

	// Resources / blockers / decisions / evidence integrity.
	resourceIDs := make(map[string]bool, len(g.Resources))
	for i := range g.Resources {
		r := &g.Resources[i]
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("resource %d has an empty id", i)
		}
		if resourceIDs[r.ID] {
			return fmt.Errorf("duplicate resource id %q", r.ID)
		}
		resourceIDs[r.ID] = true
		if !ValidResourceTypes[r.Type] {
			return fmt.Errorf("resource %q has invalid type %q", r.ID, r.Type)
		}
		if strings.TrimSpace(r.Ref) == "" {
			return fmt.Errorf("resource %q has an empty ref", r.ID)
		}
		if r.NodeID != "" && !nodeIDs[r.NodeID] {
			return fmt.Errorf("resource %q references unknown node %q", r.ID, r.NodeID)
		}
	}
	blockerIDs := make(map[string]bool, len(g.Blockers))
	for i := range g.Blockers {
		b := &g.Blockers[i]
		if strings.TrimSpace(b.ID) == "" {
			return fmt.Errorf("blocker %d has an empty id", i)
		}
		if blockerIDs[b.ID] {
			return fmt.Errorf("duplicate blocker id %q", b.ID)
		}
		blockerIDs[b.ID] = true
		if strings.TrimSpace(b.Reason) == "" {
			return fmt.Errorf("blocker %q has an empty reason", b.ID)
		}
		if b.NodeID != "" && !nodeIDs[b.NodeID] {
			return fmt.Errorf("blocker %q references unknown node %q", b.ID, b.NodeID)
		}
	}
	for i := range g.Evidence {
		ev := &g.Evidence[i]
		if !ValidEvidenceKinds[ev.Kind] {
			return fmt.Errorf("evidence %d has invalid kind %q", i, ev.Kind)
		}
		if ev.NodeID != "" && !nodeIDs[ev.NodeID] {
			return fmt.Errorf("evidence %d references unknown node %q", i, ev.NodeID)
		}
	}

	// NextAction: a recommendation must be executable — its tool must exist in
	// the live registry, and an optional node binding must name a real node.
	// (It still carries zero authority; dispatch decides.) The ingest paths
	// (GraphFromPlan / ApplyPatch) DROP an unknown binding before validation, so
	// this is a backstop against locally-constructed corruption.
	if g.NextAction != nil {
		if strings.TrimSpace(g.NextAction.ToolName) == "" {
			return fmt.Errorf("nextAction has an empty toolName")
		}
		if opts.HasTool != nil && !opts.HasTool(g.NextAction.ToolName) {
			return fmt.Errorf("nextAction names unknown tool %q", g.NextAction.ToolName)
		}
		if g.NextAction.NodeID != "" && !nodeIDs[g.NextAction.NodeID] {
			return fmt.Errorf("nextAction references unknown node %q", g.NextAction.NodeID)
		}
	}

	// A graph is done only when nothing remains to do — every node terminal and
	// none failed — or the success criteria were EXPLICITLY waived with a reason.
	if g.Status == StatusDone && g.CriteriaWaived == "" {
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if !n.Status.IsTerminal() || n.Status == NodeFailed {
				return fmt.Errorf("graph cannot be done: node %q is %s (waive criteria explicitly to override)", n.ID, n.Status)
			}
		}
	}
	return nil
}

// findCycle runs Kahn's algorithm over the union dependency graph and returns
// a comma-joined list of node ids stuck in a cycle ("" when acyclic).
func findCycle(g *Graph) string {
	// indegree counts prerequisites; deps maps a node to its dependents.
	indegree := make(map[string]int, len(g.Nodes))
	dependents := make(map[string][]string, len(g.Nodes))
	for i := range g.Nodes {
		indegree[g.Nodes[i].ID] = 0
	}
	addEdge := func(from, to string) {
		dependents[from] = append(dependents[from], to)
		indegree[to]++
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		for _, dep := range n.DependsOn {
			addEdge(dep, n.ID)
		}
	}
	for _, e := range g.Edges {
		addEdge(e.From, e.To)
	}

	queue := make([]string, 0, len(g.Nodes))
	for i := range g.Nodes {
		if indegree[g.Nodes[i].ID] == 0 {
			queue = append(queue, g.Nodes[i].ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, dep := range dependents[id] {
			indegree[dep]--
			if indegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if visited == len(g.Nodes) {
		return ""
	}
	var stuck []string
	for i := range g.Nodes {
		if indegree[g.Nodes[i].ID] > 0 {
			stuck = append(stuck, g.Nodes[i].ID)
		}
	}
	return strings.Join(stuck, ", ")
}

// graphTransitionLegal reports whether a graph-status change is allowed.
// Non-terminal statuses move freely (done is additionally gated by Validate's
// all-nodes-terminal check); done/cancelled are frozen; failed may be revived
// to active (an explicit retry).
func graphTransitionLegal(from, to Status) bool {
	if from == to {
		return true
	}
	switch from {
	case StatusDone, StatusCancelled:
		return false
	case StatusFailed:
		return to == StatusActive
	default:
		return ValidStatuses[to]
	}
}

// nodeTransitions is the legal node status-transition table. Same-status is
// always a no-op (allowed). failed → ready/blocked are the explicit retry/park
// paths; done/skipped/cancelled are hard-terminal.
var nodeTransitions = map[NodeStatus]map[NodeStatus]bool{
	NodePending: {NodeReady: true, NodeRunning: true, NodeBlocked: true, NodeSkipped: true, NodeCancelled: true},
	NodeReady:   {NodeRunning: true, NodeWaiting: true, NodeBlocked: true, NodeSkipped: true, NodeCancelled: true, NodeDone: true, NodeFailed: true},
	NodeRunning: {NodeWaiting: true, NodeDone: true, NodeFailed: true, NodeBlocked: true, NodeCancelled: true},
	NodeWaiting: {NodeRunning: true, NodeDone: true, NodeFailed: true, NodeBlocked: true, NodeCancelled: true},
	NodeBlocked: {NodeReady: true, NodeRunning: true, NodeWaiting: true, NodeSkipped: true, NodeCancelled: true, NodeFailed: true},
	NodeFailed:  {NodeReady: true, NodeBlocked: true, NodeSkipped: true, NodeCancelled: true},
	NodeDone:    {}, NodeSkipped: {}, NodeCancelled: {},
}

// NodeTransitionLegal reports whether a node may move from → to.
func NodeTransitionLegal(from, to NodeStatus) bool {
	if from == to {
		return true
	}
	allowed, ok := nodeTransitions[from]
	return ok && allowed[to]
}
