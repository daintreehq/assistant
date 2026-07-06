package storage

import (
	"errors"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func openGraphStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func graphRec(id string) domain.WorkflowGraphRecord {
	return domain.WorkflowGraphRecord{
		ID:            id,
		Status:        "active",
		Goal:          "fix the watcher tests",
		SchemaVersion: 1,
		SnapshotJson:  `{"id":"` + id + `","goal":"fix the watcher tests","status":"active","schemaVersion":1,"nodes":[]}`,
	}
}

func TestWorkflowGraphInsertGetRoundTrip(t *testing.T) {
	s := openGraphStore(t)
	rec, err := s.InsertWorkflowGraph(graphRec("wfg_00000001"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revision != 1 || rec.CreatedAt == 0 || rec.UpdatedAt == 0 {
		t.Fatalf("insert must set revision 1 + timestamps, got %+v", rec)
	}
	got, err := s.GetWorkflowGraph("wfg_00000001")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.SnapshotJson != rec.SnapshotJson || got.Revision != 1 || got.Goal != rec.Goal {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if missing, err := s.GetWorkflowGraph("wfg_missing0"); err != nil || missing != nil {
		t.Fatalf("absent id must be (nil, nil), got %v %v", missing, err)
	}
}

func TestWorkflowGraphOptimisticRevision(t *testing.T) {
	s := openGraphStore(t)
	rec, err := s.InsertWorkflowGraph(graphRec("wfg_00000002"))
	if err != nil {
		t.Fatal(err)
	}

	rec.Status = "blocked"
	newRev, err := s.UpdateWorkflowGraphSnapshot(rec.ID, 1, rec)
	if err != nil || newRev != 2 {
		t.Fatalf("update at the read revision should land at 2, got %d %v", newRev, err)
	}

	// A STALE writer (still naming revision 1) must be refused with the typed
	// sentinel — never a silent last-writer-wins.
	if _, err := s.UpdateWorkflowGraphSnapshot(rec.ID, 1, rec); !errors.Is(err, domain.ErrWorkflowGraphRevisionConflict) {
		t.Fatalf("stale revision must conflict, got %v", err)
	}
	// A vanished row is the same conflict class.
	if _, err := s.UpdateWorkflowGraphSnapshot("wfg_gone0000", 1, rec); !errors.Is(err, domain.ErrWorkflowGraphRevisionConflict) {
		t.Fatalf("missing row must conflict, got %v", err)
	}

	got, _ := s.GetWorkflowGraph(rec.ID)
	if got.Revision != 2 || got.Status != "blocked" {
		t.Fatalf("committed state should be rev 2 blocked, got %+v", got)
	}
}

func TestWorkflowGraphListFiltersAndBounds(t *testing.T) {
	s := openGraphStore(t)
	for _, st := range []struct{ id, status string }{
		{"wfg_a0000001", "active"}, {"wfg_a0000002", "blocked"},
		{"wfg_a0000003", "done"}, {"wfg_a0000004", "planned"},
	} {
		rec := graphRec(st.id)
		rec.Status = st.status
		if _, err := s.InsertWorkflowGraph(rec); err != nil {
			t.Fatal(err)
		}
	}
	open, err := s.ListWorkflowGraphs(OpenWorkflowGraphStatuses, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 3 {
		t.Fatalf("want 3 open graphs, got %d", len(open))
	}
	all, _ := s.ListWorkflowGraphs(nil, 10)
	if len(all) != 4 {
		t.Fatalf("nil filter should list all, got %d", len(all))
	}
	if capped, _ := s.ListWorkflowGraphs(nil, 2); len(capped) != 2 {
		t.Fatalf("limit must bound the read, got %d", len(capped))
	}
	if none, _ := s.ListWorkflowGraphs(nil, 0); none != nil {
		t.Fatal("non-positive limit returns nil")
	}
}

func TestWorkflowGraphEventsNewestFirst(t *testing.T) {
	s := openGraphStore(t)
	for i, kind := range []string{"planned", "evidence", "reconciled"} {
		if _, err := s.InsertWorkflowGraphEvent(domain.WorkflowGraphEventRecord{
			WorkflowID: "wfg_00000003", Revision: int64(i + 1), Kind: kind,
			Summary: kind + " happened", PayloadHash: "h", CreatedAt: int64(100 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.ListWorkflowGraphEvents("wfg_00000003", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != "reconciled" || evs[1].Kind != "evidence" {
		t.Fatalf("want newest-first capped at 2, got %+v", evs)
	}
	if evs[0].PayloadJson != "{}" {
		t.Fatalf("empty payload should normalize to {}, got %q", evs[0].PayloadJson)
	}
}

func TestWorkflowResourceLinkUpsertAndReverseLookup(t *testing.T) {
	s := openGraphStore(t)
	nodeID := "n_repair"
	link := domain.WorkflowResourceLinkRecord{
		WorkflowID: "wfg_00000004", ResourceType: "async", ResourceRef: "asy_11112222",
		NodeID: &nodeID, Label: ptrStr("wait for agent"),
	}
	if err := s.UpsertWorkflowResourceLink(link); err != nil {
		t.Fatal(err)
	}
	// Re-linking the SAME (workflow, type, ref) updates in place, keeping createdAt.
	first, _ := s.FindWorkflowResourceLinks("async", "asy_11112222")
	link.Status = ptrStr("succeeded")
	if err := s.UpsertWorkflowResourceLink(link); err != nil {
		t.Fatal(err)
	}
	links, err := s.FindWorkflowResourceLinks("async", "asy_11112222")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 {
		t.Fatalf("upsert must not duplicate, got %d rows", len(links))
	}
	got := links[0]
	if got.Status == nil || *got.Status != "succeeded" || got.NodeID == nil || *got.NodeID != "n_repair" {
		t.Fatalf("upsert should refresh fields, got %+v", got)
	}
	if got.CreatedAt != first[0].CreatedAt {
		t.Fatal("upsert must keep the original createdAt")
	}
	byWF, _ := s.ListWorkflowResourceLinks("wfg_00000004")
	if len(byWF) != 1 {
		t.Fatalf("per-workflow list should see the link, got %d", len(byWF))
	}
}

func TestWorkflowReconcileRunLifecycle(t *testing.T) {
	s := openGraphStore(t)
	run, err := s.InsertWorkflowReconcileRun(domain.WorkflowReconcileRunRecord{
		WorkflowID: "wfg_00000005", BaseRevision: 3, Status: "started", InputHash: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkflowReconcileRun(run.ID, map[string]any{
		"status": "applied", "appliedRevision": int64(4), "outputHash": "def456",
	}); err != nil {
		t.Fatal(err)
	}
	// The disallowed column is silently dropped by the allowlist (never written).
	if err := s.UpdateWorkflowReconcileRun(run.ID, map[string]any{"workflowId": "wfg_evil"}); err != nil {
		t.Fatal(err)
	}
	var status, wfID string
	var appliedRev int64
	if err := s.DB().QueryRow(
		"SELECT status, workflowId, appliedRevision FROM workflow_reconcile_runs WHERE id = ?", run.ID).
		Scan(&status, &wfID, &appliedRev); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || appliedRev != 4 || wfID != "wfg_00000005" {
		t.Fatalf("unexpected row: %s %s %d", status, wfID, appliedRev)
	}
}
