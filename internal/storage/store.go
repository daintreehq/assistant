// Package storage is the durable SQLite store of the assistant: timers, watchers,
// the attention-queue inbox (events), the tool-dispatch audit trail, per-run event
// logs, the conversation transcript, automation
// grants, the workflow ledger, agent-launch sagas, skill run state, and
// cross-session project memories (with an FTS5 recall index).
//
// Uses modernc.org/sqlite (pure Go, CGO-free) directly. The store is
// single-writer (SetMaxOpenConns(1)) to preserve the no-interleave assumption the
// "atomic" grant consume / event upsert / resolve rely on. Cross-PROCESS
// exclusivity is NOT the store's job: the ipc owner lock guarantees at most one
// process holds an open Store per project DB (see docs/SUPERVISOR.md).
//
// Construction is deliberately NON-destructive: watchers, async invocations,
// and the attention inbox are PROJECT-scoped and survive process boundaries so
// the supervisor daemon (or the next cockpit) can adopt them. The explicit
// owner-boot reconciliation lives in BeginOwnership; the only wholesale
// teardown left is /clear (CancelLiveWatchers / CancelLiveAsyncInvocations /
// ResolveAllOpenEvents).
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
	// for verbatim replay); to 9 when the runtime_state key/value table was added (the
	// persistent-supervisor handoff surface: current session id + backend state token
	// survive a process boundary); to 10 when the workflow-intelligence graph layer
	// landed (workflow_graphs + workflow_events + workflow_resource_links +
	// workflow_reconcile_runs) — a schema change is a hard-reset (make db-reset),
	// not a migration.
	schemaUserVersion = 10
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
}

// Open opens (or creates) the SQLite file at dbPath, applies PRAGMAs, execs the
// schema, and runs a best-effort retention sweep. It NEVER tears down live
// supervision state — watchers, async invocations, and the inbox are
// project-scoped and adopted by the next owner (see BeginOwnership). Pass
// ":memory:" for an in-memory store in tests.
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

	// Best-effort housekeeping: a sweep failure must NEVER abort construction.
	// Swallow the error here. (The old session-boundary teardown — cancel live
	// watchers/async, wipe the inbox — is GONE from Open: supervision state is
	// project-scoped now and adopted by the owner, not the act of opening the file.)
	_ = s.GCRetentionSweep(s.now())

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
//
// Prefer BackupDB for the stale-schema recovery path: it moves the files aside
// (recoverable) instead of destroying the user's timers/watchers/memories/history.
func ResetDB(dbPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// BackupDB moves the SQLite database at dbPath — together with its -wal/-shm
// sidecars — to a timestamped backup alongside it
// (<db>.bak-v<oldVersion>-<unixts>) and returns the backup's main-file path. It
// is the non-destructive form of ResetDB for the stale-schema recovery: a rename
// is atomic and cheap (same directory), the next Open rebuilds a fresh schema at
// dbPath, and the user's previous state (timers, watchers, memories, history)
// stays recoverable on disk instead of being silently wiped.
//
// Failure posture is conservative: if the MAIN file's rename fails, nothing has
// moved and the error is returned — the caller must NOT go on to delete anything.
// Sidecars are moved best-effort AFTER the main file; one that can't be renamed
// is removed instead (a stale -wal/-shm left beside a fresh db would poison it),
// and only an un-removable sidecar surfaces an error. A missing main file returns
// ("", nil): nothing to back up.
func BackupDB(dbPath string, oldVersion int) (string, error) {
	backup := fmt.Sprintf("%s.bak-v%d-%d", dbPath, oldVersion, time.Now().Unix())
	// Never overwrite an existing backup (two resets inside one second): pick a
	// distinct suffixed name instead. Bounded — on a weird stat failure the rename
	// below surfaces the real error.
	for i := 2; i < 100; i++ {
		if _, err := os.Stat(backup); err != nil {
			break
		}
		backup = fmt.Sprintf("%s.bak-v%d-%d-%d", dbPath, oldVersion, time.Now().Unix(), i)
	}
	if err := os.Rename(dbPath, backup); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("backup database %s: %w", dbPath, err)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if err := os.Rename(dbPath+ext, backup+ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			if rerr := os.Remove(dbPath + ext); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return backup, fmt.Errorf("backup database: clear sidecar %s: %w", dbPath+ext, rerr)
			}
		}
	}
	return backup, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the raw handle (escape hatch, mainly for tests). Port of raw().
func (s *Store) DB() *sql.DB { return s.db }

// OwnershipSummary reports what BeginOwnership found waiting when this process
// took over the project DB — the raw material for the "while you were away /
// resumed supervision" surfaces.
type OwnershipSummary struct {
	// ResumedWatcherTitles are the live (active/created/paused) watchers adopted
	// from a prior owner, oldest first.
	ResumedWatcherTitles []string
	// ResumedAsyncCount is how many non-terminal async invocations were adopted.
	ResumedAsyncCount int
	// UnpublishedAsyncCount is how many async invocations a prior owner finalized
	// but died before publishing — the coordinator retries those publishes.
	UnpublishedAsyncCount int
	// OpenAttentionCount is the unresolved inbox carried over.
	OpenAttentionCount int
}

// BeginOwnership is the owner-boot reconciliation, run ONCE by the process that
// just acquired the project owner lock (cockpit, REPL, one-shot, or the
// supervisor daemon). It replaces the old destructive Open-time sweep: instead
// of cancelling live supervision it ADOPTS it — the only cleanup is the spawn
// roster (in-flight launch sagas are dead with the process that ran them; a
// confirmed launch with a bound terminal survives because Daintree owns that
// terminal). Everything else is a read: the summary tells the caller what it
// just inherited.
func (s *Store) BeginOwnership(now int64) (OwnershipSummary, error) {
	var sum OwnershipSummary
	if err := s.resetStaleAgentLaunches(); err != nil {
		return sum, err
	}
	watchers, err := s.ListWatchers("")
	if err != nil {
		return sum, fmt.Errorf("ownership boot: list watchers: %w", err)
	}
	for _, w := range watchers {
		switch w.Status {
		case "active", "created", "paused":
			sum.ResumedWatcherTitles = append(sum.ResumedWatcherTitles, w.Title)
		}
	}
	live, err := s.ListLiveAsyncInvocations()
	if err != nil {
		return sum, fmt.Errorf("ownership boot: list live async: %w", err)
	}
	sum.ResumedAsyncCount = len(live)
	unpub, err := s.ListUnpublishedAsyncInvocations()
	if err != nil {
		return sum, fmt.Errorf("ownership boot: list unpublished async: %w", err)
	}
	sum.UnpublishedAsyncCount = len(unpub)
	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		return sum, fmt.Errorf("ownership boot: list open events: %w", err)
	}
	sum.OpenAttentionCount = len(open)
	return sum, nil
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
