package workflowgraph

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func statusPtr(s NodeStatus) *NodeStatus { return &s }

func TestApplyPatchLegalTransitionAndTimestamps(t *testing.T) {
	g := twoNodeGraph()
	next, err := ApplyPatch(g, &Patch{
		WorkflowID:  g.ID,
		NodeUpdates: []NodePatch{{ID: "n_orient", Status: statusPtr(NodeRunning)}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := next.NodeByID("n_orient")
	if n.Status != NodeRunning {
		t.Fatalf("want running, got %s", n.Status)
	}
	if n.StartedAt == nil || *n.StartedAt != 100 {
		t.Fatalf("running must stamp StartedAt, got %v", n.StartedAt)
	}
	if next.UpdatedAt != 100 {
		t.Fatalf("graph UpdatedAt must be stamped, got %d", next.UpdatedAt)
	}

	// Complete it: CompletedAt stamps and the dependent pending node auto-promotes
	// to ready (RecomputeDerived) with ActiveNodeIDs rebuilt mechanically.
	final, err := ApplyPatch(next, &Patch{
		NodeUpdates: []NodePatch{{ID: "n_orient", Status: statusPtr(NodeDone)}},
	}, 200, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if final.NodeByID("n_orient").CompletedAt == nil {
		t.Fatal("done must stamp CompletedAt")
	}
	if got := final.NodeByID("n_verify").Status; got != NodeReady {
		t.Fatalf("dependent should auto-promote to ready, got %s", got)
	}
	if len(final.ActiveNodeIDs) != 1 || final.ActiveNodeIDs[0] != "n_verify" {
		t.Fatalf("ActiveNodeIDs should rebuild to [n_verify], got %v", final.ActiveNodeIDs)
	}
}

func TestApplyPatchIllegalTransitionRejected(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes[0].Status = NodeDone
	g.ActiveNodeIDs = nil
	_, err := ApplyPatch(g, &Patch{
		NodeUpdates: []NodePatch{{ID: "n_orient", Status: statusPtr(NodeRunning)}},
	}, 100, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "illegal transition") {
		t.Fatalf("want illegal-transition error, got %v", err)
	}
}

func TestApplyPatchUnknownNodeRejected(t *testing.T) {
	g := twoNodeGraph()
	_, err := ApplyPatch(g, &Patch{
		NodeUpdates: []NodePatch{{ID: "n_ghost", Status: statusPtr(NodeDone)}},
	}, 100, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown node") {
		t.Fatalf("want unknown-node error, got %v", err)
	}
}

func TestApplyPatchNeverMutatesInput(t *testing.T) {
	g := twoNodeGraph()
	// A patch that FAILS validation (adds a cycle) must leave g untouched…
	_, err := ApplyPatch(g, &Patch{AddEdges: []Edge{{From: "n_verify", To: "n_orient"}}}, 100, ValidateOptions{})
	if err == nil {
		t.Fatal("cycle-adding patch should fail")
	}
	if len(g.Edges) != 0 {
		t.Fatal("failed patch mutated the input graph")
	}
	// …and a SUCCESSFUL one too (commit-or-nothing on a copy).
	next, err := ApplyPatch(g, &Patch{
		NodeUpdates: []NodePatch{{ID: "n_orient", Status: statusPtr(NodeRunning)}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g.NodeByID("n_orient").Status != NodeReady || next.NodeByID("n_orient").Status != NodeRunning {
		t.Fatal("ApplyPatch must operate on a copy, never the input")
	}
}

func TestApplyPatchResourcesUpsertByTypeRef(t *testing.T) {
	g := twoNodeGraph()
	next, err := ApplyPatch(g, &Patch{
		AddResources: []Resource{{Type: "terminal", Ref: "terminal-abc", NodeID: "n_orient", Label: "agent"}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Resources) != 1 || next.Resources[0].ID == "" {
		t.Fatalf("resource should be added with a generated id, got %+v", next.Resources)
	}
	// Re-adding the same (type, ref) updates in place instead of duplicating.
	again, err := ApplyPatch(next, &Patch{
		AddResources: []Resource{{Type: "terminal", Ref: "terminal-abc", Status: "done"}},
	}, 200, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Resources) != 1 {
		t.Fatalf("re-link must not duplicate, got %d resources", len(again.Resources))
	}
	if again.Resources[0].Status != "done" || again.Resources[0].NodeID != "n_orient" {
		t.Fatalf("re-link should update status and keep the node link, got %+v", again.Resources[0])
	}
}

func TestApplyPatchBlockersAddResolve(t *testing.T) {
	g := twoNodeGraph()
	next, err := ApplyPatch(g, &Patch{
		AddBlockers: []Blocker{{NodeID: "n_orient", Reason: "waiting on user decision"}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.OpenBlockers()) != 1 {
		t.Fatal("blocker should be open")
	}
	id := next.Blockers[0].ID
	resolved, err := ApplyPatch(next, &Patch{ResolveBlockers: []string{id}}, 200, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.OpenBlockers()) != 0 {
		t.Fatal("blocker should be resolved")
	}
	if _, err := ApplyPatch(next, &Patch{ResolveBlockers: []string{"blk_ghost"}}, 200, ValidateOptions{}); err == nil {
		t.Fatal("resolving an unknown blocker should fail")
	}
}

func TestApplyPatchEvidenceRingPrunesOldest(t *testing.T) {
	g := twoNodeGraph()
	for i := 0; i < MaxEvidence; i++ {
		g.Evidence = append(g.Evidence, EvidenceRef{
			ID: fmt.Sprintf("evd_%04d", i), Kind: "manual_note", Summary: "old", CreatedAt: int64(i),
		})
	}
	next, err := ApplyPatch(g, &Patch{
		AddEvidence: []EvidenceRef{{Kind: "tool_result", Summary: "newest"}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Evidence) != MaxEvidence {
		t.Fatalf("evidence must stay capped at %d, got %d", MaxEvidence, len(next.Evidence))
	}
	if next.Evidence[len(next.Evidence)-1].Summary != "newest" {
		t.Fatal("newest evidence must survive the prune")
	}
	if next.Evidence[0].ID == "evd_0000" {
		t.Fatal("the OLDEST entry should have been pruned")
	}
}

func TestApplyPatchGraphStatusAndNextAction(t *testing.T) {
	g := twoNodeGraph()
	blocked := StatusBlocked
	next, err := ApplyPatch(g, &Patch{
		NewStatus:  &blocked,
		NextAction: &domain.RecommendedAction{Label: "ask the user", ToolName: "user.askMultipleChoice"},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != StatusBlocked || next.NextAction == nil {
		t.Fatalf("status+nextAction should apply, got %s %v", next.Status, next.NextAction)
	}
	cleared, err := ApplyPatch(next, &Patch{ClearNextAction: true}, 200, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.NextAction != nil {
		t.Fatal("ClearNextAction should clear")
	}

	// Terminal graphs freeze: cancelled → active is rejected.
	cancelled := StatusCancelled
	dead, err := ApplyPatch(g, &Patch{NewStatus: &cancelled}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dead.CompletedAt == nil {
		t.Fatal("terminal status must stamp CompletedAt")
	}
	active := StatusActive
	if _, err := ApplyPatch(dead, &Patch{NewStatus: &active}, 200, ValidateOptions{}); err == nil {
		t.Fatal("cancelled → active must be rejected")
	}
}

// A next action's node binding is an advisory hint: a binding that names a node
// the graph has (including one added by the SAME patch) survives; one that names
// an unknown node is dropped while the action itself is kept.
func TestApplyPatchNextActionNodeBinding(t *testing.T) {
	g := twoNodeGraph()
	next, err := ApplyPatch(g, &Patch{
		NextAction: &domain.RecommendedAction{
			Label: "verify", ToolName: "user.askMultipleChoice", NodeID: "n_verify"},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if next.NextAction == nil || next.NextAction.NodeID != "n_verify" {
		t.Fatalf("a known node binding must survive, got %+v", next.NextAction)
	}

	dropped, err := ApplyPatch(g, &Patch{
		NextAction: &domain.RecommendedAction{
			Label: "verify", ToolName: "user.askMultipleChoice", NodeID: "n_ghost"},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dropped.NextAction == nil || dropped.NextAction.NodeID != "" {
		t.Fatalf("an unknown node binding must be dropped (action kept), got %+v", dropped.NextAction)
	}
}

func TestApplyPatchFailedNodeRetryClearsCompletion(t *testing.T) {
	g := twoNodeGraph()
	g.Nodes[0].Status = NodeFailed
	ts := int64(50)
	g.Nodes[0].CompletedAt = &ts
	g.Nodes[0].LastError = "boom"
	next, err := ApplyPatch(g, &Patch{
		NodeUpdates: []NodePatch{{ID: "n_orient", Status: statusPtr(NodeReady), BumpAttempts: true}},
	}, 100, ValidateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := next.NodeByID("n_orient")
	if n.CompletedAt != nil {
		t.Fatal("a revived node must clear CompletedAt")
	}
	if n.Attempts != 1 {
		t.Fatalf("BumpAttempts should increment, got %d", n.Attempts)
	}
}
