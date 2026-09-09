package storage

import (
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

// ActivateAsyncSubmission atomically makes a starting invocation adoptable with
// its receipt. A concurrent cancellation cannot be resurrected.
func (s *Store) ActivateAsyncSubmission(id string, receipt domain.SubmissionReceipt, now int64) (bool, error) {
	return s.writeAsyncSubmission(id, receipt, now, true)
}

// CheckpointAsyncSubmission preserves decisive observations before the owner
// uses them. Receipt lifetime follows its invocation, including retained history.
func (s *Store) CheckpointAsyncSubmission(id string, receipt domain.SubmissionReceipt, now int64) (bool, error) {
	return s.writeAsyncSubmission(id, receipt, now, false)
}

func (s *Store) writeAsyncSubmission(id string, receipt domain.SubmissionReceipt, now int64, activate bool) (bool, error) {
	if !receipt.Valid() {
		return false, fmt.Errorf("invalid async submission receipt")
	}
	value, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := `UPDATE async_invocations SET status = status WHERE id = ? AND status IN ('running','settling')`
	args := []any{id}
	if activate {
		query = `UPDATE async_invocations SET status = 'running', startedAt = ? WHERE id = ? AND status = 'starting'`
		args = []any{now, id}
	}
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	_, err = tx.Exec(`INSERT INTO runtime_state (key,value,updatedAt) VALUES (?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updatedAt=excluded.updatedAt`, "async_submission:"+id, string(value), now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
