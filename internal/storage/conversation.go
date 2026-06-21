package storage

import (
	"database/sql"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
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
		INSERT INTO conversation (id,sessionId,seq,role,content,toolCallsJson,toolCallId,createdAt)
		VALUES (?,?,?,?,?,?,?,?)`,
		rec.ID, rec.SessionID, rec.Seq, rec.Role, rec.Content,
		nullStr(rec.ToolCallsJson), nullStr(rec.ToolCallID), rec.CreatedAt)
	if err != nil {
		return domain.ConversationMessageRecord{}, fmt.Errorf("insert message: %w", err)
	}
	return rec, nil
}

// ListMessages returns a session's messages in seq order.
func (s *Store) ListMessages(sessionID string) ([]domain.ConversationMessageRecord, error) {
	rows, err := s.db.Query(`
		SELECT id,sessionId,seq,role,content,toolCallsJson,toolCallId,createdAt
		  FROM conversation WHERE sessionId = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var out []domain.ConversationMessageRecord
	for rows.Next() {
		var m domain.ConversationMessageRecord
		var toolCalls, toolCallID sql.NullString
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content,
			&toolCalls, &toolCallID, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ToolCallsJson = strFromNull(toolCalls)
		m.ToolCallID = strFromNull(toolCallID)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- skill_selection_log ----

// InsertSkillSelection appends a selector diagnostic (id rsl_, ts now when zero).
func (s *Store) InsertSkillSelection(rec domain.SkillSelectionLogRecord) (domain.SkillSelectionLogRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixSkillSel)
	}
	if rec.Ts == 0 {
		rec.Ts = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO skill_selection_log
		  (id,ts,sessionId,userInput,selectedSkillIdsJson,confidence,taskType,reason)
		VALUES (?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Ts, rec.SessionID, rec.UserInput, rec.SelectedSkillIdsJson,
		rec.Confidence, nullStr(rec.TaskType), nullStr(rec.Reason))
	if err != nil {
		return domain.SkillSelectionLogRecord{}, fmt.Errorf("insert skill selection: %w", err)
	}
	return rec, nil
}

// ListSkillSelections returns the most recent selections, ts DESC.
func (s *Store) ListSkillSelections(limit int) ([]domain.SkillSelectionLogRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id,ts,sessionId,userInput,selectedSkillIdsJson,confidence,taskType,reason
		  FROM skill_selection_log ORDER BY ts DESC LIMIT %d`, limit))
	if err != nil {
		return nil, fmt.Errorf("list skill selections: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillSelectionLogRecord
	for rows.Next() {
		var r domain.SkillSelectionLogRecord
		var taskType, reason sql.NullString
		if err := rows.Scan(&r.ID, &r.Ts, &r.SessionID, &r.UserInput,
			&r.SelectedSkillIdsJson, &r.Confidence, &taskType, &reason); err != nil {
			return nil, err
		}
		r.TaskType = strFromNull(taskType)
		r.Reason = strFromNull(reason)
		out = append(out, r)
	}
	return out, rows.Err()
}
