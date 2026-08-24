// Package storage is the durable SQLite store of the assistant: timers, watchers,
// the attention-queue inbox (events), the tool-dispatch audit trail, per-run event
// logs, the conversation transcript, automation
// grants, the workflow ledger, agent-launch sagas, runbook run state, and
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
// the supervisor daemon (or the next attached session) can adopt them. The explicit
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
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
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
	// (named as it actually was — this line is a statement about the past, and the
	// skill -> runbook rename came later)
	// (runbook selection is server-owned now — the CLI never logged selections); to 8 when
	// conversation gained reasoningContent (persist DeepSeek thinking-mode chain-of-thought
	// for verbatim replay); to 9 when the runtime_state key/value table was added (the
	// persistent-supervisor handoff surface: current session id + backend state token
	// survive a process boundary); to 10 when the workflow-intelligence graph layer
	// landed (workflow_graphs + workflow_events + workflow_resource_links +
	// workflow_reconcile_runs); to 11 when conversation gained `name` (the reserved
	// `daintree_compaction` marker on a server-delivered compacted context block, which
	// has to survive a restart or the next request re-sends history the server already
	// froze); to 12 when `skill_run_state` was renamed to `runbook_run_state` (with its
	// `skillId` column and unique index) for protocol 3 — the rename is why the bump is
	// REQUIRED rather than cosmetic: the schema is all `CREATE TABLE IF NOT EXISTS`, so an
	// existing file left at 11 would keep the old table, never create the new one, and
	// every step-advance would fail against a table that is present but not the one the
	// code now names — a schema change is a hard-reset (make db-reset), not a migration.
	schemaUserVersion = 12
)

// SchemaVersion exposes the on-disk schema baseline to callers that need to REPORT it
// rather than act on it — the generated compatibility manifest, `doctor`, and the
// support bundle. Kept as an accessor over the unexported constant so nothing outside
// this package can compare against a copy that has drifted.
func SchemaVersion() int { return schemaUserVersion }

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

	// busy_timeout only, here: it is connection-scoped and covers the user_version
	// read below against a momentarily locked WAL file. The pragmas that DO rewrite
	// the file's on-disk format (journal_mode's DELETE→WAL transition) wait until
	// applySchema has confirmed the version gate — a stale or too-new database gets
	// no DDL and no schema-level write from THIS code, rather than being refused a
	// DDL exec after already having had its journal mode flipped underneath it. This
	// is a guarantee about application-level writes, not an absolute one about every
	// byte SQLite's own connection/pager machinery might touch (a hot-journal
	// recovery, or a WAL checkpoint on close) — those are the engine's own format
	// housekeeping, not the schema-write hazard this gate exists to prevent.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pragma busy_timeout: %w", err)
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

// OpenReadOnly opens an EXISTING database for querying only — no schema exec, no
// retention sweep, no journal-mode change.
//
// It exists because `Open` is not a read: it applies PRAGMAs (including `journal_mode =
// WAL`, which rewrites the file header), execs the schema, and runs a retention sweep
// that DELETES rows. A diagnostic that wants to look at the audit trail — the support
// bundle — would therefore mutate a database whose owner lease it does not hold, racing
// a live attached session or the daemon. A tool that changes the thing it is diagnosing is not a
// diagnostic.
//
// `mode=ro` makes that structural rather than a matter of discipline: SQLite itself
// refuses any write on this handle, so a future caller cannot accidentally reintroduce
// one. A missing file is an error here rather than a fresh database — "there is nothing
// to read" is the honest answer, not an empty file the caller did not ask for.
func OpenReadOnly(dbPath string) (*Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: domain.NowMS, retention: DefaultRetention}
	// busy_timeout only. Everything else Open applies is a WRITE.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMS)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return s, nil
}

// applyWriteOrientedPragmas runs the two connection PRAGMAs that mutate the file
// (journal_mode's DELETE→WAL transition rewrites the header) or would otherwise be
// pointless to apply before knowing whether this open is even going to proceed.
// Called from applySchema, AFTER the version gate — see its comment for why.
func (s *Store) applyWriteOrientedPragmas() error {
	stmts := []string{
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
// OR NEWER baseline is rejected with a typed error BEFORE any DDL/write can touch it.
// For the older case that matters because the current-shape DDL can trip on the old
// table shape (e.g. a pre-v5 `memories` table lacks `expiresAt`, so `CREATE INDEX …
// ON memories(expiresAt)` in schema.sql would fail with a cryptic "no such column" —
// masking the stale baseline and defeating the caller's graceful reset, which keys on
// errors.As(err, &SchemaStaleError)). For the newer case it matters more: an older
// binary does not know what a newer schema changed, and "harmless" IF NOT EXISTS DDL
// is not the risk — the WRITES this binary would go on to make are (wrong invariants,
// misread enum/status values, columns the newer code requires that this binary never
// populates). Duplicate binaries on PATH silently selecting an older build is an
// explicitly recognized failure mode in this project, which is exactly the scenario
// that puts an old binary in front of a database a newer one already upgraded. Order:
//   - v in (0, schemaUserVersion): stale baseline → SchemaStaleError, NO DDL run.
//   - v == 0: brand-new file → run the schema DDL (builds the whole shape), then stamp.
//   - v > schemaUserVersion: newer baseline → SchemaTooNewError, NO DDL run, no
//     schema-level write of any kind from this code.
//   - v == schemaUserVersion: current → run the idempotent IF NOT EXISTS DDL as a
//     no-op safety net; leave the version untouched.
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
	// A version above the current baseline means a NEWER binary already upgraded this
	// file — reject it before any DDL or write, the same way, for the opposite reason:
	// there is nothing wrong with the file, everything wrong with running old code
	// against it.
	if v > schemaUserVersion {
		return &SchemaTooNewError{Have: v, MaxSupported: schemaUserVersion}
	}
	// The gate passed: only now is it safe to apply the pragmas that rewrite the
	// file (journal_mode chief among them) and run the DDL.
	if err := s.applyWriteOrientedPragmas(); err != nil {
		return err
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

// SchemaTooNewError reports that the on-disk DB was initialized at a NEWER schema
// baseline than this build understands (Have > MaxSupported). Unlike a stale file,
// this is never something to reset — the file is not behind, this binary is: a
// duplicate older copy on PATH (an explicitly recognized failure mode in this
// project) opening a database a newer install already upgraded. There is no
// graceful recovery to offer here, only a refusal: update this binary. It is a
// TYPED error, not a bare string, so a caller (doctor, the host's boot path) can
// name the exact condition rather than pattern-matching a driver error string.
type SchemaTooNewError struct {
	Have         int // the user_version stamped on the file
	MaxSupported int // schemaUserVersion this build understands
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("database schema (version %d) is newer than this binary understands (max %d) — "+
		"update daintree-assistant; if multiple copies are on PATH, `doctor` lists them", e.Have, e.MaxSupported)
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

// backupRename is the rename primitive BackupDB moves files with. A var so the
// fault-injection tests can fail the second/third move and prove the rollback
// contract; production always uses os.Rename.
var backupRename = os.Rename

// BackupDB moves the SQLite database at dbPath — together with its -wal/-shm
// sidecars — into a freshly-created backup DIRECTORY alongside it
// (<db>.bak-v<oldVersion>-<unixts>/) and returns the backup's main-file path
// inside it. It is the non-destructive form of ResetDB for the stale-schema
// recovery: same-directory renames are atomic and cheap, the next Open rebuilds
// a fresh schema at dbPath, and the user's previous state (timers, watchers,
// memories, history) stays recoverable on disk instead of being silently wiped.
//
// The backup directory is created EXCLUSIVELY (os.Mkdir, retrying suffixed names
// on EEXIST), so a concurrent backup can never be overwritten — os.Rename onto an
// existing destination would silently replace it, which is why the old
// stat-then-rename naming was not collision-safe.
//
// Failure posture: all-or-nothing. The db → wal → shm triplet moves as one unit;
// if ANY move fails, everything already moved is moved BACK (and the directory
// removed), so the caller sees either a complete backup or the original files in
// place — a WAL is NEVER deleted (committed transactions can live only in the
// WAL; dropping it would corrupt the backup). Success (and the backup path) is
// reported only when the whole triplet moved. If the rollback itself also fails,
// the returned error states exactly which files remain where. A missing main
// file returns ("", nil): nothing to back up.
func BackupDB(dbPath string, oldVersion int) (string, error) {
	// Exclusively create the backup directory; suffix on collision.
	base := fmt.Sprintf("%s.bak-v%d-%d", dbPath, oldVersion, time.Now().Unix())
	dir := base
	for i := 2; ; i++ {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("backup database: create backup dir %s: %w", dir, err)
		}
		if i >= 100 {
			return "", fmt.Errorf("backup database: create backup dir %s: too many existing backups", base)
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}

	// Move the triplet; remember every completed move for rollback.
	type moved struct{ src, dst string }
	var done []moved
	rollback := func() error {
		var stuck []string
		for i := len(done) - 1; i >= 0; i-- {
			if rerr := backupRename(done[i].dst, done[i].src); rerr != nil {
				stuck = append(stuck, fmt.Sprintf("%s could not be moved back from %s: %v",
					done[i].src, done[i].dst, rerr))
			}
		}
		if len(stuck) == 0 {
			_ = os.Remove(dir) // empty now; best-effort
			return nil
		}
		return errors.New(strings.Join(stuck, "; "))
	}

	for i, src := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		dst := filepath.Join(dir, filepath.Base(src))
		err := backupRename(src, dst)
		if err == nil {
			done = append(done, moved{src: src, dst: dst})
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			// Main file absent (i==0): nothing to back up at all. An absent sidecar
			// is normal (no -wal/-shm after a clean close) — skip it.
			if i == 0 {
				_ = os.Remove(dir)
				return "", nil
			}
			continue
		}
		// A move failed mid-triplet: restore what already moved. NEVER delete the
		// stranded file — a WAL can hold the only copy of committed transactions.
		if rberr := rollback(); rberr != nil {
			return "", fmt.Errorf(
				"backup database: move %s failed (%v) AND rolling back already-moved files failed — "+
					"the database is split between %s and its backup dir %s: %w",
				src, err, filepath.Dir(dbPath), dir, rberr)
		}
		return "", fmt.Errorf("backup database: move %s: %w (all files restored to their original location)", src, err)
	}
	return filepath.Join(dir, filepath.Base(dbPath)), nil
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
// just acquired the project owner lock (attached session, REPL, one-shot, or the
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
	watchers, err := s.ListLiveWatchers()
	if err != nil {
		return sum, fmt.Errorf("ownership boot: list live watchers: %w", err)
	}
	for _, w := range watchers {
		sum.ResumedWatcherTitles = append(sum.ResumedWatcherTitles, w.Title)
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
