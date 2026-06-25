package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Two fixed slots back the durable compaction checkpoint: the current one and the
// immediately-preceding fallback. UpsertContextCheckpoint rotates prev←latest then
// writes the new latest; LoadLatestCheckpoint reads latest and, ONLY if its payload
// is unparseable, falls back to prev. Callers never name a slot — the rotation and
// fallback are entirely internal to this layer.
const (
	checkpointSlotLatest = "latest"
	checkpointSlotPrev   = "prev"
)

const checkpointCols = `slot,compactionDepth,summaryText,lastRunId,lastSeq,payloadJson,createdAt`

func scanCheckpoint(sc scanner) (domain.ContextCheckpointRecord, error) {
	var c domain.ContextCheckpointRecord
	var lastRunID sql.NullString
	if err := sc.Scan(&c.Slot, &c.CompactionDepth, &c.SummaryText, &lastRunID,
		&c.LastSeq, &c.PayloadJson, &c.CreatedAt); err != nil {
		return domain.ContextCheckpointRecord{}, err
	}
	c.LastRunID = lastRunID.String // "" when NULL
	return c, nil
}

// UpsertContextCheckpoint writes rec as the new 'latest' checkpoint, first rotating
// the existing 'latest' down into the 'prev' fallback slot — both in one transaction.
// The store is single-writer (SetMaxOpenConns(1)), so no other writer can interleave
// between the rotate and the overwrite: a concurrent LoadLatestCheckpoint never
// observes 'latest' overwritten without 'prev' already holding the prior value. The
// caller never picks a slot (rec.Slot is ignored); CreatedAt defaults to now when
// zero. Best-effort by convention at the call site (a checkpoint-write failure must
// never break compaction), so the error is returned for logging, not control flow.
func (s *Store) UpsertContextCheckpoint(rec domain.ContextCheckpointRecord) error {
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin checkpoint upsert: %w", err)
	}
	// Rollback is a no-op once Commit succeeds; on any early return it undoes the rotate.
	defer func() { _ = tx.Rollback() }()

	// Rotate the existing latest into prev (a no-op SELECT when there is no latest yet,
	// e.g. the very first checkpoint of a fresh DB).
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO context_checkpoints
		   (slot,compactionDepth,summaryText,lastRunId,lastSeq,payloadJson,createdAt)
		 SELECT ?,compactionDepth,summaryText,lastRunId,lastSeq,payloadJson,createdAt
		   FROM context_checkpoints WHERE slot = ?`,
		checkpointSlotPrev, checkpointSlotLatest); err != nil {
		return fmt.Errorf("rotate prev checkpoint: %w", err)
	}

	var runID any
	if rec.LastRunID != "" {
		runID = rec.LastRunID
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO context_checkpoints
		   (slot,compactionDepth,summaryText,lastRunId,lastSeq,payloadJson,createdAt)
		 VALUES (?,?,?,?,?,?,?)`,
		checkpointSlotLatest, rec.CompactionDepth, rec.SummaryText, runID,
		rec.LastSeq, rec.PayloadJson, rec.CreatedAt); err != nil {
		return fmt.Errorf("write latest checkpoint: %w", err)
	}
	return tx.Commit()
}

// LoadLatestCheckpoint returns the most recent VALID checkpoint. It reads the
// 'latest' slot first; if that row's PayloadJson is non-empty and not well-formed
// JSON (corruption) it falls back to the 'prev' slot. Returns (rec, true, nil) on a
// hit, (_, false, nil) when neither slot holds a valid checkpoint, and (_, false, err)
// only on an unexpected DB error. "Corrupt" is a JSON parse failure ONLY: a row with
// an EMPTY payload is still valid (an empty structured object is usable — it just
// carries no extra detail), so it is returned rather than skipped. The returned
// Slot field tells the caller which slot answered (useful for observability).
func (s *Store) LoadLatestCheckpoint() (domain.ContextCheckpointRecord, bool, error) {
	for _, slot := range []string{checkpointSlotLatest, checkpointSlotPrev} {
		row := s.db.QueryRow("SELECT "+checkpointCols+" FROM context_checkpoints WHERE slot = ?", slot)
		rec, err := scanCheckpoint(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return domain.ContextCheckpointRecord{}, false, fmt.Errorf("load %s checkpoint: %w", slot, err)
		}
		if rec.PayloadJson != "" && !json.Valid([]byte(rec.PayloadJson)) {
			// Unparseable payload — treat as corrupt and try the fallback slot.
			continue
		}
		return rec, true, nil
	}
	return domain.ContextCheckpointRecord{}, false, nil
}
