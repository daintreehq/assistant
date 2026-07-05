// Package storage is the durable SQLite store of the assistant: timers, watchers,
// the attention-queue inbox (events), the tool-dispatch audit trail, per-run event
// logs, the conversation transcript, automation
// grants, the workflow ledger, agent-launch sagas, skill run state, and
// cross-session project memories (with an FTS5 recall index).
//
// Uses modernc.org/sqlite (pure Go, CGO-free) directly. The store is
// single-writer (SetMaxOpenConns(1)) to preserve the no-interleave assumption the
// "atomic" grant consume / event upsert / resolve rely on. Construction is a
// session boundary: stale watchers cancelled, the dead spawn roster cleared,
// retention swept.
package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
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
	// and agent_launches gained workflowRunId (durable workflow-ledger back-links); to 4
	// when the artifacts table was added (durable tool-result overflow payloads); to 5
	// when memories gained expiresAt/runId/kind/sessionId (TTL + provenance + episodic);
	// to 6 when the context_checkpoints table was added (durable compaction checkpoint
	// reloaded on resume); to 7 when the dead skill_selection_log table was DROPPED
	// (skill selection is server-owned now — the CLI never logged selections); to 8 when
	// conversation gained reasoningContent (persist DeepSeek thinking-mode chain-of-thought
	// for verbatim replay) — a schema change is a hard-reset (make db-reset), not a migration.
	schemaUserVersion = 8
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
	ArtifactsMaxAge      time.Duration // hard-delete overflow artifacts past window
	ArtifactsKeepRows    int
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
	// Artifacts age WITH the conversation that references them (90d / 1000 rows): a
	// truncation stub persisted in the transcript must not outlive its payload, or
	// artifact.read would 404 a stub the user can still scroll to.
	ArtifactsMaxAge:    time.Duration(90*dayMS) * time.Millisecond,
	ArtifactsKeepRows:  1000,
	EventsTerminalAge:  time.Duration(7*dayMS) * time.Millisecond,
	MemoriesDeletedAge: time.Duration(30*dayMS) * time.Millisecond,
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
// schema, then runs the session-boundary routines: cancel stale watchers, reset the
// dead spawn roster, and a best-effort retention sweep. Pass ":memory:" for an
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
	if err := s.applySchema(); err != nil {
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
	if err := s.resetStaleAgentLaunches(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reset stale agent launches: %w", err)
	}
	// Async futures are session-scoped like watchers: abandon every non-terminal
	// invocation a prior session left behind (the foreground coordinator that owned
	// them is gone; a new session must never silently resume them).
	if err := s.cancelStaleAsyncInvocations(now); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Fresh-start inbox: resolve EVERY open attention event from prior sessions so a
	// new run begins with an empty inbox (the !N badge at 0) — supervision and its
	// notifications never carry over. Best-effort; events published THIS session
	// (after Open returns) are unaffected.
	_, _ = s.ResolveAllOpenEvents(now)
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

// applySchema keys off PRAGMA user_version. The schema's IF NOT EXISTS DDL builds a
// fresh file's whole current shape, so there is no migration chain. Dev policy
// hard-resets on a schema change rather than chaining.
//
// CRUCIALLY, the version is read FIRST — before any DDL runs — so a file at an OLDER
// baseline is rejected with a typed *SchemaStaleError BEFORE the current-shape DDL can
// trip on the old table shape. (e.g. a pre-v5 `memories` table lacks `expiresAt`, so
// `CREATE INDEX … ON memories(expiresAt)` in schema.sql would fail with a cryptic "no
// such column" — masking the stale baseline and defeating the caller's graceful reset,
// which keys on errors.As(err, &SchemaStaleError).) Order:
//   - v in (0, schemaUserVersion): stale baseline → SchemaStaleError, NO DDL run.
//   - v == 0: brand-new file → run the schema DDL (builds the whole shape), then stamp.
//   - v >= schemaUserVersion: current (or newer, opened by older code) → run the
//     idempotent IF NOT EXISTS DDL as a no-op safety net; leave the version untouched.
func (s *Store) applySchema() error {
	v, err := s.userVersion()
	if err != nil {
		return err
	}
	// A non-zero version below the current baseline is a stale file: reject it BEFORE the
	// DDL so the loud, typed error (not a mid-DDL "no such column") reaches the caller.
	if v != 0 && v < schemaUserVersion {
		return &SchemaStaleError{Have: v, Want: schemaUserVersion}
	}
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("exec schema: %w", err)
	}
	// v == 0 is a brand-new file: the CREATE TABLE DDL just built the current shape, so
	// stamp the version. PRAGMA user_version doesn't accept bound params.
	if v == 0 {
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaUserVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	}
	return nil
}

// userVersion reads PRAGMA user_version (0 for a brand-new file).
func (s *Store) userVersion() (int, error) {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// SchemaStaleError reports that the on-disk DB was initialized at an OLDER schema
// baseline than this build expects (Have < Want, both non-zero). Pre-release policy
// hard-resets rather than chaining migrations, so the only recovery is a
// wipe-and-rebuild (ResetDB). It is a TYPED error — not a bare string — so a caller
// can detect this exact case with errors.As and offer to reset the DB (e.g. an
// interactive y/N prompt) instead of dumping the raw "run make db-reset" message.
type SchemaStaleError struct {
	Have int // the user_version stamped on the file
	Want int // schemaUserVersion this build builds
}

func (e *SchemaStaleError) Error() string {
	// Keep the actionable dev message verbatim: this is what surfaces when no caller
	// offers a graceful reset (scripts, the host, a non-TTY launch).
	return fmt.Sprintf("database schema is stale (version %d, current %d) — run 'make db-reset' to reset it (honours DAINTREE_ASSISTANT_STATE_DIR)", e.Have, e.Want)
}

// ResetDB deletes the SQLite database at dbPath together with its WAL/SHM sidecar
// files, so the next Open rebuilds the schema from scratch. This is the programmatic
// twin of `make db-reset` for the one recovery the pre-release policy supports — a
// stale schema baseline (see SchemaStaleError). It removes ONLY the three DB files,
// never the enclosing state dir, so anything else living there survives. A missing
// file is not an error (idempotent). The handle must be closed first — a failed Open
// already closes it before returning the SchemaStaleError, so the typical caller can
// reset and re-Open immediately.
func ResetDB(dbPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
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
