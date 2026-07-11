package workflowgraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

/* --------------------------------- fakes ---------------------------------- */

// memGraphStore is a minimal in-memory Store (mirrors the SQLite semantics the
// service depends on: revision guard, natural-key link upsert, newest-first
// event reads).
type memGraphStore struct {
	graphs     map[string]domain.WorkflowGraphRecord
	events     []domain.WorkflowGraphEventRecord
	links      map[string]domain.WorkflowResourceLinkRecord // key wf|type|ref
	reconciles map[string]domain.WorkflowReconcileRunRecord
	// failNextUpdate forces one revision conflict (concurrent-writer simulation).
	failNextUpdate bool
}

func newMemGraphStore() *memGraphStore {
	return &memGraphStore{
		graphs:     map[string]domain.WorkflowGraphRecord{},
		links:      map[string]domain.WorkflowResourceLinkRecord{},
		reconciles: map[string]domain.WorkflowReconcileRunRecord{},
	}
}

func (m *memGraphStore) InsertWorkflowGraph(rec domain.WorkflowGraphRecord) (domain.WorkflowGraphRecord, error) {
	rec.Revision = 1
	m.graphs[rec.ID] = rec
	return rec, nil
}

func (m *memGraphStore) GetWorkflowGraph(id string) (*domain.WorkflowGraphRecord, error) {
	rec, ok := m.graphs[id]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

func (m *memGraphStore) ListWorkflowGraphs(statuses []string, limit int) ([]domain.WorkflowGraphRecord, error) {
	want := map[string]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	var out []domain.WorkflowGraphRecord
	for _, rec := range m.graphs {
		if len(statuses) == 0 || want[rec.Status] {
			out = append(out, rec)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (m *memGraphStore) UpdateWorkflowGraphSnapshot(id string, expectedRevision int64, rec domain.WorkflowGraphRecord) (int64, error) {
	cur, ok := m.graphs[id]
	if !ok || cur.Revision != expectedRevision || m.failNextUpdate {
		m.failNextUpdate = false
		return 0, fmt.Errorf("update %s: %w", id, domain.ErrWorkflowGraphRevisionConflict)
	}
	rec.ID = id
	rec.Revision = expectedRevision + 1
	m.graphs[id] = rec
	return rec.Revision, nil
}

func (m *memGraphStore) InsertWorkflowGraphEvent(rec domain.WorkflowGraphEventRecord) (domain.WorkflowGraphEventRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflowGraphEvent)
	}
	m.events = append(m.events, rec)
	return rec, nil
}

func (m *memGraphStore) ListWorkflowGraphEvents(workflowID string, limit int) ([]domain.WorkflowGraphEventRecord, error) {
	var out []domain.WorkflowGraphEventRecord
	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if m.events[i].WorkflowID == workflowID {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}

func linkKey(rec domain.WorkflowResourceLinkRecord) string {
	return rec.WorkflowID + "|" + rec.ResourceType + "|" + rec.ResourceRef
}

func (m *memGraphStore) UpsertWorkflowResourceLink(rec domain.WorkflowResourceLinkRecord) error {
	m.links[linkKey(rec)] = rec
	return nil
}

func (m *memGraphStore) ListWorkflowResourceLinks(workflowID string) ([]domain.WorkflowResourceLinkRecord, error) {
	var out []domain.WorkflowResourceLinkRecord
	for _, l := range m.links {
		if l.WorkflowID == workflowID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m *memGraphStore) FindWorkflowResourceLinks(resourceType, resourceRef string) ([]domain.WorkflowResourceLinkRecord, error) {
	var out []domain.WorkflowResourceLinkRecord
	for _, l := range m.links {
		if l.ResourceType == resourceType && l.ResourceRef == resourceRef {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m *memGraphStore) InsertWorkflowReconcileRun(rec domain.WorkflowReconcileRunRecord) (domain.WorkflowReconcileRunRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixReconcileRun)
	}
	m.reconciles[rec.ID] = rec
	return rec, nil
}

func (m *memGraphStore) UpdateWorkflowReconcileRun(id string, patch map[string]any) error {
	rec, ok := m.reconciles[id]
	if !ok {
		return fmt.Errorf("no reconcile run %s", id)
	}
	if v, ok := patch["status"].(string); ok {
		rec.Status = v
	}
	if v, ok := patch["appliedRevision"].(int64); ok {
		rec.AppliedRevision = &v
	}
	if v, ok := patch["warning"].(string); ok {
		rec.Warning = &v
	}
	m.reconciles[id] = rec
	return nil
}

// fakeTasks is a canned backend.TaskRunner.
type fakeTasks struct {
	outputs  map[string]string // task id → output JSON
	err      error
	lastTask string
	inputs   []map[string]any
}

func (f *fakeTasks) RunTask(_ context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	f.lastTask = req.Task
	f.inputs = append(f.inputs, req.Input)
	if f.err != nil {
		return backend.TaskResult{}, f.err
	}
	out := f.outputs[req.Task]
	if out == "" {
		out = "{}"
	}
	return backend.TaskResult{Task: req.Task, Output: json.RawMessage(out)}, nil
}

func newTestService(t *testing.T, store *memGraphStore, tasks backend.TaskRunner) *Service {
	t.Helper()
	registered := map[string]bool{
		"agentTask.spawnForEdits": true, "terminal.await.async": true,
		"workflow.reconcile": true, "user.askMultipleChoice": true,
	}
	now := int64(1000)
	svc, err := NewService(Deps{
		Store:   store,
		Tasks:   tasks,
		HasTool: func(name string) bool { return registered[name] },
		ToolInventory: func() []backend.WorkflowToolInfo {
			return []backend.WorkflowToolInfo{{Name: "agentTask.spawnForEdits", Risk: "project"}}
		},
		RuntimeSummary: func() map[string]any { return map[string]any{"project_id": "prj"} },
		Now:            func() int64 { now++; return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// seedGraph plans nothing — it stores a fixture graph directly.
func seedGraph(t *testing.T, svc *Service, g *Graph) {
	t.Helper()
	rec, err := EncodeSnapshot(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.deps.Store.InsertWorkflowGraph(rec); err != nil {
		t.Fatal(err)
	}
}

const validPlanJSON = `{
  "goal": "Fix the watcher tests",
  "status": "active",
  "success_criteria": ["watcher tests pass"],
  "nodes": [
    {"id": "n_orient", "title": "Orient on repo", "kind": "orient"},
    {"id": "n_repair", "title": "Spawn repair agent", "kind": "delegate", "depends_on": ["n_orient"], "tool_name": "agentTask.spawnForEdits"},
    {"id": "n_wait", "title": "Wait for agent", "kind": "wait", "depends_on": ["n_repair"], "async_policy": "async_required"},
    {"id": "n_verify", "title": "Verify tests", "kind": "verify", "depends_on": ["n_wait"]}
  ],
  "next_action": {"label": "Orient on repo state", "tool_name": "agentTask.spawnForEdits"}
}`

/* ---------------------------------- plan ---------------------------------- */

func TestServicePlanStoresValidatedGraph(t *testing.T) {
	store := newMemGraphStore()
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowPlan: validPlanJSON}}
	svc := newTestService(t, store, tasks)

	res, err := svc.Plan(context.Background(), PlanRequest{Goal: "Fix the watcher tests"})
	if err != nil {
		t.Fatal(err)
	}
	if tasks.lastTask != backend.TaskWorkflowPlan {
		t.Fatalf("want workflow_plan, got %s", tasks.lastTask)
	}
	g := res.Graph
	if len(g.Nodes) != 4 || g.Status != StatusActive {
		t.Fatalf("unexpected graph: %d nodes, status %s", len(g.Nodes), g.Status)
	}
	// Root node with no deps auto-promotes pending → ready.
	if got := g.NodeByID("n_orient").Status; got != NodeReady {
		t.Fatalf("root node should be ready, got %s", got)
	}
	if len(g.ActiveNodeIDs) != 1 || g.ActiveNodeIDs[0] != "n_orient" {
		t.Fatalf("ActiveNodeIDs should be [n_orient], got %v", g.ActiveNodeIDs)
	}
	// Stored at revision 1 with a "planned" event.
	if rec := store.graphs[g.ID]; rec.Revision != 1 {
		t.Fatalf("stored revision should be 1, got %d", rec.Revision)
	}
	if len(store.events) != 1 || store.events[0].Kind != "planned" {
		t.Fatalf("want one 'planned' event, got %+v", store.events)
	}
	// The plan input carried the constraint + tool inventory + runtime summary.
	in := tasks.inputs[0]
	if !strings.Contains(fmt.Sprint(in["constraints"]), "never edit project files") {
		t.Fatalf("plan input must carry the no-file-edit constraint, got %v", in["constraints"])
	}
}

func TestServicePlanDropsUnknownNextActionTool(t *testing.T) {
	store := newMemGraphStore()
	plan := strings.ReplaceAll(validPlanJSON, `{"label": "Orient on repo state", "tool_name": "agentTask.spawnForEdits"}`,
		`{"label": "Orient on repo state", "tool_name": "made.up.tool"}`)
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowPlan: plan}}
	svc := newTestService(t, store, tasks)

	res, err := svc.Plan(context.Background(), PlanRequest{Goal: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Graph.NextAction != nil {
		t.Fatal("a next action naming an unknown tool must be dropped")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "made.up.tool") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropping the action must surface a warning, got %v", res.Warnings)
	}
}

func TestServicePlanRejectsCyclicPlan(t *testing.T) {
	store := newMemGraphStore()
	cyclic := `{"goal":"g","nodes":[
	  {"id":"a","title":"A","kind":"orient","depends_on":["b"]},
	  {"id":"b","title":"B","kind":"verify","depends_on":["a"]}]}`
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowPlan: cyclic}}
	svc := newTestService(t, store, tasks)

	if _, err := svc.Plan(context.Background(), PlanRequest{Goal: "g"}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic plan must be rejected, got %v", err)
	}
	if len(store.graphs) != 0 {
		t.Fatal("a rejected plan must store nothing")
	}
}

func TestServicePlanRefusesLiveExistingWithoutForce(t *testing.T) {
	store := newMemGraphStore()
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowPlan: validPlanJSON}}
	svc := newTestService(t, store, tasks)
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	_, err := svc.Plan(context.Background(), PlanRequest{Goal: "g", ExistingWorkflowID: g.ID})
	if err == nil || !strings.Contains(err.Error(), "workflow.reconcile") {
		t.Fatalf("planning over a live graph without forceReplan must be refused, got %v", err)
	}

	// With ForceReplan the old graph is cancelled as superseded.
	res, err := svc.Plan(context.Background(), PlanRequest{Goal: "g", ExistingWorkflowID: g.ID, ForceReplan: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.SupersededID != g.ID {
		t.Fatalf("want superseded id %s, got %q", g.ID, res.SupersededID)
	}
	old, _, err := svc.Get(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusCancelled {
		t.Fatalf("superseded graph should be cancelled, got %s", old.Status)
	}
}

func TestServicePlanOfflineFailsGracefully(t *testing.T) {
	svc := newTestService(t, newMemGraphStore(), nil)
	if _, err := svc.Plan(context.Background(), PlanRequest{Goal: "g"}); err == nil ||
		!strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("nil task runner must fail gracefully, got %v", err)
	}
}

/* -------------------------------- reconcile -------------------------------- */

const reconcilePatchJSON = `{
  "patch": {
    "node_updates": [{"node_id": "n_orient", "status": "done"}],
    "rationale": "orientation finished"
  },
  "next_action": {"label": "Verify the tests", "tool_name": "terminal.await.async"},
  "summary": "Orientation complete; verification is next."
}`

func TestServiceReconcileAppliesValidatedPatch(t *testing.T) {
	store := newMemGraphStore()
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowReconcile: reconcilePatchJSON}}
	svc := newTestService(t, store, tasks)
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	res, err := svc.Reconcile(context.Background(), ReconcileRequest{
		WorkflowID: g.ID, Reason: "tool_result", Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Revision != 2 {
		t.Fatalf("patch should apply at revision 2, got applied=%v rev=%d", res.Applied, res.Revision)
	}
	if got := res.Graph.NodeByID("n_orient").Status; got != NodeDone {
		t.Fatalf("n_orient should be done, got %s", got)
	}
	// Dependent auto-promoted; digest-visible next action installed.
	if got := res.Graph.NodeByID("n_verify").Status; got != NodeReady {
		t.Fatalf("n_verify should be ready, got %s", got)
	}
	if res.Graph.NextAction == nil || res.Graph.NextAction.ToolName != "terminal.await.async" {
		t.Fatalf("next action should be installed, got %+v", res.Graph.NextAction)
	}
	// The reconcile forensic row ended "applied".
	var run domain.WorkflowReconcileRunRecord
	for _, r := range store.reconciles {
		run = r
	}
	if run.Status != "applied" || run.AppliedRevision == nil || *run.AppliedRevision != 2 {
		t.Fatalf("reconcile run should be applied@2, got %+v", run)
	}
}

func TestServiceReconcileRejectsCorruptPatch(t *testing.T) {
	store := newMemGraphStore()
	// done → running on a done node = illegal transition.
	bad := `{"patch": {"node_updates": [{"node_id": "n_orient", "status": "banana"}]}}`
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowReconcile: bad}}
	svc := newTestService(t, store, tasks)
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	_, err := svc.Reconcile(context.Background(), ReconcileRequest{WorkflowID: g.ID, Reason: "manual", Apply: true})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("corrupt patch must be rejected, got %v", err)
	}
	// Nothing committed: still revision 1, node untouched.
	cur, rev, _ := svc.Get(g.ID)
	if rev != 1 || cur.NodeByID("n_orient").Status != NodeReady {
		t.Fatalf("rejected patch must not mutate the graph (rev=%d)", rev)
	}
	var run domain.WorkflowReconcileRunRecord
	for _, r := range store.reconciles {
		run = r
	}
	if run.Status != "rejected" {
		t.Fatalf("reconcile run should be rejected, got %+v", run)
	}
}

func TestServiceReconcilePreviewDoesNotCommit(t *testing.T) {
	store := newMemGraphStore()
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowReconcile: reconcilePatchJSON}}
	svc := newTestService(t, store, tasks)
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	res, err := svc.Reconcile(context.Background(), ReconcileRequest{WorkflowID: g.ID, Reason: "manual", Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("preview must not apply")
	}
	if res.Patch == nil || len(res.Patch.NodeUpdates) != 1 {
		t.Fatalf("preview should return the validated patch, got %+v", res.Patch)
	}
	if _, rev, _ := svc.Get(g.ID); rev != 1 {
		t.Fatalf("preview must not bump the revision, got %d", rev)
	}
}

func TestServiceReconcileInvalidReason(t *testing.T) {
	svc := newTestService(t, newMemGraphStore(), &fakeTasks{})
	if _, err := svc.Reconcile(context.Background(), ReconcileRequest{WorkflowID: "wfg_x", Reason: "vibes"}); err == nil {
		t.Fatal("invalid reason must be rejected")
	}
}

/* ------------------------------- local ops -------------------------------- */

func TestServiceAttachEvidenceCancelFlow(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil) // local ops need no backend
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	// Attach a terminal to a node → snapshot resource + reverse-index row.
	if _, _, err := svc.AttachResource(g.ID, "n_orient", Resource{Type: "terminal", Ref: "terminal-abc", Label: "Claude"}); err != nil {
		t.Fatal(err)
	}
	links, _ := store.FindWorkflowResourceLinks("terminal", "terminal-abc")
	if len(links) != 1 || links[0].NodeID == nil || *links[0].NodeID != "n_orient" {
		t.Fatalf("reverse-index link should exist for the node, got %+v", links)
	}
	// Attaching to an unknown node fails.
	if _, _, err := svc.AttachResource(g.ID, "n_ghost", Resource{Type: "terminal", Ref: "t2"}); err == nil {
		t.Fatal("attach to unknown node must fail")
	}

	// Evidence lands in the snapshot + event log.
	if _, _, err := svc.RecordEvidence(g.ID, "n_orient", EvidenceRef{Kind: "manual_note", Summary: "observed X"}); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := svc.Get(g.ID)
	if len(cur.Evidence) != 1 {
		t.Fatalf("evidence should be recorded, got %d", len(cur.Evidence))
	}

	// Node cancel, then graph cancel.
	if _, _, err := svc.Cancel(g.ID, "n_verify", "not needed"); err != nil {
		t.Fatal(err)
	}
	cur, _, _ = svc.Get(g.ID)
	if got := cur.NodeByID("n_verify").Status; got != NodeCancelled {
		t.Fatalf("node should be cancelled, got %s", got)
	}
	if _, _, err := svc.Cancel(g.ID, "", "done with this"); err != nil {
		t.Fatal(err)
	}
	cur, _, _ = svc.Get(g.ID)
	if cur.Status != StatusCancelled {
		t.Fatalf("graph should be cancelled, got %s", cur.Status)
	}
	// Cancelling again fails cleanly.
	if _, _, err := svc.Cancel(g.ID, "", "again"); err == nil {
		t.Fatal("double cancel must fail")
	}
}

func TestServiceMutateRetriesOnRevisionConflict(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := twoNodeGraph()
	seedGraph(t, svc, g)

	// One forced conflict: the mutate loop must reload and succeed on retry.
	store.failNextUpdate = true
	if _, rev, err := svc.RecordEvidence(g.ID, "", EvidenceRef{Kind: "manual_note", Summary: "retry me"}); err != nil {
		t.Fatalf("mutate should retry through one conflict, got %v", err)
	} else if rev != 2 {
		t.Fatalf("want revision 2 after retry, got %d", rev)
	}
}

/* --------------------------------- digests -------------------------------- */

func TestServiceWorkflowDigestsBoundedAndBestEffort(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)

	// 10 large graphs, all open → capped to the wire max with clamped fields.
	longGoal := strings.Repeat("investigate the flaky integration suite ", 40)
	for i := 0; i < 10; i++ {
		g := twoNodeGraph()
		g.ID = fmt.Sprintf("wfg_%08d", i)
		g.Goal = longGoal
		seedGraph(t, svc, g)
	}
	digests := svc.WorkflowDigests(100)
	if len(digests) == 0 || len(digests) > backend.MaxWorkflowDigests {
		t.Fatalf("digests must be 1..%d, got %d", backend.MaxWorkflowDigests, len(digests))
	}
	for _, d := range digests {
		if len([]rune(d.Goal)) > 512 {
			t.Fatalf("digest goal must be clamped to 512 runes, got %d", len([]rune(d.Goal)))
		}
	}
	if b, _ := json.Marshal(digests); len(b) > backend.MaxWorkflowStateBytes {
		t.Fatalf("serialized digests exceed %d bytes: %d", backend.MaxWorkflowStateBytes, len(b))
	}
}

func TestServiceWorkflowDigestsOmitsTerminalGraphs(t *testing.T) {
	store := newMemGraphStore()
	svc := newTestService(t, store, nil)
	g := twoNodeGraph()
	g.Status = StatusDone
	g.CriteriaWaived = "test fixture"
	seedGraph(t, svc, g)
	if digests := svc.WorkflowDigests(5); len(digests) != 0 {
		t.Fatalf("terminal graphs must not produce digests, got %d", len(digests))
	}
}

/* ------------------------------ resume digest ------------------------------ */

func TestServiceResumeDigest(t *testing.T) {
	store := newMemGraphStore()
	out := `{"ranked": [{"workflow_id": "wfg_test0001", "rank": 1, "headline": "fix the watcher tests",
	  "status": "active", "summary": "agent finished; verify next", "blockers": [],
	  "recommended_action": {"label": "run the tests", "tool_name": "terminal.run"}}],
	  "suggested_focus_workflow_id": "wfg_test0001"}`
	tasks := &fakeTasks{outputs: map[string]string{backend.TaskWorkflowResumeDigest: out}}
	svc := newTestService(t, store, tasks)
	seedGraph(t, svc, twoNodeGraph())

	res, err := svc.ResumeDigest(context.Background(), "where were we?")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ranked) != 1 || res.SuggestedFocusWorkflowID != "wfg_test0001" {
		t.Fatalf("unexpected resume package: %+v", res)
	}
	// No active graphs ⇒ no backend call, empty package.
	empty := newTestService(t, newMemGraphStore(), tasks)
	res, err = empty.ResumeDigest(context.Background(), "hi")
	if err != nil || len(res.Ranked) != 0 {
		t.Fatalf("no graphs should yield an empty package without error, got %+v %v", res, err)
	}
}

/* ------------------------ conflict sentinel plumbing ----------------------- */

func TestRevisionConflictSentinelDetectable(t *testing.T) {
	store := newMemGraphStore()
	_, err := store.UpdateWorkflowGraphSnapshot("wfg_missing", 1, domain.WorkflowGraphRecord{})
	if !errors.Is(err, domain.ErrWorkflowGraphRevisionConflict) {
		t.Fatalf("conflict must be detectable with errors.Is, got %v", err)
	}
}
