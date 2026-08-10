package storage

import (
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

// ---- artifacts: durable tool-result overflow payloads ----

// InsertArtifact persists one overflow payload (id artifact_ when zero, createdAt
// now when zero). The in-memory ArtifactStore is the hot path; this row is the
// fallback that survives the cache's 64-entry eviction and a process restart. The
// caller (agent.ArtifactStore.set) treats the write as best-effort and swallows the
// error — a DB failure must never break the turn that produced the artifact.
func (s *Store) InsertArtifact(rec domain.ArtifactRecord) (domain.ArtifactRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixArtifact)
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO artifacts (id,sessionId,content,totalChars,totalBytes,createdAt)
		VALUES (?,?,?,?,?,?)`,
		rec.ID, rec.SessionID, rec.Content, rec.TotalChars, rec.TotalBytes, rec.CreatedAt)
	if err != nil {
		return domain.ArtifactRecord{}, fmt.Errorf("insert artifact: %w", err)
	}
	return rec, nil
}

// GetArtifact returns the stored content for an id; ("", false) on a miss OR any
// error. The artifact.read fallback path calls this when the session's in-memory
// cache misses (an evicted id, or a stub rehydrated after restart), so a DB error
// degrades to a clean ARTIFACT_NOT_FOUND for the model rather than surfacing as a
// tool crash. Lookup is global by id (no sessionId filter): a rehydrated stub from a
// prior session must still resolve, and the ids are UUID-derived so cross-session
// reuse can't collide.
func (s *Store) GetArtifact(id string) (string, bool) {
	var content string
	// Any error — sql.ErrNoRows (an evicted/pruned/unknown id) or a real DB failure —
	// degrades to a clean miss. The read path must never throw; the model treats the
	// absent artifact as unretrievable rather than as a tool crash.
	if err := s.db.QueryRow(`SELECT content FROM artifacts WHERE id = ?`, id).Scan(&content); err != nil {
		return "", false
	}
	return content, true
}
