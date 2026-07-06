package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// ---- async_invocations (runtime-owned async tool futures) ----

// InsertAsyncInvocation inserts an async invocation, defaulting id (asy_),
// status 'starting', and createdAt now (when the caller leaves them zero/empty).
func (s *Store) InsertAsyncInvocation(rec domain.AsyncInvocationRecord) (domain.AsyncInvocationRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixAsync)
	}
	if rec.Status == "" {
		rec.Status = domain.AsyncStarting
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	if rec.GroupID == "" {
		// An invocation always has a coalescing group; a caller with no run id
		// self-groups so the settle-grace logic needs no nil-group special case.
		rec.GroupID = rec.ID
	}
	_, err := s.db.Exec(`
		INSERT INTO async_invocations
		  (id,toolName,title,groupId,sessionId,terminalIdsJson,command,
		   status,outcomesJson,lastError,queueEventId,endedReason,createdAt,startedAt,
		   expiresAt,finishedAt)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.ToolName, rec.Title, rec.GroupID, rec.SessionID,
		rec.TerminalIdsJson, nullStr(rec.Command), string(rec.Status),
		nullStr(rec.OutcomesJson), nullStr(rec.LastError), nullStr(rec.QueueEventID),
		nullStr(rec.EndedReason), rec.CreatedAt, nullI64(rec.StartedAt),
		rec.ExpiresAt, nullI64(rec.FinishedAt))
	if err != nil {
		return domain.AsyncInvocationRecord{}, fmt.Errorf("insert async invocation: %w", err)
	}
	return rec, nil
}

const asyncCols = `id,toolName,title,groupId,sessionId,terminalIdsJson,command,status,outcomesJson,lastError,queueEventId,endedReason,createdAt,startedAt,expiresAt,finishedAt`

func scanAsyncInvocation(sc scanner) (domain.AsyncInvocationRecord, error) {
	var a domain.AsyncInvocationRecord
	var command, outcomes, lastErr, queueEventID, endedReason sql.NullString
	var startedAt, finishedAt sql.NullInt64
	var status string
	if err := sc.Scan(&a.ID, &a.ToolName, &a.Title, &a.GroupID, &a.SessionID,
		&a.TerminalIdsJson, &command, &status, &outcomes, &lastErr,
		&queueEventID, &endedReason, &a.CreatedAt, &startedAt, &a.ExpiresAt,
		&finishedAt); err != nil {
		return domain.AsyncInvocationRecord{}, err
	}
	a.Command = strFromNull(command)
	a.Status = domain.AsyncStatus(status)
	a.OutcomesJson = strFromNull(outcomes)
	a.LastError = strFromNull(lastErr)
	a.QueueEventID = strFromNull(queueEventID)
	a.EndedReason = strFromNull(endedReason)
	a.StartedAt = i64FromNull(startedAt)
	a.FinishedAt = i64FromNull(finishedAt)
	return a, nil
}

// GetAsyncInvocation returns an invocation by id, or (nil, nil) when absent.
func (s *Store) GetAsyncInvocation(id string) (*domain.AsyncInvocationRecord, error) {
	row := s.db.QueryRow("SELECT "+asyncCols+" FROM async_invocations WHERE id = ?", id)
	a, err := scanAsyncInvocation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get async invocation: %w", err)
	}
	return &a, nil
}

// ListAsyncInvocations returns all invocations (status == "") or those of one
// status, ORDER BY createdAt.
func (s *Store) ListAsyncInvocations(status string) ([]domain.AsyncInvocationRecord, error) {
	q := "SELECT " + asyncCols + " FROM async_invocations"
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY createdAt"
	return queryAsyncInvocations(s.db, q, args...)
}

// asyncLiveStatuses is the non-terminal set (fixed internal identifiers — safe to
// interpolate). Mirrors domain.AsyncStatus.IsTerminal's complement.
const asyncLiveStatuses = "('starting','running','settling')"

// ListLiveAsyncInvocations returns the non-terminal invocations
// (starting/running/settling), ORDER BY createdAt — the coordinator's working
// set, the model-facing turn-context block, and the ops deck all read this.
func (s *Store) ListLiveAsyncInvocations() ([]domain.AsyncInvocationRecord, error) {
	return queryAsyncInvocations(s.db,
		"SELECT "+asyncCols+" FROM async_invocations WHERE status IN "+asyncLiveStatuses+" ORDER BY createdAt")
}

// CountLiveAsyncInvocations reports how many invocations are non-terminal — the
// backpressure input for the async tools' active-work cap.
func (s *Store) CountLiveAsyncInvocations() (int, error) {
	var n int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM async_invocations WHERE status IN " + asyncLiveStatuses).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live async invocations: %w", err)
	}
	return n, nil
}

func queryAsyncInvocations(db *sql.DB, q string, args ...any) ([]domain.AsyncInvocationRecord, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query async invocations: %w", err)
	}
	defer rows.Close()
	var out []domain.AsyncInvocationRecord
	for rows.Next() {
		a, err := scanAsyncInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// asyncUpdateCols are the mutable columns (id/createdAt and the identity fields
// are immutable after insert).
var asyncUpdateCols = newColSet("title", "status", "outcomesJson", "lastError",
	"queueEventId", "endedReason", "startedAt", "expiresAt", "finishedAt")

// UpdateAsyncInvocation applies an allowlisted patch (no-op when no allowed key
// is set).
func (s *Store) UpdateAsyncInvocation(id string, patch map[string]any) error {
	return s.applyUpdate("async_invocations", asyncUpdateCols, id, patch)
}

// ClaimLiveAsyncInvocation atomically applies `patch` ONLY while the invocation
// is still non-terminal. Returns true iff a row changed. A false return means a
// concurrent cancel (async.cancel) already finalized it — the coordinator must
// then NOT write it back, or it would resurrect a cancelled invocation (the
// same claim discipline as ClaimDueTimer / ClaimDueWatcher).
func (s *Store) ClaimLiveAsyncInvocation(id string, patch map[string]any) (bool, error) {
	n, err := s.applyUpdateGuarded("async_invocations", asyncUpdateCols, id, patch,
		" AND status IN "+asyncLiveStatuses)
	return n > 0, err
}

// StampAsyncQueueEvents back-links a published group's rows to their queue
// event in ONE statement. Atomicity is the point: the coordinator publishes a
// group event, then stamps — a crash between the two leaves the WHOLE group
// unstamped, so the next owner's adoption retries the publish with the same
// member set (same dedupe key ⇒ a queue-level dedupe hit, never a duplicate
// wake). A per-row stamp loop could crash half-done and split the set.
func (s *Store) StampAsyncQueueEvents(ids []string, eventID string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, eventID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.Exec(
		"UPDATE async_invocations SET queueEventId = ? WHERE id IN ("+placeholders[:len(placeholders)-1]+")",
		args...)
	if err != nil {
		return fmt.Errorf("stamp async queue events: %w", err)
	}
	return nil
}

// ListUnpublishedAsyncInvocations returns invocations a prior owner finalized
// (succeeded/failed/expired) but never confirmed a queue publish for
// (queueEventId IS NULL) — the crash window between "row terminal" and "event
// in the inbox". The adopting coordinator retries exactly these publishes; the
// stable per-group DedupeKey makes a retry idempotent at the queue. Cancelled
// and abandoned rows are deliberately excluded: those endings never publish.
func (s *Store) ListUnpublishedAsyncInvocations() ([]domain.AsyncInvocationRecord, error) {
	return queryAsyncInvocations(s.db,
		"SELECT "+asyncCols+" FROM async_invocations WHERE status IN ('succeeded','failed','expired') AND queueEventId IS NULL ORDER BY createdAt")
}

// CancelLiveAsyncInvocations cancels EVERY live invocation in ONE statement
// (/clear's clean slate), stamping the caller's endedReason. It snapshots the
// affected ids INSIDE the transaction so the caller can deregister exactly the
// rows the UPDATE flipped — the same read-then-flip shape as CancelLiveWatchers.
func (s *Store) CancelLiveAsyncInvocations(now int64, reason string) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin cancel live async invocations: %w", err)
	}
	rows, err := tx.Query(`SELECT id FROM async_invocations WHERE status IN ` + asyncLiveStatuses)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("select live async invocations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return nil, fmt.Errorf("scan live async invocation id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return nil, fmt.Errorf("iterate live async invocation ids: %w", err)
	}
	_ = rows.Close()
	if _, err := tx.Exec(`
		UPDATE async_invocations
		   SET status = 'cancelled', endedReason = ?, finishedAt = ?
		 WHERE status IN `+asyncLiveStatuses,
		reason, now); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("cancel live async invocations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("commit cancel live async invocations: %w", err)
	}
	return ids, nil
}
