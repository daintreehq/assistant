package storage

import (
	"database/sql"
	"errors"
	"fmt"
)

// Well-known runtime_state keys. The table is a tiny cross-process handoff
// surface for the persistent supervisor — resist the urge to grow it into a
// config store.
const (
	// RuntimeKeyCurrentSession holds the session id of the live conversation.
	// The last interactive owner writes it; a detached supervisor daemon reads
	// it so its autonomous wake turns continue the SAME transcript.
	RuntimeKeyCurrentSession = "current_session_id"
	// runtimeKeyBackendStatePrefix namespaces the per-session opaque backend
	// state token (server-signed skill-selection state).
	runtimeKeyBackendStatePrefix = "backend_state:"
	// RuntimeKeyDetachedActivity accumulates what the supervisor daemon did while
	// no assistant was attached (wake-turn count + last reply preview, JSON).
	// Written by the daemon after each successful wake turn; consumed (read +
	// deleted) by the next attaching assistant for its "while you were away"
	// notice.
	RuntimeKeyDetachedActivity = "detached_activity"
)

// PutRuntimeState upserts one key. Empty value deletes the key — callers treat
// "" and absent identically, and storing an empty token would just smuggle the
// same information less clearly.
func (s *Store) PutRuntimeState(key, value string) error {
	if key == "" {
		return errors.New("runtime state: empty key")
	}
	if value == "" {
		return s.DeleteRuntimeState(key)
	}
	_, err := s.db.Exec(`
		INSERT INTO runtime_state (key, value, updatedAt) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updatedAt = excluded.updatedAt`,
		key, value, s.now())
	if err != nil {
		return fmt.Errorf("put runtime state %q: %w", key, err)
	}
	return nil
}

// GetRuntimeState reads one key; ("", nil) when absent.
func (s *Store) GetRuntimeState(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM runtime_state WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get runtime state %q: %w", key, err)
	}
	return v, nil
}

// DeleteRuntimeState removes one key (idempotent).
func (s *Store) DeleteRuntimeState(key string) error {
	if _, err := s.db.Exec("DELETE FROM runtime_state WHERE key = ?", key); err != nil {
		return fmt.Errorf("delete runtime state %q: %w", key, err)
	}
	return nil
}

// PutSessionBackendState persists the opaque backend state token for a session
// (empty token clears it).
func (s *Store) PutSessionBackendState(sessionID, token string) error {
	if sessionID == "" {
		return errors.New("runtime state: empty session id")
	}
	return s.PutRuntimeState(runtimeKeyBackendStatePrefix+sessionID, token)
}

// GetSessionBackendState loads the persisted backend state token for a session;
// "" when none — a perfectly valid starting point (the backend just re-runs
// skill selection on the first respond).
func (s *Store) GetSessionBackendState(sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	return s.GetRuntimeState(runtimeKeyBackendStatePrefix + sessionID)
}
