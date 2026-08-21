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
