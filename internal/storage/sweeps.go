package storage

import (
	"fmt"
	"strings"
)

// cancelStaleWatchers runs on DB open (session boundary). KEEP EXACT ORDER
// (storage.md §3 cancelStaleWatchers, §7.1):
//
//  1. revoke automation_grants for watcher actors whose watcher is non-terminal
//     (status in active/created/paused) and not already revoked;
//  2. flip those watchers to status='cancelled';
//  3. resolve all open events sourced from the three watcher sources.
//
// Order matters: revoke BEFORE the flip so no grant is ever live for a watcher we
// just cancelled. Terminal-status watchers (condition_met/timeout/cancelled/error)
// are left for the UI history. Scoped to the 3 watcher sources so timer/system/
// user events persist across sessions.
func (s *Store) cancelStaleWatchers(now int64) error {
	const liveStatuses = "('active','created','paused')"

	// Wrap the three ordered statements in ONE transaction so the session boundary
	// either fully applies or not at all — a mid-failure must not leave a watcher
	// cancelled while its grant stays live, or vice versa. Single connection ⇒
	// BEGIN IMMEDIATE serialization; only tx-scoped statements inside.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin cancel stale watchers: %w", err)
	}

	// (1) revoke watcher grants pointing at non-terminal watchers (BEFORE the flip,
	// so no grant is ever live for a watcher we just cancelled).
	if _, err := tx.Exec(`
		UPDATE automation_grants
		   SET revokedAt = ?
		 WHERE actorType = 'watcher'
		   AND revokedAt IS NULL
		   AND actorId IN (SELECT id FROM watchers WHERE status IN `+liveStatuses+`)`,
		now); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("revoke watcher grants: %w", err)
	}

	// (2) cancel non-terminal watchers.
	if _, err := tx.Exec(
		`UPDATE watchers SET status = 'cancelled' WHERE status IN ` + liveStatuses); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("cancel watchers: %w", err)
	}

	// (3) resolve open watcher-sourced events (no sessionId / TTL on these).
	if _, err := tx.Exec(`
		UPDATE events
		   SET resolvedAt = ?
		 WHERE resolvedAt IS NULL
		   AND source IN ('terminal_watcher','worktree_watcher','pr_watcher')`,
		now); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("resolve watcher events: %w", err)
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit cancel stale watchers: %w", err)
	}
	return nil
}

// cancelStaleAgentLaunches fails any non-terminal spawn saga on open so a fresh
// launch's idempotencyKey isn't blocked by a dead in-flight record (storage.md
// §7.2). COALESCE preserves any pre-existing error code/message.
func (s *Store) cancelStaleAgentLaunches(now int64) error {
	_, err := s.db.Exec(`
		UPDATE agent_launches
		   SET stage        = 'failed',
		       errorCode    = COALESCE(errorCode, 'SESSION_ENDED'),
		       errorMessage = COALESCE(errorMessage, 'session ended before confirmation'),
		       updatedAt    = ?
		 WHERE stage NOT IN ('confirmed','failed')`, now)
	if err != nil {
		return fmt.Errorf("fail stale agent launches: %w", err)
	}
	return nil
}

// GCRetentionSweep enforces the retention policy. ORDER (storage.md §7.3): prune
// whole old runs FIRST (co-deletes their audit rows so audit's own count floor is
// spent on retained rows, preserving the run_events↔audit_log pairing), then the
// age+count sweep on audit_log/conversation/skill_selection_log, then hard-delete
// terminal events past the window, then hard-delete soft-deleted memories past the
// undo window (the memories_ad trigger auto-evicts them from FTS — do NOT also
// issue a manual FTS delete). No VACUUM (freed pages return to the freelist).
//
// Exported (TS @internal) so tests can drive it with a pinned clock.
//
// PARTIAL GC IS ACCEPTABLE. The sweep is intentionally NOT one big transaction:
// each section is its own best-effort unit (and pruneOldRuns owns its own tx — see
// #2). The whole sweep is itself swallowed at construction (store.go: a sweep
// failure must never abort Open), so if a later section errors the earlier deletes
// simply stand — they are pure housekeeping (retention deletes are idempotent and
// re-run next session). Wrapping everything in one tx would only buy us "nothing
// deleted on any error", which is strictly worse for a best-effort GC and would
// force pruneOldRuns to give up its own tx. So we keep per-section best-effort.
func (s *Store) GCRetentionSweep(now int64) error {
	r := s.retention

	// run_events by whole run, FIRST.
	if err := s.pruneOldRuns(now-durMS(r.RunEventsMaxAge), r.RunEventsKeepRuns); err != nil {
		return err
	}
	if err := s.deleteByAgeAndCount("audit_log", "ts",
		now-durMS(r.AuditLogMaxAge), r.AuditLogKeepRows); err != nil {
		return err
	}
	if err := s.deleteByAgeAndCount("conversation", "createdAt",
		now-durMS(r.ConversationMaxAge), r.ConversationKeepRows); err != nil {
		return err
	}
	if err := s.deleteByAgeAndCount("skill_selection_log", "ts",
		now-durMS(r.SkillSelLogMaxAge), r.SkillSelLogKeepRows); err != nil {
		return err
	}

	// Terminal (resolved OR expired) events past the window. A row is terminal
	// when EITHER stamp is set, but it is only swept once the LATER of the two
	// stamps (the true terminal moment) has aged out — so an event resolved long
	// ago but expiring only recently is retained. MAX(COALESCE(...,0)) mirrors the
	// TS sweep; without it, a stale resolvedAt would prematurely drop a still-live
	// (recently-expiring) row.
	evCutoff := now - durMS(r.EventsTerminalAge)
	if _, err := s.db.Exec(`
		DELETE FROM events
		 WHERE (resolvedAt IS NOT NULL OR expiresAt IS NOT NULL)
		   AND MAX(COALESCE(resolvedAt, 0), COALESCE(expiresAt, 0)) < ?`,
		evCutoff); err != nil {
		return fmt.Errorf("sweep events: %w", err)
	}

	// soft-deleted memories past the undo window (trigger evicts FTS).
	if _, err := s.db.Exec(
		`DELETE FROM memories WHERE deletedAt IS NOT NULL AND deletedAt < ?`,
		now-durMS(r.MemoriesDeletedAge)); err != nil {
		return fmt.Errorf("sweep memories: %w", err)
	}
	return nil
}

// durMS converts a time.Duration (the retention window) to whole epoch ms.
func durMS(d interface{ Milliseconds() int64 }) int64 { return d.Milliseconds() }

// deleteByAgeAndCount keeps the newer of (rows past cutoff) OR (last keepN rows by
// timeCol). table/timeCol are FIXED internal identifiers — interpolation is safe
// (never user-supplied).
func (s *Store) deleteByAgeAndCount(table, timeCol string, cutoff int64, keepN int) error {
	q := fmt.Sprintf(`
		DELETE FROM %[1]s
		 WHERE %[2]s < ?
		   AND rowid NOT IN (
		     SELECT rowid FROM %[1]s ORDER BY %[2]s DESC LIMIT %[3]d)`,
		table, timeCol, keepN)
	if _, err := s.db.Exec(q, cutoff); err != nil {
		return fmt.Errorf("deleteByAgeAndCount %s: %w", table, err)
	}
	return nil
}

// pruneOldRuns selects run ids whose MAX(ts) < cutoff, skipping the most-recent
// keepRuns (LIMIT -1 OFFSET keepRuns), then deletes their run_events and audit_log
// rows, chunked at pruneChunk to stay under the bound-var limit. The candidate
// SELECT runs INSIDE the transaction (BEGIN before select), so a run_event appended
// after the cutoff snapshot can't be deleted by a later runId-scoped delete: the tx
// sees one consistent snapshot, and on the single connection no concurrent writer
// can append between the select and the deletes. Rolls back on any error.
func (s *Store) pruneOldRuns(cutoff int64, keepRuns int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin prune tx: %w", err)
	}

	rows, err := tx.Query(`
		SELECT runId FROM run_events
		 GROUP BY runId
		HAVING MAX(ts) < ?
		 ORDER BY MAX(ts) DESC
		 LIMIT -1 OFFSET ?`, cutoff, keepRuns)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("select old runs: %w", err)
	}
	var runIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return fmt.Errorf("scan run id: %w", err)
		}
		runIDs = append(runIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return err
	}
	_ = rows.Close()
	if len(runIDs) == 0 {
		_ = tx.Rollback()
		return nil
	}

	for start := 0; start < len(runIDs); start += pruneChunk {
		end := start + pruneChunk
		if end > len(runIDs) {
			end = len(runIDs)
		}
		chunk := runIDs[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := tx.Exec(
			"DELETE FROM run_events WHERE runId IN ("+placeholders+")", args...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete run_events chunk: %w", err)
		}
		if _, err := tx.Exec(
			"DELETE FROM audit_log WHERE runId IN ("+placeholders+")", args...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete audit_log chunk: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit prune tx: %w", err)
	}
	return nil
}
