package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Workflow-intelligence graph persistence: durable JSON snapshots with an
// optimistic revision counter, an append-only projection-event log, the
// resource reverse index, and the reconcile forensic trail. The typed graph
// model + all validation live in internal/workflowgraph; this layer stores
// records and enforces exactly one invariant of its own — no snapshot write
// lands over a revision the writer didn't read.

// ErrWorkflowGraphRevisionConflict aliases the domain sentinel (kept here for
// call-site readability; callers match with errors.Is either way).
var ErrWorkflowGraphRevisionConflict = domain.ErrWorkflowGraphRevisionConflict

const workflowGraphCols = `id,status,goal,schemaVersion,revision,snapshotJson,createdAt,updatedAt,completedAt`

func scanWorkflowGraph(sc scanner) (domain.WorkflowGraphRecord, error) {
	var g domain.WorkflowGraphRecord
	var completedAt sql.NullInt64
	if err := sc.Scan(&g.ID, &g.Status, &g.Goal, &g.SchemaVersion, &g.Revision,
		&g.SnapshotJson, &g.CreatedAt, &g.UpdatedAt, &completedAt); err != nil {
		return domain.WorkflowGraphRecord{}, err
	}
	g.CompletedAt = i64FromNull(completedAt)
	return g, nil
}

// InsertWorkflowGraph persists a fresh graph snapshot at revision 1 (id and
// timestamps filled when zero). Returns the stored record.
func (s *Store) InsertWorkflowGraph(rec domain.WorkflowGraphRecord) (domain.WorkflowGraphRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflowGraph)
	}
	now := s.now()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	rec.Revision = 1
	_, err := s.db.Exec(`
		INSERT INTO workflow_graphs
		  (id,status,goal,schemaVersion,revision,snapshotJson,createdAt,updatedAt,completedAt)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Status, rec.Goal, rec.SchemaVersion, rec.Revision,
		rec.SnapshotJson, rec.CreatedAt, rec.UpdatedAt, nullI64(rec.CompletedAt))
	if err != nil {
		return domain.WorkflowGraphRecord{}, fmt.Errorf("insert workflow graph: %w", err)
	}
	return rec, nil
}

// GetWorkflowGraph returns a graph record by id, or (nil, nil) when absent.
func (s *Store) GetWorkflowGraph(id string) (*domain.WorkflowGraphRecord, error) {
	row := s.db.QueryRow("SELECT "+workflowGraphCols+" FROM workflow_graphs WHERE id = ?", id)
	g, err := scanWorkflowGraph(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow graph: %w", err)
	}
	return &g, nil
}

// ListWorkflowGraphs returns graphs filtered to the given statuses (nil/empty ⇒
// all), most-recently-updated first, capped at limit (non-positive ⇒ nil, the
// footer-read discipline of ListNonTerminalWorkflowRuns).
func (s *Store) ListWorkflowGraphs(statuses []string, limit int) ([]domain.WorkflowGraphRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	q := "SELECT " + workflowGraphCols + " FROM workflow_graphs"
	var args []any
	if len(statuses) > 0 {
		q += " WHERE status IN (?" + strings.Repeat(",?", len(statuses)-1) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	// id DESC tiebreak keeps LIMIT deterministic under same-ms updates.
	q += " ORDER BY updatedAt DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow graphs: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowGraphRecord
	for rows.Next() {
		g, err := scanWorkflowGraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// OpenWorkflowGraphStatuses is the non-terminal status filter for "active"
// graph reads (turn digests, dashboards, adoption).
var OpenWorkflowGraphStatuses = []string{"planned", "active", "blocked"}

// UpdateWorkflowGraphSnapshot replaces a graph's snapshot ATOMICALLY guarded by
// the revision the writer read: the row must still be at expectedRevision or
// the write is refused with ErrWorkflowGraphRevisionConflict (reload, recompute,
// retry). On success the revision becomes expectedRevision+1 (returned).
func (s *Store) UpdateWorkflowGraphSnapshot(id string, expectedRevision int64, rec domain.WorkflowGraphRecord) (int64, error) {
	res, err := s.db.Exec(`
		UPDATE workflow_graphs
		SET status = ?, goal = ?, schemaVersion = ?, revision = revision + 1,
		    snapshotJson = ?, updatedAt = ?, completedAt = ?
		WHERE id = ? AND revision = ?`,
		rec.Status, rec.Goal, rec.SchemaVersion, rec.SnapshotJson, s.now(),
		nullI64(rec.CompletedAt), id, expectedRevision)
	if err != nil {
		return 0, fmt.Errorf("update workflow graph: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Distinguish a moved revision from a vanished row for the error text,
		// but both are the same conflict class to the caller.
		cur, gerr := s.GetWorkflowGraph(id)
		if gerr == nil && cur == nil {
			return 0, fmt.Errorf("update workflow graph %s: %w (row gone)", id, ErrWorkflowGraphRevisionConflict)
		}
		return 0, fmt.Errorf("update workflow graph %s at revision %d: %w", id, expectedRevision, ErrWorkflowGraphRevisionConflict)
	}
	return expectedRevision + 1, nil
}

/* ----------------------------- workflow_events ---------------------------- */

const workflowEventCols = `id,workflowId,revision,kind,nodeId,summary,payloadJson,payloadHash,createdAt`

func scanWorkflowEvent(sc scanner) (domain.WorkflowGraphEventRecord, error) {
	var e domain.WorkflowGraphEventRecord
	var nodeID sql.NullString
	if err := sc.Scan(&e.ID, &e.WorkflowID, &e.Revision, &e.Kind, &nodeID,
		&e.Summary, &e.PayloadJson, &e.PayloadHash, &e.CreatedAt); err != nil {
		return domain.WorkflowGraphEventRecord{}, err
	}
	e.NodeID = strFromNull(nodeID)
	return e, nil
}

// InsertWorkflowGraphEvent appends one projection event (id/createdAt filled
// when zero; empty payload normalized to "{}").
func (s *Store) InsertWorkflowGraphEvent(rec domain.WorkflowGraphEventRecord) (domain.WorkflowGraphEventRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflowGraphEvent)
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	if rec.PayloadJson == "" {
		rec.PayloadJson = "{}"
	}
	_, err := s.db.Exec(`
		INSERT INTO workflow_events
		  (id,workflowId,revision,kind,nodeId,summary,payloadJson,payloadHash,createdAt)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.WorkflowID, rec.Revision, rec.Kind, nullStr(rec.NodeID),
		rec.Summary, rec.PayloadJson, rec.PayloadHash, rec.CreatedAt)
	if err != nil {
		return domain.WorkflowGraphEventRecord{}, fmt.Errorf("insert workflow event: %w", err)
	}
	return rec, nil
}

// ListWorkflowGraphEvents returns a graph's newest events first, capped at
// limit (non-positive ⇒ nil).
func (s *Store) ListWorkflowGraphEvents(workflowID string, limit int) ([]domain.WorkflowGraphEventRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT "+workflowEventCols+
		" FROM workflow_events WHERE workflowId = ? ORDER BY createdAt DESC, id DESC LIMIT ?",
		workflowID, limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow events: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowGraphEventRecord
	for rows.Next() {
		e, err := scanWorkflowEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

/* -------------------------- workflow_resource_links ----------------------- */

const workflowResourceCols = `workflowId,resourceType,resourceRef,nodeId,label,status,metadataJson,createdAt,updatedAt`

func scanWorkflowResourceLink(sc scanner) (domain.WorkflowResourceLinkRecord, error) {
	var r domain.WorkflowResourceLinkRecord
	var nodeID, label, status sql.NullString
	if err := sc.Scan(&r.WorkflowID, &r.ResourceType, &r.ResourceRef, &nodeID,
		&label, &status, &r.MetadataJson, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return domain.WorkflowResourceLinkRecord{}, err
	}
	r.NodeID = strFromNull(nodeID)
	r.Label = strFromNull(label)
	r.Status = strFromNull(status)
	return r, nil
}

// UpsertWorkflowResourceLink inserts or refreshes one resource link. The
// natural key (workflowId, resourceType, resourceRef) makes re-linking
// idempotent: an existing row keeps its createdAt and takes the new
// node/label/status/metadata.
func (s *Store) UpsertWorkflowResourceLink(rec domain.WorkflowResourceLinkRecord) error {
	now := s.now()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.MetadataJson == "" {
		rec.MetadataJson = "{}"
	}
	_, err := s.db.Exec(`
		INSERT INTO workflow_resource_links
		  (workflowId,resourceType,resourceRef,nodeId,label,status,metadataJson,createdAt,updatedAt)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(workflowId,resourceType,resourceRef) DO UPDATE SET
		  nodeId = excluded.nodeId, label = excluded.label, status = excluded.status,
		  metadataJson = excluded.metadataJson, updatedAt = excluded.updatedAt`,
		rec.WorkflowID, rec.ResourceType, rec.ResourceRef, nullStr(rec.NodeID),
		nullStr(rec.Label), nullStr(rec.Status), rec.MetadataJson, rec.CreatedAt, now)
	if err != nil {
		return fmt.Errorf("upsert workflow resource link: %w", err)
	}
	return nil
}

// ListWorkflowResourceLinks returns all links for one graph, newest first.
func (s *Store) ListWorkflowResourceLinks(workflowID string) ([]domain.WorkflowResourceLinkRecord, error) {
	rows, err := s.db.Query("SELECT "+workflowResourceCols+
		" FROM workflow_resource_links WHERE workflowId = ? ORDER BY updatedAt DESC", workflowID)
	if err != nil {
		return nil, fmt.Errorf("list workflow resource links: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowResourceLinkRecord
	for rows.Next() {
		r, err := scanWorkflowResourceLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindWorkflowResourceLinks resolves a resource (type, ref) back to every
// graph that owns it — the reverse lookup an async completion or queue event
// uses to find its workflow/node.
func (s *Store) FindWorkflowResourceLinks(resourceType, resourceRef string) ([]domain.WorkflowResourceLinkRecord, error) {
	rows, err := s.db.Query("SELECT "+workflowResourceCols+
		" FROM workflow_resource_links WHERE resourceType = ? AND resourceRef = ? ORDER BY updatedAt DESC",
		resourceType, resourceRef)
	if err != nil {
		return nil, fmt.Errorf("find workflow resource links: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowResourceLinkRecord
	for rows.Next() {
		r, err := scanWorkflowResourceLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* -------------------------- workflow_reconcile_runs ----------------------- */

// InsertWorkflowReconcileRun records the start of one reconcile call
// (id/createdAt filled when zero).
func (s *Store) InsertWorkflowReconcileRun(rec domain.WorkflowReconcileRunRecord) (domain.WorkflowReconcileRunRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixReconcileRun)
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO workflow_reconcile_runs
		  (id,workflowId,baseRevision,appliedRevision,status,inputHash,outputHash,warning,createdAt)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.WorkflowID, rec.BaseRevision, nullI64(rec.AppliedRevision),
		rec.Status, rec.InputHash, nullStr(rec.OutputHash), nullStr(rec.Warning), rec.CreatedAt)
	if err != nil {
		return domain.WorkflowReconcileRunRecord{}, fmt.Errorf("insert workflow reconcile run: %w", err)
	}
	return rec, nil
}

var workflowReconcileUpdateCols = newColSet("appliedRevision", "status", "outputHash", "warning")

// UpdateWorkflowReconcileRun applies an allowlisted patch to a reconcile row
// (terminal outcome stamping).
func (s *Store) UpdateWorkflowReconcileRun(id string, patch map[string]any) error {
	return s.applyUpdate("workflow_reconcile_runs", workflowReconcileUpdateCols, id, patch)
}
