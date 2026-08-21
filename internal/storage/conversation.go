package storage

import (
	"database/sql"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

// ---- conversation ----

// InsertMessage appends a conversation message (id msg_, createdAt now when zero).
func (s *Store) InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixMessage)
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO conversation (id,sessionId,seq,role,content,name,reasoningContent,toolCallsJson,toolCallId,createdAt)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.SessionID, rec.Seq, rec.Role, rec.Content, nullStr(rec.Name),
		nullStr(rec.ReasoningContent), nullStr(rec.ToolCallsJson), nullStr(rec.ToolCallID), rec.CreatedAt)
	if err != nil {
		return domain.ConversationMessageRecord{}, fmt.Errorf("insert message: %w", err)
	}
	return rec, nil
}

// InsertMessages appends several conversation messages in ONE transaction: either every
// row lands or none does.
//
// It exists for the compaction boundary. A compaction rewrites the working history by
// writing a marker row (which is where rehydration starts reading), then the block, then
// the retained tail. Written as independent autocommit inserts, a failure between them
// leaves a durable state that is neither the old history nor the new one — a marker with
// no block behind it hides intact rows and resumes an empty conversation. Since the
// marker is the thing that MOVES the boundary, the group has to be atomic or the log can
// point at history that was never written.
//
// Records are stamped in order; ids and createdAt are filled the same way InsertMessage
// fills them.
func (s *Store) InsertMessages(recs []domain.ConversationMessageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("insert messages: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	for _, rec := range recs {
		if rec.ID == "" {
			rec.ID = domain.NewID(domain.PrefixMessage)
		}
		if rec.CreatedAt == 0 {
			rec.CreatedAt = s.now()
		}
		if _, err := tx.Exec(`
			INSERT INTO conversation (id,sessionId,seq,role,content,name,reasoningContent,toolCallsJson,toolCallId,createdAt)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			rec.ID, rec.SessionID, rec.Seq, rec.Role, rec.Content, nullStr(rec.Name),
			nullStr(rec.ReasoningContent), nullStr(rec.ToolCallsJson), nullStr(rec.ToolCallID), rec.CreatedAt); err != nil {
			return fmt.Errorf("insert messages: %w", err)
		}
	}
	return tx.Commit()
}

// ListMessages returns a session's messages in seq order.
func (s *Store) ListMessages(sessionID string) ([]domain.ConversationMessageRecord, error) {
	rows, err := s.db.Query(`
		SELECT id,sessionId,seq,role,content,name,reasoningContent,toolCallsJson,toolCallId,createdAt
		  FROM conversation WHERE sessionId = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []domain.ConversationMessageRecord
	for rows.Next() {
		var m domain.ConversationMessageRecord
		var name, reasoning, toolCalls, toolCallID sql.NullString
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &name,
			&reasoning, &toolCalls, &toolCallID, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Name = strFromNull(name)
		m.ReasoningContent = strFromNull(reasoning)
		m.ToolCallsJson = strFromNull(toolCalls)
		m.ToolCallID = strFromNull(toolCallID)
		out = append(out, m)
	}
	return out, rows.Err()
}
