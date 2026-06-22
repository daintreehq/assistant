package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

const memoryCols = `id,content,category,source,pinnedAt,deletedAt,createdAt,updatedAt`

func scanMemory(sc scanner) (domain.MemoryRecord, error) {
	var m domain.MemoryRecord
	var source string
	var category sql.NullString
	var pinnedAt, deletedAt sql.NullInt64
	if err := sc.Scan(&m.ID, &m.Content, &category, &source, &pinnedAt, &deletedAt,
		&m.CreatedAt, &m.UpdatedAt); err != nil {
		return domain.MemoryRecord{}, err
	}
	m.Source = domain.MemorySource(source)
	m.Category = strFromNull(category)
	m.PinnedAt = i64FromNull(pinnedAt)
	m.DeletedAt = i64FromNull(deletedAt)
	return m, nil
}

// InsertMemory inserts a memory (id mem_, source 'assistant',
// createdAt/updatedAt now when zero). The memories_ai trigger indexes content
// into FTS.
func (s *Store) InsertMemory(rec domain.MemoryRecord) (domain.MemoryRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixMemory)
	}
	if rec.Source == "" {
		rec.Source = domain.MemoryAssistant
	}
	now := s.now()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	_, err := s.db.Exec(`
		INSERT INTO memories (id,content,category,source,pinnedAt,deletedAt,createdAt,updatedAt)
		VALUES (?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Content, nullStr(rec.Category), string(rec.Source),
		nullI64(rec.PinnedAt), nullI64(rec.DeletedAt), rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		return domain.MemoryRecord{}, fmt.Errorf("insert memory: %w", err)
	}
	return rec, nil
}

// GetMemory returns a memory by id. By default a soft-deleted memory (DeletedAt
// non-null — explicit null check, so epoch 0 still counts as deleted) is hidden;
// pass includeDeleted=true to see it.
func (s *Store) GetMemory(id string, includeDeleted bool) (*domain.MemoryRecord, error) {
	row := s.db.QueryRow("SELECT "+memoryCols+" FROM memories WHERE id = ?", id)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get memory: %w", err)
	}
	if !includeDeleted && m.DeletedAt != nil {
		return nil, nil
	}
	return &m, nil
}

// MemoryListOptions filters ListMemories.
type MemoryListOptions struct {
	Category       *string
	PinnedOnly     bool
	IncludeDeleted bool
	Limit          *int
}

// ListMemories returns memories filtered by deletedAt/category/pinned, pinned
// first then by COALESCE(pinnedAt, updatedAt) DESC. Limit clamps to [1,200],
// default 50.
func (s *Store) ListMemories(opts MemoryListOptions) ([]domain.MemoryRecord, error) {
	conds := []string{}
	var args []any
	if !opts.IncludeDeleted {
		conds = append(conds, "deletedAt IS NULL")
	}
	if opts.Category != nil {
		conds = append(conds, "category = ?")
		args = append(args, *opts.Category)
	}
	if opts.PinnedOnly {
		conds = append(conds, "pinnedAt IS NOT NULL")
	}
	q := "SELECT " + memoryCols + " FROM memories"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY (pinnedAt IS NOT NULL) DESC, COALESCE(pinnedAt, updatedAt) DESC"
	q += fmt.Sprintf(" LIMIT %d", clamp(derefInt(opts.Limit, 50), 1, 200))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	return collectMemories(rows)
}

// MemoryRecallOptions filters RecallMemories.
type MemoryRecallOptions struct {
	Category *string
	Limit    *int
}

// RecallMemories runs an FTS5 implicit-AND keyword search over non-deleted
// memories, ranked by bm25. The query is tokenized + per-token quoted
// (escapeFTSQuery) — NEVER passed raw to MATCH (security boundary). Empty/blank
// query ⇒ empty result. Limit clamps to [1,50], default 10.
func (s *Store) RecallMemories(query string, opts MemoryRecallOptions) ([]domain.MemoryRecord, error) {
	match := escapeFTSQuery(query)
	if match == "" {
		return nil, nil
	}
	conds := []string{"m.deletedAt IS NULL", "memories_fts MATCH ?"}
	args := []any{match}
	if opts.Category != nil {
		conds = append(conds, "m.category = ?")
		args = append(args, *opts.Category)
	}
	q := "SELECT " + prefixCols("m", memoryCols) + `
		  FROM memories_fts
		  JOIN memories m ON m.rowid = memories_fts.rowid
		 WHERE ` + strings.Join(conds, " AND ") + `
		 ORDER BY bm25(memories_fts)
		 LIMIT ` + fmt.Sprintf("%d", clamp(derefInt(opts.Limit, 10), 1, 50))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("recall memories: %w", err)
	}
	defer rows.Close()
	return collectMemories(rows)
}

// MemoryExists reports whether a memory with EXACTLY this content has ever been stored
// — INCLUDING soft-deleted rows. The distill-on-compact path uses it to skip re-saving
// a fact it already holds: matching live rows avoids duplicates, and matching
// soft-deleted rows means a memory the user explicitly forgot is not silently
// resurrected by a later compaction. Deliberately an exact match (predictable and
// cheap) rather than an FTS similarity threshold (which needs tuning and
// false-positives on short facts).
func (s *Store) MemoryExists(content string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		"SELECT 1 FROM memories WHERE content = ? LIMIT 1", content).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory exists: %w", err)
	}
	return true, nil
}

// ForgetMemory soft-deletes (stamp deletedAt+updatedAt WHERE deletedAt IS NULL).
// It does NOT mutate content, so the FTS old.content still matches on a later
// hard-delete sweep (KEEP). Reports whether changed.
func (s *Store) ForgetMemory(id string, now int64) (bool, error) {
	if now <= 0 {
		now = s.now()
	}
	res, err := s.db.Exec(
		"UPDATE memories SET deletedAt = ?, updatedAt = ? WHERE id = ? AND deletedAt IS NULL",
		now, now, id)
	if err != nil {
		return false, fmt.Errorf("forget memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("forget memory rows affected: %w", err)
	}
	return n > 0, nil
}

// PinMemory stamps pinnedAt+updatedAt WHERE not-deleted AND not-already-pinned
// (idempotent guard avoids re-jumping the order). Returns the post-update memory
// (or the existing one when the guard was a no-op).
func (s *Store) PinMemory(id string, now int64) (*domain.MemoryRecord, error) {
	if now <= 0 {
		now = s.now()
	}
	if _, err := s.db.Exec(
		"UPDATE memories SET pinnedAt = ?, updatedAt = ? WHERE id = ? AND deletedAt IS NULL AND pinnedAt IS NULL",
		now, now, id); err != nil {
		return nil, fmt.Errorf("pin memory: %w", err)
	}
	return s.GetMemory(id, false)
}

// UnpinMemory clears pinnedAt+updatedAt WHERE not-deleted AND currently-pinned
// (idempotent). Returns the post-update memory.
func (s *Store) UnpinMemory(id string, now int64) (*domain.MemoryRecord, error) {
	if now <= 0 {
		now = s.now()
	}
	if _, err := s.db.Exec(
		"UPDATE memories SET pinnedAt = NULL, updatedAt = ? WHERE id = ? AND deletedAt IS NULL AND pinnedAt IS NOT NULL",
		now, id); err != nil {
		return nil, fmt.Errorf("unpin memory: %w", err)
	}
	return s.GetMemory(id, false)
}

// ---- small helpers ----

func collectMemories(rows *sql.Rows) ([]domain.MemoryRecord, error) {
	var out []domain.MemoryRecord
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// prefixCols qualifies a comma-joined column list with a table alias (for the FTS
// JOIN where bare column names would be ambiguous).
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
