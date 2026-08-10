package workflowgraph

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// seedSingleActiveGraph stores a graph with exactly ONE in-flight node so the
// observer's default-targeting rule (single graph + single active node) fires.
func seedSingleActiveGraph(t *testing.T, svc *Service) *Graph {
	t.Helper()
	g := twoNodeGraph() // n_orient ready, n_verify pending → one active node
	seedGraph(t, svc, g)
	return g
}

func TestObserveDispatchLinksAsyncHandleToActiveNode(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := seedSingleActiveGraph(t, svc)

	res := domain.Ok("accepted", map[string]any{"asyncId": "asy_11112222"})
	res.Async = &domain.AsyncHandle{
		ID: "asy_11112222", ToolName: "terminal.await.async",
		Title: "wait for agent", TerminalIDs: []string{"terminal-abc"},
	}
	svc.ObserveDispatch(ObservedCall{
		ToolName: "terminal.await.async", Risk: "local", Outcome: "ok",
		Result: res, ToolCallID: "call_1",
	})

	cur, _, err := svc.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.FindResource("async", "asy_11112222") == nil {
		t.Fatal("async handle should be linked as a resource")
	}
	if cur.FindResource("terminal", "terminal-abc") == nil {
		t.Fatal("watched terminal should be linked as a resource")
	}
	if len(cur.Evidence) != 1 || cur.Evidence[0].NodeID != "n_orient" {
		t.Fatalf("evidence should target the single active node, got %+v", cur.Evidence)
	}
	// Reverse-index rows exist for the completion path.
	links, _ := store.FindWorkflowResourceLinks("async", "asy_11112222")
	if len(links) != 1 || links[0].WorkflowID != g.ID {
		t.Fatalf("reverse-index async link missing, got %+v", links)
	}
}

func TestObserveDispatchExtractsResultIDs(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := seedSingleActiveGraph(t, svc)

	// Representative agentTask.spawnForEdits result: nested ids at depth 1.
	svc.ObserveDispatch(ObservedCall{
		ToolName: "agentTask.spawnForEdits", Risk: "project", Outcome: "ok",
		Result: domain.Ok("spawned", map[string]any{
			"terminalId": "terminal-xyz",
			"watcherId":  "wch_12345678",
			"launch":     map[string]any{"worktreeId": "wt_4"},
		}),
	})
	cur, _, _ := svc.Get(g.ID)
	for _, want := range [][2]string{{"terminal", "terminal-xyz"}, {"watcher", "wch_12345678"}, {"worktree", "wt_4"}} {
		if cur.FindResource(want[0], want[1]) == nil {
			t.Errorf("resource %s %s should be linked", want[0], want[1])
		}
	}
}

func TestObserveDispatchHonoursExplicitWorkflowArgs(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	// TWO open graphs → default targeting is ambiguous; only explicit args land.
	g1 := twoNodeGraph()
	seedGraph(t, svc, g1)
	g2 := twoNodeGraph()
	g2.ID = "wfg_test0002"
	seedGraph(t, svc, g2)

	// No explicit target: ambiguous → recorded nowhere.
	svc.ObserveDispatch(ObservedCall{
		ToolName: "daintree.call", Risk: "terminal", Outcome: "ok",
		Result: domain.Ok("sent", nil),
	})
	for _, id := range []string{g1.ID, g2.ID} {
		cur, _, _ := svc.Get(id)
		if len(cur.Evidence) != 0 {
			t.Fatalf("ambiguous call must not attach evidence (graph %s)", id)
		}
	}

	// Explicit workflowId/workflowNodeId in args targets precisely.
	args, _ := json.Marshal(map[string]any{"workflowId": g2.ID, "workflowNodeId": "n_orient", "command": "make test"})
	svc.ObserveDispatch(ObservedCall{
		ToolName: "daintree.call", Args: args, Risk: "terminal", Outcome: "ok",
		Result: domain.Ok("sent", nil),
	})
	cur, _, _ := svc.Get(g2.ID)
	if len(cur.Evidence) != 1 || cur.Evidence[0].NodeID != "n_orient" {
		t.Fatalf("explicit args must target g2/n_orient, got %+v", cur.Evidence)
	}
}

func TestObserveDispatchMaterialityFilter(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := seedSingleActiveGraph(t, svc)

	notMaterial := []ObservedCall{
		// Plain successful read.
		{ToolName: "terminal.read", Risk: "read", Outcome: "ok", Result: domain.Ok("tail", nil)},
		// The workflow tools themselves (no self-narration).
		{ToolName: "workflow.getGraph", Risk: "read", Outcome: "ok", Result: domain.Ok("graph", nil)},
		// A user-declined confirmation.
		{ToolName: "daintree.call", Risk: "terminal", Outcome: "denied", Result: domain.Fail("USER_DECLINED", "no")},
		// Dispatch-level model slip-ups.
		{ToolName: "made.up", Risk: "", Outcome: "error", Result: domain.Fail("UNKNOWN_TOOL", "no such tool", domain.Unrecoverable())},
		// Recoverable read failure.
		{ToolName: "terminal.read", Risk: "read", Outcome: "error", Result: domain.Fail("timeout", "slow")},
	}
	for _, obs := range notMaterial {
		svc.ObserveDispatch(obs)
	}
	cur, _, _ := svc.Get(g.ID)
	if len(cur.Evidence) != 0 {
		t.Fatalf("non-material calls must record nothing, got %+v", cur.Evidence)
	}

	// An UNRECOVERABLE real-tool failure IS material.
	svc.ObserveDispatch(ObservedCall{
		ToolName: "terminal.read", Risk: "read", Outcome: "error",
		Result: domain.Fail("TERMINAL_NOT_FOUND", "gone", domain.Unrecoverable()),
	})
	cur, _, _ = svc.Get(g.ID)
	if len(cur.Evidence) != 1 {
		t.Fatalf("unrecoverable failure should be recorded, got %d", len(cur.Evidence))
	}
}

/* ----------------------------- async settlement ---------------------------- */

func TestNoteAsyncSettledTransitionsLinkedNode(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := twoNodeGraph()
	g.Nodes[0].Status = NodeWaiting // the node awaiting the async work
	g.Resources = []Resource{{ID: "wrs_1", Type: "async", Ref: "asy_11112222", NodeID: "n_orient", Status: "active"}}
	seedGraph(t, svc, g)
	nodeID := "n_orient"
	_ = store.UpsertWorkflowResourceLink(domain.WorkflowResourceLinkRecord{
		WorkflowID: g.ID, ResourceType: "async", ResourceRef: "asy_11112222", NodeID: &nodeID,
	})

	svc.NoteAsyncSettled("asy_11112222", string(domain.AsyncSucceeded), `asy_11112222 "npm test": terminal-abc: finished`, "evt_1")

	cur, _, err := svc.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := cur.NodeByID("n_orient").Status; got != NodeDone {
		t.Fatalf("waiting node should transition to done, got %s", got)
	}
	// Dependent auto-promotes; evidence recorded with the queue-event ref.
	if got := cur.NodeByID("n_verify").Status; got != NodeReady {
		t.Fatalf("dependent should become ready, got %s", got)
	}
	if len(cur.Evidence) != 1 || cur.Evidence[0].Kind != "async_completion" || cur.Evidence[0].RefID != "evt_1" {
		t.Fatalf("async completion evidence missing, got %+v", cur.Evidence)
	}
	// Snapshot resource + reverse-index row carry the final status.
	if got := cur.FindResource("async", "asy_11112222").Status; got != "succeeded" {
		t.Fatalf("resource status should be succeeded, got %s", got)
	}
	links, _ := store.FindWorkflowResourceLinks("async", "asy_11112222")
	if len(links) != 1 || links[0].Status == nil || *links[0].Status != "succeeded" {
		t.Fatalf("link row should be succeeded, got %+v", links)
	}
}

func TestNoteAsyncSettledFailureMarksNodeFailed(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := twoNodeGraph()
	g.Nodes[0].Status = NodeWaiting
	seedGraph(t, svc, g)
	nodeID := "n_orient"
	_ = store.UpsertWorkflowResourceLink(domain.WorkflowResourceLinkRecord{
		WorkflowID: g.ID, ResourceType: "async", ResourceRef: "asy_dead0001", NodeID: &nodeID,
	})

	svc.NoteAsyncSettled("asy_dead0001", string(domain.AsyncFailed), "exited with code 1", "evt_2")

	cur, _, _ := svc.Get(g.ID)
	n := cur.NodeByID("n_orient")
	if n.Status != NodeFailed {
		t.Fatalf("node should be failed, got %s", n.Status)
	}
	if n.LastError == "" {
		t.Fatal("failure summary should land in LastError")
	}
}

func TestNoteAsyncSettledUnknownAsyncIsNoOp(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := seedSingleActiveGraph(t, svc)
	svc.NoteAsyncSettled("asy_unknown1", string(domain.AsyncSucceeded), "done", "evt_3")
	cur, _, _ := svc.Get(g.ID)
	if len(cur.Evidence) != 0 {
		t.Fatal("an unlinked async settle must record nothing")
	}
}
