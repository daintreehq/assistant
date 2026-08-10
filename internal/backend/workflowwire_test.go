package backend_test

// Golden wire-conformance tests for the three workflow-intelligence tasks.
// The fixtures under testdata/workflow/ are byte-identical copies of the
// backend repo's tests/wire/fixtures/workflow/ (generated from, and pinned
// against, the canonical pydantic models in contracts/tasks.py). Decoding
// them with the REAL DTOs — through the real typed helpers, so the semantic
// decode validation runs too — proves the Go wire layer matches the backend
// exactly. Per the fixture README, a missing key is a contract failure, so
// every drift-prone field is asserted to survive non-zero exactly as the
// fixture states.
//
// This file is an external test package so it can also drive the
// workflowgraph → backend.WorkflowSnapshot serialization against
// reconcile_input.json (workflowgraph imports backend; the internal test
// package could not import it back).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

// fixtureRunner returns one canned task output for any task.
type fixtureRunner struct{ out json.RawMessage }

func (f fixtureRunner) RunTask(_ context.Context, _ backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{Output: f.out}, nil
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "workflow", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// deref unwraps a nullable wire string for assertions (nil → "").
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// An explicit EMPTY string on a nullable patch field must decode as a non-nil
// pointer to "" — the wire-level carrier of clear semantics (last_error:""
// clears a previous error) — while null/omitted decodes to nil.
func TestNodePatchNullVsEmptyDistinguished(t *testing.T) {
	out, err := backend.RunWorkflowReconcile(context.Background(),
		fixtureRunner{out: json.RawMessage(
			`{"patch":{"node_updates":[{"node_id":"n1","last_error":"","note":null}]}}`)},
		backend.WorkflowReconcileInput{Workflow: &backend.WorkflowSnapshot{ID: "w"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	nu := out.Patch.NodeUpdates[0]
	if nu.LastError == nil || *nu.LastError != "" {
		t.Fatalf("explicit last_error:\"\" must decode non-nil empty (clear), got %v", nu.LastError)
	}
	if nu.Note != nil || nu.Status != nil || nu.Title != nil {
		t.Fatalf("null/omitted fields must decode nil, got %+v", nu)
	}
}

/* ------------------------------ plan output ------------------------------- */

func TestPlanOutputFixtureDecodesWithRealDTOs(t *testing.T) {
	out, err := backend.RunWorkflowPlan(context.Background(),
		fixtureRunner{out: readFixture(t, "plan_output.json")}, backend.WorkflowPlanInput{Goal: "g"})
	if err != nil {
		t.Fatalf("fixture plan output must decode and validate: %v", err)
	}

	if out.Goal != "Get the failing watcher unit tests green and report the fix" {
		t.Errorf("goal: %q", out.Goal)
	}
	if out.Status != "active" || out.Confidence != 0.85 {
		t.Errorf("status/confidence: %q %v", out.Status, out.Confidence)
	}
	if len(out.SuccessCriteria) != 2 || len(out.Assumptions) != 1 || len(out.Warnings) != 1 {
		t.Errorf("criteria/assumptions/warnings: %d/%d/%d",
			len(out.SuccessCriteria), len(out.Assumptions), len(out.Warnings))
	}
	if len(out.Nodes) != 5 {
		t.Fatalf("nodes: want 5, got %d", len(out.Nodes))
	}
	n0 := out.Nodes[0]
	if n0.ID != "orient-repo" || n0.Kind != "orient" || n0.Status != "done" ||
		n0.ToolName != "context.snapshot" || n0.Risk != "read" || n0.AsyncPolicy != "blocking" ||
		n0.Owner != "assistant" || len(n0.ExpectedEvidence) != 1 {
		t.Errorf("node[0] drifted: %+v", n0)
	}
	if got := out.Nodes[1].DependsOn; len(got) != 1 || got[0] != "orient-repo" {
		t.Errorf("node[1].depends_on drifted (dependsOn spelling?): %v", got)
	}
	if !out.Nodes[3].RequiresConfirm {
		t.Error("node[3].requires_confirm must decode true")
	}
	// Explicit nulls (tool_name/risk on the report node) must decode to zero values.
	if out.Nodes[4].ToolName != "" || out.Nodes[4].Risk != "" {
		t.Errorf("node[4] nulls: %+v", out.Nodes[4])
	}

	// Edges are source/target — the historical from/to drift point.
	if len(out.Edges) != 4 {
		t.Fatalf("edges: want 4, got %d", len(out.Edges))
	}
	if out.Edges[0].Source != "orient-repo" || out.Edges[0].Target != "fix-watcher-tests" {
		t.Errorf("edge[0] source/target drifted: %+v", out.Edges[0])
	}

	if out.NextAction == nil || out.NextAction.NodeID != "await-agent" ||
		out.NextAction.ToolName != "terminal.await.async" ||
		out.NextAction.Label != "Wait for the editing agent to finish" {
		t.Errorf("next_action drifted: %+v", out.NextAction)
	}
	if out.UserQuestion == nil || len(out.UserQuestion.Options) != 2 || !out.UserQuestion.AllowOther {
		t.Errorf("user_question drifted: %+v", out.UserQuestion)
	}
}

/* ---------------------------- reconcile output ---------------------------- */

func TestReconcileOutputFixtureDecodesWithRealDTOs(t *testing.T) {
	out, err := backend.RunWorkflowReconcile(context.Background(),
		fixtureRunner{out: readFixture(t, "reconcile_output.json")},
		backend.WorkflowReconcileInput{Workflow: &backend.WorkflowSnapshot{ID: "w"}, Reason: "manual"})
	if err != nil {
		t.Fatalf("fixture reconcile output must decode and validate: %v", err)
	}
	p := out.Patch

	if p.BaseRevision == nil || *p.BaseRevision != 12 {
		t.Errorf("base_revision must decode 12, got %v", p.BaseRevision)
	}
	if p.NewStatus != "active" {
		t.Errorf("new_status: %q", p.NewStatus)
	}

	if len(p.NodeUpdates) != 2 {
		t.Fatalf("node_updates: want 2, got %d", len(p.NodeUpdates))
	}
	// The nullable fields are POINTERS (backend NodePatchOut: `... | None`), so an
	// explicit null must decode to nil — NOT to "" — keeping "leave unchanged"
	// distinguishable from an explicit empty value (clear semantics).
	if p.NodeUpdates[0].NodeID != "verify-tests" || deref(p.NodeUpdates[0].Status) != "failed" ||
		deref(p.NodeUpdates[0].LastError) != "pytest exited 1: 2 watcher tests still failing" ||
		p.NodeUpdates[0].Title != nil || p.NodeUpdates[0].Note != nil {
		t.Errorf("node_updates[0] drifted (node_id spelling / null-vs-empty?): %+v", p.NodeUpdates[0])
	}
	if p.NodeUpdates[1].NodeID != "report-outcome" || deref(p.NodeUpdates[1].Note) == "" ||
		deref(p.NodeUpdates[1].Title) == "" || p.NodeUpdates[1].Status != nil ||
		p.NodeUpdates[1].LastError != nil {
		t.Errorf("node_updates[1] drifted: %+v", p.NodeUpdates[1])
	}

	if len(p.AddNodes) != 1 || p.AddNodes[0].ID != "diagnose-failures" ||
		len(p.AddNodes[0].DependsOn) != 1 || p.AddNodes[0].DependsOn[0] != "verify-tests" {
		t.Errorf("add_nodes drifted: %+v", p.AddNodes)
	}
	if len(p.AddEdges) != 1 || p.AddEdges[0].Source != "diagnose-failures" ||
		p.AddEdges[0].Target != "report-outcome" {
		t.Errorf("add_edges source/target drifted: %+v", p.AddEdges)
	}

	if len(p.ResourceUpdates) != 1 {
		t.Fatalf("resource_updates: want 1, got %d", len(p.ResourceUpdates))
	}
	ru := p.ResourceUpdates[0]
	if ru.ResourceID != "term-verify-1" || deref(ru.Status) != "exited" ||
		deref(ru.NodeID) != "verify-tests" || deref(ru.Label) != "pytest verify run (exit 1)" {
		t.Errorf("resource_updates[0] drifted (resource_id spelling?): %+v", ru)
	}

	if len(p.AddBlockers) != 2 {
		t.Fatalf("add_blockers: want 2, got %d", len(p.AddBlockers))
	}
	b0 := p.AddBlockers[0]
	if b0.ID != "blk-residual-failures" || b0.NodeID != "verify-tests" || b0.Kind != "task" ||
		b0.Summary != "Two watcher tests still fail after the agent's edits" {
		t.Errorf("add_blockers[0] drifted (summary/kind spelling?): %+v", b0)
	}
	// Second blocker carries explicit nulls for id/node_id and kind "other".
	if p.AddBlockers[1].ID != "" || p.AddBlockers[1].NodeID != "" ||
		p.AddBlockers[1].Kind != "other" || p.AddBlockers[1].Summary == "" {
		t.Errorf("add_blockers[1] drifted: %+v", p.AddBlockers[1])
	}

	if len(p.ResolveBlockers) != 1 || p.ResolveBlockers[0] != "blk-flaky-network" {
		t.Errorf("resolve_blockers drifted: %v", p.ResolveBlockers)
	}
	if p.Rationale == "" {
		t.Error("rationale must survive")
	}

	if out.NextAction == nil || out.NextAction.NodeID != "diagnose-failures" ||
		out.NextAction.ToolName != "terminal.read" {
		t.Errorf("next_action drifted: %+v", out.NextAction)
	}
	if out.UserQuestion != nil {
		t.Errorf("user_question must decode explicit null as nil, got %+v", out.UserQuestion)
	}
	if out.Summary == "" || len(out.Warnings) != 1 || out.Confidence != 0.72 {
		t.Errorf("summary/warnings/confidence drifted: %q %v %v", out.Summary, out.Warnings, out.Confidence)
	}
}

/* ------------------------------ resume output ----------------------------- */

func TestResumeOutputFixtureDecodesWithRealDTOs(t *testing.T) {
	out, err := backend.RunWorkflowResumeDigest(context.Background(),
		fixtureRunner{out: readFixture(t, "resume_output.json")}, backend.WorkflowResumeDigestInput{})
	if err != nil {
		t.Fatalf("fixture resume output must decode and validate: %v", err)
	}
	if len(out.Ranked) != 2 {
		t.Fatalf("ranked: want 2, got %d", len(out.Ranked))
	}

	r0 := out.Ranked[0]
	if r0.WorkflowID != "wf-fix-watcher-tests" || r0.Rank != 1 || r0.Status != "active" ||
		r0.Headline != "Watcher-test fix is one diagnosis step from a report" || r0.Summary == "" {
		t.Errorf("ranked[0] drifted (workflow_id/headline spelling?): %+v", r0)
	}
	if len(r0.Blockers) != 1 || r0.Blockers[0] != "Two watcher tests still fail after the agent's edits" {
		t.Errorf("ranked[0].blockers drifted: %v", r0.Blockers)
	}
	ra := r0.RecommendedAction
	if ra == nil || ra.NodeID != "diagnose-failures" || ra.ToolName != "terminal.read" ||
		ra.Label == "" || ra.RequiresConfirmation || ra.Risk != "read" {
		t.Errorf("ranked[0].recommended_action drifted: %+v", ra)
	}
	if ra != nil {
		if lines, ok := ra.Args["lines"].(float64); !ok || lines != 200 {
			t.Errorf("recommended_action.args drifted: %v", ra.Args)
		}
	}

	r1 := out.Ranked[1]
	if r1.WorkflowID != "wf-changelog-draft" || r1.Rank != 2 || r1.Status != "" ||
		r1.Summary != "" || len(r1.Blockers) != 0 || r1.RecommendedAction != nil {
		t.Errorf("ranked[1] drifted (empty/null handling): %+v", r1)
	}

	if out.SuggestedFocusWorkflowID != "wf-fix-watcher-tests" {
		t.Errorf("suggested_focus_workflow_id: %q", out.SuggestedFocusWorkflowID)
	}
	if len(out.AssistantContextLines) != 2 {
		t.Errorf("assistant_context_lines: %v", out.AssistantContextLines)
	}
}

/* --------------------------- reconcile input side -------------------------- */

// The canonical reconcile INPUT: build the equivalent local Graph, serialize
// it through the real snapshot DTO (workflowgraph.SnapshotFromGraph → the
// WorkflowReconcileInput envelope), and require semantic equality with the
// fixture, key by key. This is what keeps the backend's cycle detection and
// terminal-status guard fed — drifted keys are silently NOT read over there.
func TestReconcileInputSerializationMatchesFixture(t *testing.T) {
	g := &workflowgraph.Graph{
		ID:     "wf-fix-watcher-tests",
		Goal:   "Get the failing watcher unit tests green and report the fix",
		Status: workflowgraph.StatusActive,
		Nodes: []workflowgraph.Node{
			{ID: "orient-repo", Title: "Snapshot the repository and failing-test state",
				Kind: workflowgraph.KindOrient, Status: workflowgraph.NodeDone},
			{ID: "fix-watcher-tests", Title: "Delegate the watcher test fix to an editing agent",
				Kind: workflowgraph.KindDelegate, Status: workflowgraph.NodeDone,
				DependsOn: []string{"orient-repo"}},
			{ID: "await-agent", Title: "Wait for the editing agent to finish",
				Kind: workflowgraph.KindWait, Status: workflowgraph.NodeDone,
				DependsOn: []string{"fix-watcher-tests"}},
			{ID: "verify-tests", Title: "Re-run the watcher tests to verify the fix",
				Kind: workflowgraph.KindVerify, Status: workflowgraph.NodeRunning,
				DependsOn: []string{"await-agent"}, ToolName: "terminal.run"},
			{ID: "report-outcome", Title: "Report the fix and test results to the user",
				Kind: workflowgraph.KindReport, Status: workflowgraph.NodePending,
				DependsOn: []string{"verify-tests"}},
		},
		Edges: []workflowgraph.Edge{
			{From: "orient-repo", To: "fix-watcher-tests"},
			{From: "fix-watcher-tests", To: "await-agent"},
			{From: "await-agent", To: "verify-tests"},
			{From: "verify-tests", To: "report-outcome"},
		},
		Resources: []workflowgraph.Resource{
			{ID: "term-verify-1", Type: "terminal", Status: "running",
				NodeID: "verify-tests", Label: "pytest verify run"},
		},
		Blockers: []workflowgraph.Blocker{
			{ID: "blk-flaky-network", NodeID: "fix-watcher-tests", Kind: "environment",
				Reason: "PyPI timed out during the dependency install"},
		},
	}

	input := backend.WorkflowReconcileInput{
		Workflow: workflowgraph.SnapshotFromGraph(g, 12),
		RecentEvents: []map[string]any{
			{"type": "node_status", "node_id": "await-agent",
				"summary": "Editing agent finished; edits landed in tests/unit",
				"at":      "2026-07-11T09:12:41Z"},
			{"type": "tool_result", "node_id": "verify-tests", "tool_name": "terminal.run",
				"summary": "pytest exited 1: 2 watcher tests still failing",
				"at":      "2026-07-11T09:14:03Z"},
		},
		RuntimeSummary: map[string]any{"cwd": "/work/repo", "open_terminals": 1, "platform": "darwin"},
		QueueEvents: []map[string]any{
			{"id": "queue-1", "kind": "wake", "summary": "terminal term-verify-1 produced new output"},
		},
		LatestUserMessage: "The verify run finished - what happened?",
		Reason:            "tool_result",
		ToolInventory: []backend.WorkflowToolInfo{
			{Name: "context.snapshot", Risk: "read"},
			{Name: "terminal.run", Risk: "terminal"},
			{Name: "terminal.read", Risk: "read"},
			{Name: "terminal.await.async", Risk: "local"},
			{Name: "agentTask.spawnForEdits", Risk: "project"},
		},
	}

	gotJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(readFixture(t, "reconcile_input.json"), &want); err != nil {
		t.Fatal(err)
	}

	// Compare per top-level key first for a readable failure, then in full.
	for key, wantVal := range want {
		gotVal, ok := got[key]
		if !ok {
			t.Errorf("input key %q missing from serialized request", key)
			continue
		}
		if !reflect.DeepEqual(gotVal, wantVal) {
			gb, _ := json.MarshalIndent(gotVal, "", "  ")
			wb, _ := json.MarshalIndent(wantVal, "", "  ")
			t.Errorf("input key %q drifted:\n--- got ---\n%s\n--- want ---\n%s", key, gb, wb)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("serialized request carries key %q the canonical fixture does not", key)
		}
	}
}

/* -------------------------- decode-time validation ------------------------- */

// Zero-value identities must surface as a typed protocol error, never as
// silently-usable data.
func TestWorkflowOutputValidationRejectsZeroIdentities(t *testing.T) {
	cases := []struct {
		name string
		run  func(r backend.TaskRunner) error
		out  string
	}{
		{"plan empty node id", runPlan, `{"goal":"g","nodes":[{"id":"","title":"t","kind":"orient"}]}`},
		{"plan duplicate node id", runPlan, `{"goal":"g","nodes":[{"id":"a","title":"t","kind":"orient"},{"id":"a","title":"t2","kind":"verify"}]}`},
		{"plan edge unknown endpoint", runPlan, `{"goal":"g","nodes":[{"id":"a","title":"t","kind":"orient"}],"edges":[{"source":"a","target":"ghost"}]}`},
		{"plan edge empty endpoint", runPlan, `{"goal":"g","nodes":[{"id":"a","title":"t","kind":"orient"}],"edges":[{"source":"","target":"a"}]}`},
		{"patch empty node_id", runReconcile, `{"patch":{"node_updates":[{"status":"done"}]}}`},
		{"patch empty resource_id", runReconcile, `{"patch":{"resource_updates":[{"status":"exited"}]}}`},
		{"patch add_nodes empty id", runReconcile, `{"patch":{"add_nodes":[{"id":"","title":"t","kind":"orient"}]}}`},
		{"patch add_edges empty endpoint", runReconcile, `{"patch":{"add_edges":[{"source":"a","target":""}]}}`},
		{"resume empty workflow_id", runResume, `{"ranked":[{"workflow_id":"","rank":1,"headline":"h"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(fixtureRunner{out: json.RawMessage(tc.out)})
			var oe *backend.TaskOutputError
			if !errors.As(err, &oe) {
				t.Fatalf("want *TaskOutputError, got %v", err)
			}
		})
	}
}

func runPlan(r backend.TaskRunner) error {
	_, err := backend.RunWorkflowPlan(context.Background(), r, backend.WorkflowPlanInput{Goal: "g"})
	return err
}

func runReconcile(r backend.TaskRunner) error {
	_, err := backend.RunWorkflowReconcile(context.Background(), r, backend.WorkflowReconcileInput{
		Workflow: &backend.WorkflowSnapshot{ID: "w"}, Reason: "manual"})
	return err
}

func runResume(r backend.TaskRunner) error {
	_, err := backend.RunWorkflowResumeDigest(context.Background(), r, backend.WorkflowResumeDigestInput{})
	return err
}
