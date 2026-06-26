package storage

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
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
