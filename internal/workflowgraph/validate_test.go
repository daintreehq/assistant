package workflowgraph

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// twoNodeGraph is the minimal valid fixture: orient → verify.
func twoNodeGraph() *Graph {
	return &Graph{
		ID:            "wfg_test0001",
		Goal:          "fix the watcher tests",
		Status:        StatusActive,
		SchemaVersion: SnapshotSchemaVersion,
		Nodes: []Node{
			{ID: "n_orient", Title: "Orient on repo", Kind: KindOrient, Status: NodeReady},
			{ID: "n_verify", Title: "Verify tests", Kind: KindVerify, Status: NodePending, DependsOn: []string{"n_orient"}},
		},
		ActiveNodeIDs: []string{"n_orient"},
		CreatedAt:     1, UpdatedAt: 1,
	}
}

func wantInvalid(t *testing.T, g *Graph, opts ValidateOptions, fragment string) {
	t.Helper()
	err := Validate(g, opts)
	if err == nil {
		t.Fatalf("want validation error containing %q, got nil", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want error containing %q, got %q", fragment, err.Error())
	}
}

func TestValidateAcceptsMinimalGraph(t *testing.T) {
	if err := Validate(twoNodeGraph(), ValidateOptions{}); err != nil {
		t.Fatalf("minimal graph should validate, got %v", err)
	}
}

func TestValidateDuplicateNodeIDs(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes = append(g.Nodes, Node{ID: "n_orient", Title: "dup", Kind: KindReport, Status: NodePending})
	wantInvalid(t, g, ValidateOptions{}, "duplicate node id")
}

func TestValidateEdgeEndpointsMustExist(t *testing.T) {
	g := twoNodeGraph()
	g.Edges = []Edge{{From: "n_orient", To: "n_missing"}}
	wantInvalid(t, g, ValidateOptions{}, "unknown node")
}

func TestValidateDependsOnMustExist(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes[1].DependsOn = []string{"n_ghost"}
	wantInvalid(t, g, ValidateOptions{}, "unknown node")
}

func TestValidateCycleRejected(t *testing.T) {
	g := twoNodeGraph()
	// Cycle THROUGH THE MIX of DependsOn and an explicit edge: verify depends on
	// orient (fixture) and an edge orders verify → orient.
	g.Edges = []Edge{{From: "n_verify", To: "n_orient"}}
	wantInvalid(t, g, ValidateOptions{}, "cycle")
}

func TestValidateSelfDependencyRejected(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes[0].DependsOn = []string{"n_orient"}
	wantInvalid(t, g, ValidateOptions{}, "itself")
}

func TestValidateInvalidStatusesAndKinds(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes[0].Kind = "made_up"
	wantInvalid(t, g, ValidateOptions{}, "invalid kind")

	g = twoNodeGraph()
	g.Nodes[0].Status = "levitating"
	wantInvalid(t, g, ValidateOptions{}, "invalid status")

	g = twoNodeGraph()
	g.Status = "half-done"
	wantInvalid(t, g, ValidateOptions{}, "invalid graph status")
}

func TestValidateActiveNodeIDsMustBeActive(t *testing.T) {
	g := twoNodeGraph()
	g.ActiveNodeIDs = []string{"n_verify"} // pending — not an active status
	wantInvalid(t, g, ValidateOptions{}, "non-active status")

	g = twoNodeGraph()
	g.ActiveNodeIDs = []string{"n_ghost"}
	wantInvalid(t, g, ValidateOptions{}, "unknown node")
}

func TestValidateNextActionToolMustExist(t *testing.T) {
	g := twoNodeGraph()
	g.NextAction = &domain.RecommendedAction{Label: "run tests", ToolName: "terminal.sendCommand"}
	hasTool := func(name string) bool { return name == "workflow.getGraph" }
	wantInvalid(t, g, ValidateOptions{HasTool: hasTool}, "unknown tool")

	// Without a HasTool hook the existence check is skipped (offline validation).
	if err := Validate(g, ValidateOptions{}); err != nil {
		t.Fatalf("nil HasTool should skip the existence check, got %v", err)
	}
}

func TestValidateDoneRequiresTerminalNonFailedNodes(t *testing.T) {
	g := twoNodeGraph()
	g.Status = StatusDone
	g.ActiveNodeIDs = nil
	g.Nodes[0].Status = NodeDone
	g.Nodes[1].Status = NodeRunning
	wantInvalid(t, g, ValidateOptions{}, "cannot be done")

	// A failed node also blocks done…
	g.Nodes[1].Status = NodeFailed
	wantInvalid(t, g, ValidateOptions{}, "cannot be done")

	// …unless the criteria were explicitly waived with a reason.
	g.CriteriaWaived = "user accepted partial completion"
	if err := Validate(g, ValidateOptions{}); err != nil {
		t.Fatalf("waived criteria should allow done, got %v", err)
	}

	// All nodes done/skipped ⇒ done without a waiver.
	g.CriteriaWaived = ""
	g.Nodes[1].Status = NodeSkipped
	if err := Validate(g, ValidateOptions{}); err != nil {
		t.Fatalf("all-terminal graph should be done-able, got %v", err)
	}
}

func TestNodeTransitionTable(t *testing.T) {
	legal := []struct{ from, to NodeStatus }{
		{NodePending, NodeReady}, {NodeReady, NodeRunning}, {NodeRunning, NodeWaiting},
		{NodeWaiting, NodeDone}, {NodeRunning, NodeFailed}, {NodeFailed, NodeReady},
		{NodeBlocked, NodeReady}, {NodeReady, NodeDone}, {NodeDone, NodeDone}, // same-status no-op
	}
	for _, tc := range legal {
		if !NodeTransitionLegal(tc.from, tc.to) {
			t.Errorf("%s → %s should be legal", tc.from, tc.to)
		}
	}
	illegal := []struct{ from, to NodeStatus }{
		{NodeDone, NodeRunning}, {NodeCancelled, NodeReady}, {NodeSkipped, NodeRunning},
		{NodePending, NodeDone}, {NodePending, NodeWaiting}, {NodeDone, NodeFailed},
	}
	for _, tc := range illegal {
		if NodeTransitionLegal(tc.from, tc.to) {
			t.Errorf("%s → %s should be illegal", tc.from, tc.to)
		}
	}
}

func TestGraphTransitionLegality(t *testing.T) {
	if !graphTransitionLegal(StatusActive, StatusBlocked) || !graphTransitionLegal(StatusPlanned, StatusActive) {
		t.Error("non-terminal graph transitions should be legal")
	}
	if graphTransitionLegal(StatusDone, StatusActive) || graphTransitionLegal(StatusCancelled, StatusActive) {
		t.Error("done/cancelled graphs are frozen")
	}
	if !graphTransitionLegal(StatusFailed, StatusActive) {
		t.Error("failed → active is the explicit retry path")
	}
	if graphTransitionLegal(StatusFailed, StatusDone) {
		t.Error("failed → done must go through active")
	}
}
