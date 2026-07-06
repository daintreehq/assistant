package workflowgraph

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestBuildDigestShape(t *testing.T) {
	g := twoNodeGraph()
	g.Goal = "fix the\nwatcher tests" // embedded newline must flatten
	g.Nodes[0].Status = NodeRunning
	g.Resources = []Resource{
		{ID: "r1", Type: "terminal", Ref: "terminal-abc", Label: "Claude repair", Status: "active"},
		{ID: "r2", Type: "async", Ref: "asy_11112222", Status: "succeeded"},
	}
	g.Blockers = []Blocker{{ID: "b1", NodeID: "n_verify", Reason: "waiting on branch choice"}}
	g.NextAction = &domain.RecommendedAction{Label: "Run the tests", ToolName: "terminal.await.async"}

	d := BuildDigest(g, "Agent reported fixes complete")
	if strings.Contains(d.Goal, "\n") {
		t.Fatal("goal must be one line")
	}
	if d.Progress != "0/2 nodes done; current: Orient on repo" {
		t.Fatalf("unexpected progress: %q", d.Progress)
	}
	if len(d.ActiveNodes) != 1 || !strings.Contains(d.ActiveNodes[0], "n_orient") {
		t.Fatalf("active nodes should list the running node, got %v", d.ActiveNodes)
	}
	// Resources render newest-first with label/status.
	if len(d.Resources) != 2 || !strings.HasPrefix(d.Resources[0], "async asy_11112222") {
		t.Fatalf("resources should be newest-first, got %v", d.Resources)
	}
	if len(d.Blockers) != 1 || !strings.Contains(d.Blockers[0], "n_verify") {
		t.Fatalf("blockers should render node-tagged, got %v", d.Blockers)
	}
	if d.NextAction != "Run the tests (terminal.await.async)" {
		t.Fatalf("unexpected next action: %q", d.NextAction)
	}
	if d.LastEvent != "Agent reported fixes complete" {
		t.Fatalf("unexpected last event: %q", d.LastEvent)
	}
}

func TestBuildDigestListCaps(t *testing.T) {
	g := twoNodeGraph()
	// 10 active nodes / 12 resources / 8 open blockers → capped lists.
	g.Nodes = nil
	for i := 0; i < 10; i++ {
		g.Nodes = append(g.Nodes, Node{
			ID: "n_" + strings.Repeat("x", i+1), Title: "node", Kind: KindOrient, Status: NodeReady,
		})
	}
	g.ActiveNodeIDs = nil
	for i := 0; i < 12; i++ {
		g.Resources = append(g.Resources, Resource{
			ID: "r" + strings.Repeat("y", i+1), Type: "terminal", Ref: "t" + strings.Repeat("y", i+1),
		})
	}
	for i := 0; i < 8; i++ {
		g.Blockers = append(g.Blockers, Blocker{ID: "b" + strings.Repeat("z", i+1), Reason: "stuck"})
	}
	d := BuildDigest(g, "")
	if len(d.ActiveNodes) > digestMaxActiveNodes {
		t.Fatalf("active nodes capped at %d, got %d", digestMaxActiveNodes, len(d.ActiveNodes))
	}
	if len(d.Resources) > digestMaxResources {
		t.Fatalf("resources capped at %d, got %d", digestMaxResources, len(d.Resources))
	}
	if len(d.Blockers) > digestMaxBlockers {
		t.Fatalf("blockers capped at %d, got %d", digestMaxBlockers, len(d.Blockers))
	}
}

func TestCapWorkflowDigestsWireBounds(t *testing.T) {
	long := strings.Repeat("very long line of workflow context ", 30)
	var in []backend.WorkflowDigest
	for i := 0; i < 10; i++ {
		in = append(in, backend.WorkflowDigest{
			ID: "wfg_x", Goal: long, Status: "active",
			Progress:    long,
			ActiveNodes: []string{long, long, long, long},
			Resources:   []string{long, long, long, long, long, long},
			Blockers:    []string{long},
			NextAction:  long,
			LastEvent:   long,
		})
	}
	out := backend.CapWorkflowDigests(in)
	if len(out) == 0 || len(out) > backend.MaxWorkflowDigests {
		t.Fatalf("want 1..%d digests, got %d", backend.MaxWorkflowDigests, len(out))
	}
	for _, d := range out {
		if n := len([]rune(d.Goal)); n > 512 {
			t.Fatalf("goal must clamp to 512 runes, got %d", n)
		}
		for _, l := range d.ActiveNodes {
			if len([]rune(l)) > 512 {
				t.Fatal("active-node lines must clamp to 512 runes")
			}
		}
	}
	if backend.CapWorkflowDigests(nil) != nil {
		t.Fatal("nil in → nil out")
	}
}

func TestSnapshotCodecRoundTrip(t *testing.T) {
	g := twoNodeGraph()
	g.Resources = []Resource{{ID: "r1", Type: "terminal", Ref: "terminal-abc", Metadata: map[string]any{"k": "v"}}}
	g.NextAction = &domain.RecommendedAction{Label: "go", ToolName: "workflow.next"}

	rec, err := EncodeSnapshot(g)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != string(StatusActive) || rec.Goal != g.Goal || rec.SchemaVersion != SnapshotSchemaVersion {
		t.Fatalf("promoted columns mismatch: %+v", rec)
	}
	back, err := DecodeSnapshot(&rec)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != g.ID || len(back.Nodes) != 2 || back.NextAction.ToolName != "workflow.next" {
		t.Fatalf("round trip lost data: %+v", back)
	}

	// A NEWER schema version than this build understands must refuse to decode.
	rec2 := rec
	rec2.SnapshotJson = strings.Replace(rec.SnapshotJson,
		`"schemaVersion":1`, `"schemaVersion":99`, 1)
	if _, err := DecodeSnapshot(&rec2); err == nil {
		t.Fatal("newer snapshot schema must refuse to decode")
	}

	// An id drift between row and snapshot is corruption.
	rec3 := rec
	rec3.ID = "wfg_other001"
	if _, err := DecodeSnapshot(&rec3); err == nil {
		t.Fatal("row/snapshot id mismatch must refuse to decode")
	}
}
