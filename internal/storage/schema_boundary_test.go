package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// stampStaleDB writes a fresh sqlite file at path stamped with an OLDER baseline
// user_version (non-zero, < schemaUserVersion) so a subsequent Open trips the
// stale-schema branch. Returns the path for convenience.
func stampStaleDB(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStaleSchemaReturnsTypedError asserts the stale-schema failure is a typed
// *SchemaStaleError carrying the on-disk and expected versions — so the composition
// root can detect this exact case with errors.As and offer a graceful reset.
func TestStaleSchemaReturnsTypedError(t *testing.T) {
	path := stampStaleDB(t, filepath.Join(t.TempDir(), "state.db"))

	_, err := Open(path, &Options{Now: func() int64 { return 1 }})
	var stale *SchemaStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("want *SchemaStaleError, got %T: %v", err, err)
	}
	if stale.Have != 1 || stale.Want != schemaUserVersion {
		t.Fatalf("want Have=1 Want=%d, got Have=%d Want=%d", schemaUserVersion, stale.Have, stale.Want)
	}
	// The message stays actionable for the no-handler (script/host) path.
	if !strings.Contains(stale.Error(), "make db-reset") {
		t.Fatalf("typed error message must still point to 'make db-reset', got: %v", stale)
	}
}

// TestStaleOldShapedSchemaDetectedBeforeDDL is the regression guard for the
// detect-before-DDL ordering. A REAL old DB has tables at an older SHAPE, not just an
// older user_version on an empty file. Here `memories` predates the expiresAt column
// (added at v5); the current schema's `CREATE INDEX … ON memories(expiresAt)` would
// fail with "no such column" if the DDL ran first — masking the stale baseline behind
// an "exec schema" error and defeating the graceful reset (which keys on
// errors.As(err, &SchemaStaleError)). Open must read user_version FIRST and return the
// typed stale error before touching the DDL.
func TestStaleOldShapedSchemaDetectedBeforeDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// An old-shaped memories table: NO expiresAt/runId/kind/sessionId (the v5 additions).
	if _, err := raw.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY, content TEXT NOT NULL, category TEXT,
		pinnedAt INTEGER, deletedAt INTEGER, createdAt INTEGER NOT NULL, updatedAt INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path, &Options{Now: func() int64 { return 1 }})
	var stale *SchemaStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("old-shaped stale DB must surface *SchemaStaleError (not a mid-DDL error), got %T: %v", err, err)
	}
	if stale.Have != 4 {
		t.Fatalf("want Have=4, got %d", stale.Have)
	}
}

// TestResetDBWipesAndRebuilds asserts ResetDB removes the DB AND its WAL/SHM sidecars,
// is idempotent on a missing file, and that a fresh Open afterwards stamps the CURRENT
// schema version — i.e. the recovery actually clears the stale baseline.
func TestResetDBWipesAndRebuilds(t *testing.T) {
	path := stampStaleDB(t, filepath.Join(t.TempDir(), "state.db"))
	// Stand in WAL/SHM sidecars so we can prove ResetDB sweeps them too (a real WAL-mode
	// DB leaves both alongside state.db).
	sidecars := []string{path + "-wal", path + "-shm"}
	for _, p := range sidecars {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ResetDB(path); err != nil {
		t.Fatalf("ResetDB: %v", err)
	}
	for _, p := range append([]string{path}, sidecars...) {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be gone after ResetDB, stat err = %v", p, err)
		}
	}
	// Idempotent: a second reset on an already-missing file is a no-op, not an error.
	if err := ResetDB(path); err != nil {
		t.Fatalf("ResetDB (second, missing) must be a no-op, got: %v", err)
	}

	// Re-Open rebuilds the schema fresh and stamps the current version.
	s := openFile(t, path, 1)
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaUserVersion {
		t.Fatalf("after reset+reopen user_version want %d got %d", schemaUserVersion, version)
	}
}

// TestStaleSchemaVersionFailsLoudly asserts that opening a DB initialized at an older
// baseline (user_version < schemaUserVersion, non-zero) fails with an actionable
// "make db-reset" error rather than limping into a cryptic 'no such column' from the
// session-boundary sweep. Pre-release policy hard-resets; this keeps the failure
// discoverable.
func TestStaleSchemaVersionFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Stamp a fresh file with an older baseline version (driver registered via store.go).
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path, &Options{Now: func() int64 { return 1 }})
	if err == nil {
		t.Fatal("expected a stale-schema error opening a v1 baseline DB, got nil")
	}
	if !strings.Contains(err.Error(), "make db-reset") {
		t.Fatalf("stale-schema error must point to 'make db-reset', got: %v", err)
	}
}

// openFile opens a fresh file-backed store at a temp path with a frozen clock so
// the session-boundary routines (cancel stale watchers / fail stale launches /
// resolve watcher events) re-run on a second Open against the same file — which
// an in-memory store cannot exercise (a new :memory: handle is a new DB).
func openFile(t *testing.T, path string, now int64) *Store {
	t.Helper()
	s, err := Open(path, &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatalf("Open %s: %v", path, err)
	}
	return s
}

func colNames(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	rows, err := s.DB().Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

func indexNames(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	rows, err := s.DB().Query("PRAGMA index_list(" + table + ")")
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

// TestFreshSchemaShape asserts a fresh DB builds the whole current schema and
// stamps user_version == schemaUserVersion — the single-baseline contract.
func TestFreshSchemaShape(t *testing.T) {
	s := openTest(t, 1)

	mustHaveCols := map[string][]string{
		"events":            {"updatedAt", "notifiedAt"},
		"watchers":          {"isSupervisor", "lastEpistemicKind", "endedReason", "endedAt", "workflowRunId"},
		"automation_grants": {"source"},
		"audit_log":         {"grantSource", "grantId", "runId"},
		"run_events":        {"id", "runId", "seq", "ts", "type", "payload"},
		"workflow_runs": {"id", "issueNumber", "terminalIdsJson", "watcherIdsJson",
			"queueEventIdsJson", "status", "nextActionJson", "notesJson",
			"createdAt", "updatedAt", "completedAt"},
		"skill_run_state": {"id", "sessionId", "skillId", "currentStep", "stepsJson",
			"status", "startedAt", "updatedAt", "completedAt"},
		"agent_launches": {"id", "idempotencyKey", "agentId", "worktreeId", "mode",
			"title", "name", "terminalId", "watcherId", "stage", "errorCode",
			"errorMessage", "createdAt", "updatedAt", "workflowRunId"},
		"memories": {"id", "content", "category", "source", "pinnedAt", "deletedAt",
			"createdAt", "updatedAt"},
		"artifacts": {"id", "sessionId", "content", "totalChars", "totalBytes", "createdAt"},
		"context_checkpoints": {"slot", "compactionDepth", "summaryText", "lastRunId",
			"lastSeq", "payloadJson", "createdAt"},
	}
	for table, cols := range mustHaveCols {
		got := colNames(t, s, table)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("%s missing column %q", table, c)
			}
		}
	}

	mustHaveIdx := map[string][]string{
		"run_events":      {"idx_run_events_run", "idx_run_events_ts"},
		"conversation":    {"idx_conv_createdat"},
		"workflow_runs":   {"idx_workflow_runs_status"},
		"skill_run_state": {"idx_skill_run_state_key"},
		"agent_launches":  {"idx_agent_launches_key"},
		"artifacts":       {"idx_artifacts_session"},
	}
	for table, idxs := range mustHaveIdx {
		got := indexNames(t, s, table)
		for _, ix := range idxs {
			if !got[ix] {
				t.Errorf("%s missing index %q", table, ix)
			}
		}
	}

	// FTS5 virtual table + its three content-sync triggers exist.
	rows, err := s.DB().Query(
		"SELECT name FROM sqlite_master WHERE type IN ('table','trigger')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names[n] = true
	}
	for _, want := range []string{"memories_fts", "memories_ai", "memories_au", "memories_ad"} {
		if !names[want] {
			t.Errorf("missing FTS object %q", want)
		}
	}

	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaUserVersion {
		t.Fatalf("user_version want %d got %d", schemaUserVersion, version)
	}

	// A fresh grant defaults source to 'local' (SCHEMA default backfill).
	g, err := s.InsertGrant(domain.AutomationGrantRecord{
		ActorID: "wch_fresh", ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["git"]`), ExpiresAt: 9999999999999, MaxUses: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetGrant(g.ID)
	if got == nil || got.Source != domain.GrantSourceLocal {
		t.Fatalf("fresh grant source want local, got %v", got)
	}
}

// readBusyTimeout reads the per-connection busy_timeout in ms.
func readBusyTimeout(t *testing.T, s *Store) int {
	t.Helper()
	var ms int
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return ms
}

// TestBusyTimeoutPersistsAcrossReopen — busy_timeout is per-connection and must
// be re-applied on every Open, even when the file already has WAL set.
func TestBusyTimeoutPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := openFile(t, path, 1)
	if got := readBusyTimeout(t, first); got != busyTimeoutMS {
		t.Fatalf("first open busy_timeout want %d got %d", busyTimeoutMS, got)
	}
	_ = first.Close()
	second := openFile(t, path, 1)
	defer second.Close()
	if got := readBusyTimeout(t, second); got != busyTimeoutMS {
		t.Fatalf("reopen busy_timeout want %d got %d", busyTimeoutMS, got)
	}
}

// TestDueTimersWindow — DueTimers returns only scheduled timers with fireAt <= now.
func TestDueTimersWindow(t *testing.T) {
	now := int64(1_000_000)
	s := openTest(t, now)
	due, _ := s.InsertTimer(domain.TimerRecord{Title: "due", FireAt: now - 1, PayloadType: "enqueue", PayloadJson: "{}"})
	exactly, _ := s.InsertTimer(domain.TimerRecord{Title: "now", FireAt: now, PayloadType: "enqueue", PayloadJson: "{}"})
	s.InsertTimer(domain.TimerRecord{Title: "future", FireAt: now + 1, PayloadType: "enqueue", PayloadJson: "{}"})
	s.InsertTimer(domain.TimerRecord{Title: "fired", FireAt: now - 100, PayloadType: "enqueue", PayloadJson: "{}", Status: "fired"})

	got, err := s.DueTimers(now)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, tm := range got {
		ids[tm.ID] = true
		if tm.Status != "scheduled" {
			t.Fatalf("due timer not scheduled: %s", tm.Status)
		}
		if tm.FireAt > now {
			t.Fatalf("due timer fireAt in future: %d", tm.FireAt)
		}
	}
	if len(got) != 2 || !ids[due.ID] || !ids[exactly.ID] {
		t.Fatalf("want due+exactly, got %v", ids)
	}
}

// TestDueWatchersWindow — DueWatchers returns only active watchers with
// nextCheckAt <= now.
func TestDueWatchersWindow(t *testing.T) {
	now := int64(2_000_000)
	s := openTest(t, now)
	base := domain.WatcherRecord{Kind: "terminal", Title: "w", Goal: "g", TargetsJson: "[]", CadenceMs: 1000, ModelTier: domain.ModelSmall}
	mk := func(next int64, status string) domain.WatcherRecord {
		r := base
		r.NextCheckAt = next
		r.Status = status
		return r
	}
	due, _ := s.InsertWatcher(mk(now-1, ""))
	exactly, _ := s.InsertWatcher(mk(now, ""))
	s.InsertWatcher(mk(now+1, ""))
	s.InsertWatcher(mk(now-100, "paused"))

	got, err := s.DueWatchers(now)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, w := range got {
		ids[w.ID] = true
		if w.Status != "active" {
			t.Fatalf("due watcher not active: %s", w.Status)
		}
		if w.NextCheckAt > now {
			t.Fatalf("due watcher nextCheckAt in future")
		}
	}
	if len(got) != 2 || !ids[due.ID] || !ids[exactly.ID] {
		t.Fatalf("want due+exactly, got %v", ids)
	}
}

// TestBackupDBMovesStateAsideAndFreshOpenRebuilds is the stale-schema recovery
// contract: BackupDB renames the DB and its WAL/SHM sidecars to a timestamped
// backup (content intact — nothing is deleted), and a subsequent Open rebuilds a
// fresh current-version schema at the original path.
func TestBackupDBMovesStateAsideAndFreshOpenRebuilds(t *testing.T) {
	path := stampStaleDB(t, filepath.Join(t.TempDir(), "state.db"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Stand-in WAL/SHM sidecars with distinct content to prove they travel too.
	if err := os.WriteFile(path+"-wal", []byte("wal-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-shm", []byte("shm-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := BackupDB(path, 1)
	if err != nil {
		t.Fatalf("BackupDB: %v", err)
	}
	if !strings.Contains(backup, ".bak-v1-") {
		t.Fatalf("backup path should carry the old version + timestamp, got %q", backup)
	}
	// Originals gone (a fresh Open must see an empty slot)…
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should have been moved aside, stat err = %v", p, err)
		}
	}
	// …and the backup holds the ORIGINAL bytes: state was preserved, not wiped.
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("backup main file missing: %v", err)
	}
	if string(got) != string(original) {
		t.Fatal("backup main file content differs from the original DB")
	}
	if got, _ := os.ReadFile(backup + "-wal"); string(got) != "wal-bytes" {
		t.Fatalf("backup -wal content = %q, want original", got)
	}
	if got, _ := os.ReadFile(backup + "-shm"); string(got) != "shm-bytes" {
		t.Fatalf("backup -shm content = %q, want original", got)
	}

	// A fresh Open at the original path rebuilds the current schema.
	s := openFile(t, path, 1)
	defer s.Close()
	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaUserVersion {
		t.Fatalf("after backup+reopen user_version want %d got %d", schemaUserVersion, version)
	}
}

// TestBackupDBMissingFileIsNoop: nothing on disk → nothing to back up, no error.
func TestBackupDBMissingFileIsNoop(t *testing.T) {
	backup, err := BackupDB(filepath.Join(t.TempDir(), "absent.db"), 3)
	if err != nil {
		t.Fatalf("BackupDB on a missing file must be a no-op, got: %v", err)
	}
	if backup != "" {
		t.Fatalf("no file was backed up, path should be empty, got %q", backup)
	}
}

// TestBackupDBFailureLeavesFilesUntouched is the conservative-failure contract:
// when the backup rename cannot be performed, BackupDB returns the error and the
// original DB stays exactly where it was — the caller must never proceed to wipe.
func TestBackupDBFailureLeavesFilesUntouched(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	dir := t.TempDir()
	path := stampStaleDB(t, filepath.Join(dir, "state.db"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A read-only directory makes the same-dir rename fail.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	backup, err := BackupDB(path, 1)
	if err == nil {
		t.Fatalf("BackupDB must fail when the rename fails, got backup %q", backup)
	}
	_ = os.Chmod(dir, 0o755)
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("original DB must survive a failed backup: %v", rerr)
	}
	if string(got) != string(original) {
		t.Fatal("original DB content changed across a failed backup")
	}
}

// TestBackupDBDistinctNamesWithinOneSecond: two resets in the same second must not
// overwrite each other's backup. The backup directory is created EXCLUSIVELY
// (os.Mkdir), so a name collision picks a suffixed sibling instead of renaming
// over an existing backup.
func TestBackupDBDistinctNamesWithinOneSecond(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	stampStaleDB(t, path)
	first, err := BackupDB(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	stampStaleDB(t, path)
	second, err := BackupDB(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("backups must land at distinct paths, got %q and %q", first, second)
	}
	for _, p := range []string{first, second} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("backup %s missing: %v", p, err)
		}
	}
}

// failNthRename installs a backupRename hook that fails the Nth move call
// (1-based) with a synthetic error and restores os.Rename on cleanup. Rollback
// renames (made after the failure) go through the hook too, so failEverAfter
// can additionally model a wedged rollback.
func failNthRename(t *testing.T, n int, failEverAfter bool) {
	t.Helper()
	calls := 0
	failed := false
	backupRename = func(oldpath, newpath string) error {
		calls++
		if calls == n || (failed && failEverAfter) {
			failed = true
			return errors.New("synthetic rename failure")
		}
		return os.Rename(oldpath, newpath)
	}
	t.Cleanup(func() { backupRename = os.Rename })
}

// TestBackupDBWalMoveFailureRollsBackAndNeverDeletesWal is the finding-7 core:
// when the WAL's move (the SECOND rename) fails, the already-moved main DB must
// be moved BACK, the WAL must still exist with its original bytes (a WAL can
// hold the only copy of committed transactions — it is NEVER deleted), and the
// error must be returned with no backup path.
func TestBackupDBWalMoveFailureRollsBackAndNeverDeletesWal(t *testing.T) {
	dir := t.TempDir()
	path := stampStaleDB(t, filepath.Join(dir, "state.db"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("wal-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-shm", []byte("shm-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	failNthRename(t, 2, false) // 1st = main db OK, 2nd = wal fails

	backup, err := BackupDB(path, 1)
	if err == nil {
		t.Fatalf("BackupDB must fail when the WAL move fails, got backup %q", backup)
	}
	if backup != "" {
		t.Fatalf("a failed backup must not report a backup path, got %q", backup)
	}
	// The triplet is back in place, byte-identical.
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(original) {
		t.Fatalf("main db not rolled back intact (err=%v)", rerr)
	}
	if got, rerr := os.ReadFile(path + "-wal"); rerr != nil || string(got) != "wal-bytes" {
		t.Fatalf("WAL was deleted or altered on a failed backup (err=%v got=%q)", rerr, got)
	}
	if got, rerr := os.ReadFile(path + "-shm"); rerr != nil || string(got) != "shm-bytes" {
		t.Fatalf("SHM was deleted or altered on a failed backup (err=%v got=%q)", rerr, got)
	}
	if !strings.Contains(err.Error(), "restored") {
		t.Fatalf("clean-rollback error must state the files were restored, got: %v", err)
	}
}

// TestBackupDBShmMoveFailureRollsBackWholeTriplet: a THIRD-rename (shm) failure
// must move back BOTH the main db and the wal — a partial backup is never
// reported as success.
func TestBackupDBShmMoveFailureRollsBackWholeTriplet(t *testing.T) {
	dir := t.TempDir()
	path := stampStaleDB(t, filepath.Join(dir, "state.db"))
	if err := os.WriteFile(path+"-wal", []byte("wal-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-shm", []byte("shm-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	failNthRename(t, 3, false) // main + wal move, shm fails

	backup, err := BackupDB(path, 1)
	if err == nil {
		t.Fatalf("BackupDB must fail when the SHM move fails, got backup %q", backup)
	}
	for p, want := range map[string]string{path + "-wal": "wal-bytes", path + "-shm": "shm-bytes"} {
		if got, rerr := os.ReadFile(p); rerr != nil || string(got) != want {
			t.Fatalf("%s not rolled back intact (err=%v got=%q)", p, rerr, got)
		}
	}
	if _, rerr := os.Stat(path); rerr != nil {
		t.Fatalf("main db not rolled back: %v", rerr)
	}
}

// TestBackupDBFailedRollbackReportsTrueState: when a move fails AND the rollback
// cannot restore the already-moved files, the error must say the database is
// split (never claim it was left untouched), and the stranded files must still
// exist in the backup directory — nothing is deleted.
func TestBackupDBFailedRollbackReportsTrueState(t *testing.T) {
	dir := t.TempDir()
	path := stampStaleDB(t, filepath.Join(dir, "state.db"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("wal-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2nd rename (wal) fails, and every rename after it (the rollback) fails too.
	failNthRename(t, 2, true)

	backup, err := BackupDB(path, 1)
	if err == nil {
		t.Fatalf("BackupDB must fail, got backup %q", backup)
	}
	if !strings.Contains(err.Error(), "split") {
		t.Fatalf("failed-rollback error must state the split reality, got: %v", err)
	}
	// The main db is stranded in the backup dir — but it must EXIST, intact.
	matches, _ := filepath.Glob(path + ".bak-v1-*")
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup dir, got %v", matches)
	}
	got, rerr := os.ReadFile(filepath.Join(matches[0], filepath.Base(path)))
	if rerr != nil || string(got) != string(original) {
		t.Fatalf("stranded main db missing or altered (err=%v)", rerr)
	}
	// The WAL never moved and was never deleted.
	if got, rerr := os.ReadFile(path + "-wal"); rerr != nil || string(got) != "wal-bytes" {
		t.Fatalf("WAL deleted/altered during failed rollback (err=%v got=%q)", rerr, got)
	}
}
