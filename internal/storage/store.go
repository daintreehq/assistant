// Package storage is the durable SQLite store of the assistant: timers, watchers,
// the attention-queue inbox (events), the tool-dispatch audit trail, per-run event
// logs, the conversation transcript, skill-selection diagnostics, automation
// grants, the workflow ledger, agent-launch sagas, skill run state, and
// cross-session project memories (with an FTS5 recall index).
//
// Uses modernc.org/sqlite (pure Go, CGO-free) directly. The store is
// single-writer (SetMaxOpenConns(1)) to preserve the no-interleave assumption the
// "atomic" grant consume / event upsert / resolve rely on. Construction is a
// session boundary: stale watchers cancelled, stale agent-launch sagas failed,
// retention swept.
package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	// dayMS is one day in epoch milliseconds.
	dayMS int64 = 86_400_000
	// busyTimeoutMS is the lock-retry budget for a single-writer local CLI; set
	// FIRST so it covers the WAL transition's write lock.
	busyTimeoutMS = 5000
	// schedulerTickMS is the supervisor cadence floor.
	schedulerTickMS int = 3000
	// pruneChunk caps run-id deletes per batch under SQLITE_MAX_VARIABLE_NUMBER 999.
	pruneChunk = 900
	// schemaUserVersion is the single baseline; dev hard-resets rather than chain.
	// Bumped to 2 when the watchers table gained endedReason/endedAt; to 3 when watchers
	// and agent_launches gained workflowRunId (durable workflow-ledger back-links) — a
	// schema change is a hard-reset (make db-reset), not a migration.
	schemaUserVersion = 3
)

// Retention bounds the append-only tables. Each plain log table keeps the newer
// of MaxAge OR the last KeepN rows.
type Retention struct {
	AuditLogMaxAge       time.Duration
	AuditLogKeepRows     int
	RunEventsMaxAge      time.Duration
	RunEventsKeepRuns    int
	ConversationMaxAge   time.Duration
	ConversationKeepRows int
	SkillSelLogMaxAge    time.Duration
	SkillSelLogKeepRows  int
	EventsTerminalAge    time.Duration // hard-delete resolved/expired events past window
	MemoriesDeletedAge   time.Duration // hard-delete soft-deleted memories past undo window
}

// DefaultRetention is the default retention policy.
var DefaultRetention = Retention{
	AuditLogMaxAge:       time.Duration(30*dayMS) * time.Millisecond,
	AuditLogKeepRows:     5000,
	RunEventsMaxAge:      time.Duration(14*dayMS) * time.Millisecond,
	RunEventsKeepRuns:    500,
	ConversationMaxAge:   time.Duration(90*dayMS) * time.Millisecond,
	ConversationKeepRows: 1000,
	SkillSelLogMaxAge:    time.Duration(30*dayMS) * time.Millisecond,
	SkillSelLogKeepRows:  500,
	EventsTerminalAge:    time.Duration(7*dayMS) * time.Millisecond,
	MemoriesDeletedAge:   time.Duration(30*dayMS) * time.Millisecond,
}

// Options configures a Store. Both fields are test seams: pin the sweep clock and
// shrink retention windows.
type Options struct {
	// Now returns "current" epoch-ms; nil ⇒ domain.NowMS.
	Now func() int64
	// Retention overrides DefaultRetention when non-nil.
	Retention *Retention
}

// Store is the concrete persistence layer.
type Store struct {
	db        *sql.DB
	now       func() int64
	retention Retention

	// sessionEndedWatchers holds the titles of watchers cancelStaleWatchers cancelled
	// on THIS Open because the prior session ended (nil when none). It is a one-shot
	// carryover: the composition root reads it once to surface a single "these stopped
	// when the last session ended" NOTE. Set only during Open; never mutated after.
	sessionEndedWatchers []string
}

// Open opens (or creates) the SQLite file at dbPath, applies PRAGMAs, execs the
// schema, then runs the session-boundary routines: cancel stale watchers, fail
// stale agent launches, and a best-effort retention sweep. Pass ":memory:" for an
// in-memory store in tests.
func Open(dbPath string, opts *Options) (*Store, error) {
	nowFn := domain.NowMS
	ret := DefaultRetention
	if opts != nil {
		if opts.Now != nil {
			nowFn = opts.Now
		}
		if opts.Retention != nil {
			ret = *opts.Retention
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single connection preserves the no-interleave guarantee that "atomic"
	// check-and-decrement (grants) / check-and-set (events, resolve) rely on;
	// Go's pool would otherwise let goroutines interleave.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: nowFn, retention: ret}

	if err := s.applyPragmas(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("exec schema: %w", err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}

	now := s.now()
	endedTitles, err := s.cancelStaleWatchers(now)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cancel stale watchers: %w", err)
	}
	s.sessionEndedWatchers = endedTitles
	if err := s.cancelStaleAgentLaunches(now); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cancel stale agent launches: %w", err)
	}
	// Best-effort housekeeping: a sweep failure must NEVER abort construction.
	// Swallow the error here.
	_ = s.GCRetentionSweep(now)

	return s, nil
}

// applyPragmas runs the three connection PRAGMAs in order; busy_timeout first so
// it covers the WAL write lock on a fresh file.
func (s *Store) applyPragmas() error {
	stmts := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS),
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range stmts {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	return nil
}

// migrate keys off PRAGMA user_version. The schema's IF NOT EXISTS DDL builds a
// fresh file's whole current shape, so there is no migration chain. Dev policy
// hard-resets on a schema change rather than chaining — so a file initialized at an
// OLDER baseline (its tables lack newly-added columns) is failed LOUDLY here with the
// fix, instead of limping into a cryptic "no such column" from the very next
// session-boundary statement.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v >= schemaUserVersion {
		// Current — or a newer DB opened by older code; leave forward-compat untouched.
		return nil
	}
	// v == 0 is a brand-new file: the CREATE TABLE DDL just built the current shape,
	// so we only stamp the version. Any v in (0, schemaUserVersion) is a stale baseline.
	if v != 0 {
		return fmt.Errorf("database schema is stale (version %d, current %d) — run 'make db-reset' to reset it (honours DAINTREE_ASSISTANT_STATE_DIR)", v, schemaUserVersion)
	}
	// PRAGMA user_version doesn't accept bound params.
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaUserVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the raw handle (escape hatch, mainly for tests). Port of raw().
func (s *Store) DB() *sql.DB { return s.db }

// SessionEndedWatchers returns the titles of watchers that THIS Open cancelled
// because the prior session ended (nil when none). Read once by the composition
// root to surface a one-time NOTE; returns a defensive copy so the caller can't
// mutate the store's slice.
func (s *Store) SessionEndedWatchers() []string {
	if len(s.sessionEndedWatchers) == 0 {
		return nil
	}
	out := make([]string, len(s.sessionEndedWatchers))
	copy(out, s.sessionEndedWatchers)
	return out
}

// ---- ports.Store seam (the minimum the agent loop compiles against) ----

// AppendRunEvent satisfies ports.Store; it is InsertRunEvent ignoring the record.
func (s *Store) AppendRunEvent(_ context.Context, ev domain.RunEventRecord) error {
	_, err := s.InsertRunEvent(ev)
	return err
}

// AppendAudit satisfies ports.Store; returns the inserted audit id.
func (s *Store) AppendAudit(_ context.Context, rec domain.AuditRecord) (string, error) {
	out, err := s.InsertAudit(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}
