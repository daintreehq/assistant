package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

// fakeGraphService records calls and returns canned graphs.
type fakeGraphService struct {
	graph     *workflowgraph.Graph
	planErr   error
	lastPlan  workflowgraph.PlanRequest
	lastRecon workflowgraph.ReconcileRequest
}

func testGraph() *workflowgraph.Graph {
	return &workflowgraph.Graph{
		ID: "wfg_11223344", Goal: "fix tests", Status: workflowgraph.StatusActive,
		SchemaVersion: 1,
		Nodes: []workflowgraph.Node{
			{ID: "n_a", Title: "Orient", Kind: workflowgraph.KindOrient, Status: workflowgraph.NodeReady},
			{ID: "n_b", Title: "Verify", Kind: workflowgraph.KindVerify, Status: workflowgraph.NodePending, DependsOn: []string{"n_a"}},
		},
		ActiveNodeIDs: []string{"n_a"},
	}
}

func (f *fakeGraphService) Plan(_ context.Context, req workflowgraph.PlanRequest) (workflowgraph.PlanResult, error) {
	f.lastPlan = req
	if f.planErr != nil {
		return workflowgraph.PlanResult{}, f.planErr
	}
	return workflowgraph.PlanResult{Graph: f.graph, Revision: 1, Warnings: []string{"w1"}}, nil
}

func (f *fakeGraphService) Get(id string) (*workflowgraph.Graph, int64, error) {
	if f.graph != nil && f.graph.ID == id {
		return f.graph, 3, nil
	}
	return nil, 0, nil
}

func (f *fakeGraphService) Next(id string) (workflowgraph.NextResult, error) {
	return workflowgraph.NextResult{
		WorkflowID: id, Status: workflowgraph.StatusActive,
		ReadyNodes: []workflowgraph.Node{f.graph.Nodes[0]},
	}, nil
}

func (f *fakeGraphService) AttachResource(workflowID, nodeID string, res workflowgraph.Resource) (*workflowgraph.Graph, int64, error) {
	g := *f.graph
	res.NodeID = nodeID
	g.Resources = append(g.Resources, res)
	return &g, 4, nil
}

func (f *fakeGraphService) RecordEvidence(workflowID, nodeID string, ev workflowgraph.EvidenceRef) (*workflowgraph.Graph, int64, error) {
	g := *f.graph
	g.Evidence = append(g.Evidence, ev)
	return &g, 4, nil
}

func (f *fakeGraphService) Reconcile(_ context.Context, req workflowgraph.ReconcileRequest) (workflowgraph.ReconcileResult, error) {
	f.lastRecon = req
	return workflowgraph.ReconcileResult{Graph: f.graph, Revision: 4, Applied: req.Apply, Summary: "ok"}, nil
}

func (f *fakeGraphService) Cancel(workflowID, nodeID, reason string) (*workflowgraph.Graph, int64, error) {
	g := *f.graph
	g.Status = workflowgraph.StatusCancelled
	return &g, 5, nil
}

func graphToolset(t *testing.T, svc GraphService) map[string]*tools.Tool {
	t.Helper()
	out := map[string]*tools.Tool{}
	for _, tool := range Tools(Deps{Store: nil, Graph: svc}) {
		out[tool.Name] = tool
	}
	return out
}

func TestGraphToolsRegisterOnlyWithService(t *testing.T) {
	without := Tools(Deps{})
	if len(without) != 4 {
		t.Fatalf("feature off: want the 4 flat tools, got %d", len(without))
	}
	with := Tools(Deps{Graph: &fakeGraphService{graph: testGraph()}})
	if len(with) != 11 {
		t.Fatalf("feature on: want 4+7 tools, got %d", len(with))
	}
}

func TestPlanToolShapesResult(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)
	res := set["workflow.plan"].Handle(context.Background(),
		json.RawMessage(`{"goal": "fix tests", "activeSkillIds": ["s1"]}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("plan should succeed, got %+v", res.Error)
	}
	out := res.Result.(map[string]any)
	if out["workflowId"] != "wfg_11223344" {
		t.Fatalf("result must carry the workflowId, got %v", out)
	}
	if svc.lastPlan.Source != workflowgraph.SourceUser || len(svc.lastPlan.ActiveSkillIDs) != 1 {
		t.Fatalf("plan request mis-mapped: %+v", svc.lastPlan)
	}
	// Missing goal is INVALID_ARGS via the strict decoder.
	if dec, err := set["workflow.plan"].Decode(json.RawMessage(`{}`)); err == nil {
		t.Fatalf("empty goal must fail validation, got %s", dec)
	}
}

func TestGetGraphToolViews(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)

	res := set["workflow.getGraph"].Handle(context.Background(),
		json.RawMessage(`{"id": "wfg_11223344"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatal(res.Summary)
	}
	compact := res.Result.(map[string]any)
	if compact["progress"] != "0/2 nodes done" {
		t.Fatalf("compact view should carry progress, got %v", compact["progress"])
	}

	full := set["workflow.getGraph"].Handle(context.Background(),
		json.RawMessage(`{"id": "wfg_11223344", "view": "full"}`), &tools.ToolContext{})
	if _, ok := full.Result.(map[string]any)["graph"]; !ok {
		t.Fatal("full view should return the whole graph")
	}

	missing := set["workflow.getGraph"].Handle(context.Background(),
		json.RawMessage(`{"id": "wfg_nope0000"}`), &tools.ToolContext{})
	if missing.Ok || missing.Error.Code != codeGraphNotFound || missing.Error.Recoverable {
		t.Fatalf("missing graph must be an unrecoverable not-found, got %+v", missing.Error)
	}
}

func TestReconcileToolDefaultsApplyTrue(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)
	res := set["workflow.reconcile"].Handle(context.Background(),
		json.RawMessage(`{"workflowId": "wfg_11223344", "reason": "wake"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatal(res.Summary)
	}
	if !svc.lastRecon.Apply {
		t.Fatal("apply must default to true")
	}
	// Invalid reason rejected by the validator BEFORE the service runs.
	if _, err := set["workflow.reconcile"].Decode(json.RawMessage(`{"workflowId": "x", "reason": "vibes"}`)); err == nil {
		t.Fatal("invalid reason must fail decode")
	}
	// Explicit preview.
	res = set["workflow.reconcile"].Handle(context.Background(),
		json.RawMessage(`{"workflowId": "wfg_11223344", "reason": "manual", "apply": false}`), &tools.ToolContext{})
	if svc.lastRecon.Apply {
		t.Fatal("apply=false must be honoured")
	}
	if !strings.Contains(res.Summary, "Previewed") {
		t.Fatalf("preview summary expected, got %q", res.Summary)
	}
}

func TestAttachResourceToolValidatesType(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)
	if _, err := set["workflow.attachResource"].Decode(
		json.RawMessage(`{"workflowId": "w", "type": "spaceship", "ref": "x"}`)); err == nil {
		t.Fatal("invalid resource type must fail decode")
	}
	res := set["workflow.attachResource"].Handle(context.Background(),
		json.RawMessage(`{"workflowId": "wfg_11223344", "nodeId": "n_a", "type": "terminal", "ref": "terminal-1"}`),
		&tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("attach should succeed, got %+v", res.Error)
	}
}

func TestRecordEvidenceToolValidatesKind(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)
	if _, err := set["workflow.recordEvidence"].Decode(
		json.RawMessage(`{"workflowId": "w", "kind": "gossip", "summary": "s"}`)); err == nil {
		t.Fatal("invalid evidence kind must fail decode")
	}
	res := set["workflow.recordEvidence"].Handle(context.Background(),
		json.RawMessage(`{"workflowId": "wfg_11223344", "kind": "manual_note", "summary": "observed"}`),
		&tools.ToolContext{})
	if !res.Ok {
		t.Fatal(res.Summary)
	}
}

func TestCancelToolStatesLocalOnly(t *testing.T) {
	svc := &fakeGraphService{graph: testGraph()}
	set := graphToolset(t, svc)
	res := set["workflow.cancel"].Handle(context.Background(),
		json.RawMessage(`{"workflowId": "wfg_11223344"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatal(res.Summary)
	}
	if !strings.Contains(res.Summary, "graph state only") {
		t.Fatalf("cancel summary must state it touched local state only, got %q", res.Summary)
	}
}

func TestGraphToolRisksStayLocal(t *testing.T) {
	// The graph tools record and recommend; none may carry a mutating risk class
	// that would trigger confirmations or grant requirements.
	for name, tool := range graphToolset(t, &fakeGraphService{graph: testGraph()}) {
		if !strings.HasPrefix(name, "workflow.") {
			continue
		}
		if tool.Risk != domain.RiskRead && tool.Risk != domain.RiskLocal {
			t.Errorf("%s has risk %s; graph tools must be read/local only", name, tool.Risk)
		}
	}
}
