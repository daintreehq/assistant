package storage

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

const workflowCols = `id,issueNumber,issueUrl,issueTitle,branch,worktreeId,prNumber,prUrl,terminalIdsJson,watcherIdsJson,queueEventIdsJson,status,nextActionJson,notesJson,createdAt,updatedAt,completedAt`

func scanWorkflowRun(sc scanner) (domain.WorkflowRunRecord, error) {
	var w domain.WorkflowRunRecord
	var issueNum, prNum sql.NullInt64
	var completedAt sql.NullInt64
	var status string
	var issueURL, issueTitle, branch, worktreeID, prURL sql.NullString
	var termJson, watchJson, queueJson, nextAction, notes sql.NullString
	if err := sc.Scan(&w.ID, &issueNum, &issueURL, &issueTitle, &branch, &worktreeID,
		&prNum, &prURL, &termJson, &watchJson, &queueJson, &status, &nextAction, &notes,
		&w.CreatedAt, &w.UpdatedAt, &completedAt); err != nil {
		return domain.WorkflowRunRecord{}, err
	}
	w.IssueNumber = intFromNull(issueNum)
	w.IssueURL = strFromNull(issueURL)
	w.IssueTitle = strFromNull(issueTitle)
	w.Branch = strFromNull(branch)
	w.WorktreeID = strFromNull(worktreeID)
	w.PRNumber = intFromNull(prNum)
	w.PRURL = strFromNull(prURL)
	w.TerminalIdsJson = strFromNull(termJson)
	w.WatcherIdsJson = strFromNull(watchJson)
	w.QueueEventIdsJson = strFromNull(queueJson)
	w.Status = domain.WorkflowRunStatus(status)
	w.NextActionJson = strFromNull(nextAction)
	w.NotesJson = strFromNull(notes)
	w.CompletedAt = i64FromNull(completedAt)
	return w, nil
}

// InsertWorkflowRun inserts a workflow run (id wfr_, status 'pending',
// createdAt/updatedAt now when zero).
func (s *Store) InsertWorkflowRun(rec domain.WorkflowRunRecord) (domain.WorkflowRunRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflow)
	}
	if rec.Status == "" {
		rec.Status = domain.WorkflowPending
	}
	now := s.now()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	_, err := s.db.Exec(`
		INSERT INTO workflow_runs
		  (id,issueNumber,issueUrl,issueTitle,branch,worktreeId,prNumber,prUrl,
		   terminalIdsJson,watcherIdsJson,queueEventIdsJson,status,nextActionJson,
		   notesJson,createdAt,updatedAt,completedAt)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, nullInt(rec.IssueNumber), nullStr(rec.IssueURL), nullStr(rec.IssueTitle),
		nullStr(rec.Branch), nullStr(rec.WorktreeID), nullInt(rec.PRNumber), nullStr(rec.PRURL),
		nullStr(rec.TerminalIdsJson), nullStr(rec.WatcherIdsJson), nullStr(rec.QueueEventIdsJson),
		string(rec.Status), nullStr(rec.NextActionJson), nullStr(rec.NotesJson),
		rec.CreatedAt, rec.UpdatedAt, nullI64(rec.CompletedAt))
	if err != nil {
		return domain.WorkflowRunRecord{}, fmt.Errorf("insert workflow run: %w", err)
	}
	return rec, nil
}

// GetWorkflowRun returns a run by id, or (nil, nil) when absent.
func (s *Store) GetWorkflowRun(id string) (*domain.WorkflowRunRecord, error) {
	row := s.db.QueryRow("SELECT "+workflowCols+" FROM workflow_runs WHERE id = ?", id)
	w, err := scanWorkflowRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow run: %w", err)
	}
	return &w, nil
}

// ListWorkflowRuns returns all runs (status=="") or by status, ORDER BY updatedAt DESC.
func (s *Store) ListWorkflowRuns(status string) ([]domain.WorkflowRunRecord, error) {
	q := "SELECT " + workflowCols + " FROM workflow_runs"
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY updatedAt DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowRunRecord
	for rows.Next() {
		w, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListNonTerminalWorkflowRuns returns the open (pending|active|blocked) runs,
// most-recently-updated first, capped at limit. It backs the read-only turn-footer
// view, so it filters to non-terminal statuses IN SQL (one round trip, no Go-side
// post-filter) and bounds the row count — the footer only needs the top handful of
// open runs, never the whole history. A non-positive limit returns nil rather than
// firing an unbounded query (a footer asking for ≤0 rows wants nothing).
func (s *Store) ListNonTerminalWorkflowRuns(limit int) ([]domain.WorkflowRunRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT "+workflowCols+
		" FROM workflow_runs WHERE status IN (?, ?, ?) ORDER BY updatedAt DESC LIMIT ?",
		string(domain.WorkflowPending), string(domain.WorkflowActive), string(domain.WorkflowBlocked), limit)
	if err != nil {
		return nil, fmt.Errorf("list non-terminal workflow runs: %w", err)
	}
	defer rows.Close()
	var out []domain.WorkflowRunRecord
	for rows.Next() {
		w, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

var workflowUpdateCols = newColSet("issueNumber", "issueUrl", "issueTitle", "branch",
	"worktreeId", "prNumber", "prUrl", "terminalIdsJson", "watcherIdsJson",
	"queueEventIdsJson", "status", "nextActionJson", "notesJson", "completedAt")

// UpdateWorkflowRun applies an allowlisted patch with a NO-OP GUARD: it only
// proceeds when the patch changes an allowed column OTHER than updatedAt, then
// force-sets updatedAt = now (never taken from the patch). completedAt IS
// caller-settable (KEEP).
func (s *Store) UpdateWorkflowRun(id string, patch map[string]any) error {
	if !patchHasAllowedKey(patch, workflowUpdateCols) {
		return nil // no-op: avoid bumping updatedAt for an empty/irrelevant patch
	}
	merged := make(map[string]any, len(patch)+1)
	for k, v := range patch {
		merged[k] = v
	}
	merged["updatedAt"] = s.now() // store-forced
	return s.applyUpdate("workflow_runs", workflowUpdateColsWithTime, id, merged)
}

// workflowUpdateColsWithTime adds updatedAt to the allowlist so applyUpdate can
// write the store-forced timestamp (callers can't set it themselves — it's stripped
// from their patch only by the no-op guard reading workflowUpdateCols).
var workflowUpdateColsWithTime = unionColSet(workflowUpdateCols, "updatedAt")
