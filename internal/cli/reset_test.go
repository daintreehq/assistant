package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/ipc"
)

// resetFixture builds a populated state dir and the config that describes it.
func resetFixture(t *testing.T) (config.AppConfig, string) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "proj-abc123")
	if err := os.MkdirAll(filepath.Join(stateDir, "artifacts"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	dbPath := filepath.Join(stateDir, "state.db")
	write(dbPath)
	write(dbPath + "-wal")
	write(dbPath + "-shm")
	write(filepath.Join(stateDir, ipc.OwnerLockName))
	write(filepath.Join(stateDir, ipc.DaemonLockName))
	write(filepath.Join(stateDir, "artifacts", "art_1.json"))

	return config.AppConfig{
		StateDir: stateDir,
		DBPath:   dbPath,
	}, root
}

func paths(targets []resetTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Path)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func TestResetNeverRemovesTheLeaseFilesItHolds(t *testing.T) {
	cfg, _ := resetFixture(t)

	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	got := paths(targets)
	for _, lock := range []string{ipc.OwnerLockName, ipc.DaemonLockName} {
		if contains(got, filepath.Join(cfg.StateDir, lock)) {
			t.Errorf("%s must not be removed — unlinking a held lock file breaks single-owner", lock)
		}
	}
}

// A directory sweep, not an allowlist: anything new written into the state dir is
// cleaned by default rather than silently surviving every reset because nobody
// remembered to add its filename to a list.
func TestResetProjectStateSweepsUnknownFiles(t *testing.T) {
	cfg, _ := resetFixture(t)
	stray := filepath.Join(cfg.StateDir, "some-future-cache.bin")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	if !contains(paths(targets), stray) {
		t.Errorf("a file nobody anticipated must still be swept: %v", paths(targets))
	}
}

func TestResetAllDataRemovesContentsButNeverTheHeldLease(t *testing.T) {
	cfg, _ := resetFixture(t)

	targets, err := resetTargets(cfg, ScopeAllData)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	got := paths(targets)
	if contains(got, cfg.StateDir) {
		t.Errorf("all-data must not remove the state DIRECTORY — the held lease lives in it: %v", got)
	}
	for _, lock := range []string{ipc.OwnerLockName, ipc.DaemonLockName} {
		if contains(got, filepath.Join(cfg.StateDir, lock)) {
			t.Errorf("all-data must not remove %s: %v", lock, got)
		}
	}
	if !contains(got, cfg.DBPath) {
		t.Errorf("all-data must remove the database, got %v", got)
	}
}

func TestResetRefusesAnUnsafeStateDirectory(t *testing.T) {
	unsafe := []string{
		"", "   ", "/", ".", string(filepath.Separator),
		"relative/path",           // resolves inside the bound project
		"./also-relative",         //
		"/tmp",                    // one segment from the root: far too broad
		"/Users",                  //
		"/nonexistent-root-thing", //
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		unsafe = append(unsafe, home, home+string(filepath.Separator))
	}
	for _, dir := range unsafe {
		cfg := config.AppConfig{StateDir: dir, DBPath: filepath.Join(dir, "state.db")}
		if _, err := resetTargets(cfg, ScopeProjectState); err == nil {
			t.Errorf("resetTargets accepted an unsafe state dir %q", dir)
		}
	}
}

// THE one that matters most. DAINTREE_ASSISTANT_STATE_DIR is an env var a developer sets
// by hand, so `DAINTREE_ASSISTANT_STATE_DIR="$PWD" make db-reset` is one shell-history
// entry away — and the sweep would have enumerated .git, go.mod, and every source file
// for deletion while the confirmation printed "your code is untouched". Path SHAPE cannot
// catch this: the path is perfectly well-formed. Only provenance can.
func TestResetRefusesADirectoryThatIsNotAStateDir(t *testing.T) {
	repo := t.TempDir()
	for _, f := range []string{"go.mod", "Makefile", "README.md"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := config.AppConfig{StateDir: repo, DBPath: filepath.Join(repo, "state.db")}
	_, err := resetTargets(cfg, ScopeProjectState)
	if err == nil {
		t.Fatal("resetTargets accepted a source directory — this would delete somebody's repository")
	}
	if !strings.Contains(err.Error(), "does not look like") {
		t.Errorf("the refusal should explain WHY and name the env var: %v", err)
	}
}

// The converse: a real state dir must still work, recognised by the artefacts only this
// CLI writes. An unused (empty) state dir is fine too — there is nothing to lose.
func TestResetAcceptsARealStateDir(t *testing.T) {
	cfg, _ := resetFixture(t)
	if _, err := resetTargets(cfg, ScopeProjectState); err != nil {
		t.Errorf("a populated state dir was refused: %v", err)
	}

	empty := t.TempDir()
	emptyCfg := config.AppConfig{StateDir: empty, DBPath: filepath.Join(empty, "state.db")}
	if _, err := resetTargets(emptyCfg, ScopeProjectState); err != nil {
		t.Errorf("an unused state dir was refused: %v", err)
	}
}

// The project directory itself, and any ancestor of it, must be refused even when it
// somehow looks like a state dir — the promise printed on screen is that code is safe.
func TestResetRefusesTheProjectDirectoryAndItsAncestors(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "myproject")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	// Give both candidates state-dir provenance so ONLY the relationship check can refuse.
	for _, d := range []string{root, project} {
		if err := os.WriteFile(filepath.Join(d, "state.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, dir := range []string{project, root} {
		cfg := config.AppConfig{
			StateDir:    dir,
			DBPath:      filepath.Join(dir, "state.db"),
			ProjectPath: project,
		}
		if _, err := resetTargets(cfg, ScopeProjectState); err == nil {
			t.Errorf("resetTargets accepted %q, which is or contains the project directory", dir)
		}
	}
}

// RemoveAll on a symlinked directory removes the LINK, so a reset would report success
// while deleting nothing and every later launch would recreate state somewhere
// unexpected. Refusing is the only honest outcome.
func TestResetRefusesASymlinkedStateDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-state")
	link := filepath.Join(root, "linked-state")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := config.AppConfig{StateDir: link, DBPath: filepath.Join(link, "state.db")}
	if _, err := resetTargets(cfg, ScopeProjectState); err == nil {
		t.Error("resetTargets accepted a symlinked state dir")
	}
}

// Resolving targets must not invent paths that do not exist — otherwise the preview
// promises to delete files the user does not have, and the summary count is a lie.
func TestResetTargetsSkipMissingPaths(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.AppConfig{
		StateDir: stateDir,
		DBPath:   filepath.Join(stateDir, "state.db"), // never created
	}
	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Errorf("an empty state dir should yield no targets, got %v", paths(targets))
	}
}

func TestParseResetScope(t *testing.T) {
	for _, want := range []ResetScope{ScopeProjectState, ScopeAllData} {
		if got, ok := ParseResetScope(string(want)); !ok || got != want {
			t.Errorf("ParseResetScope(%q) = %q, %v", want, got, ok)
		}
	}
	for _, bad := range []string{"", "all", "everything", "project", "credentials", "PROJECT-STATE", "rm -rf"} {
		if _, ok := ParseResetScope(bad); ok {
			t.Errorf("ParseResetScope(%q) should not match a scope", bad)
		}
	}
}

// The backup is the safety net a reset is taken under. It must copy real state, skip
// process-lifetime files that mean nothing outside their process, and land OUTSIDE the
// directory being deleted — a backup inside the state dir would be destroyed with it.
func TestBackupTargetsCopiesStateBesideTheStateDir(t *testing.T) {
	cfg, _ := resetFixture(t)
	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}

	dest, err := backupTargets(cfg, targets)
	if err != nil {
		t.Fatalf("backupTargets: %v", err)
	}
	if dest == "" {
		t.Fatal("expected a backup directory")
	}
	if strings.HasPrefix(dest, cfg.StateDir+string(os.PathSeparator)) {
		t.Fatalf("the backup must not live inside the directory being removed: %s", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "state.db")); err != nil {
		t.Errorf("the database was not backed up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "artifacts", "art_1.json")); err != nil {
		t.Errorf("artifacts were not backed up recursively: %v", err)
	}
	// Locks are process-lifetime artifacts; copying one would preserve something
	// meaningless and, restored, actively misleading.
	if _, err := os.Stat(filepath.Join(dest, ipc.OwnerLockName)); err == nil {
		t.Error("the owner lock should not be backed up")
	}
	// Owner-only, like everything else this CLI writes.
	info, err := os.Stat(filepath.Join(dest, "state.db"))
	if err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("backed-up database mode = %v, want 0600", info.Mode().Perm())
	}
}

// Running a reset twice must not copy the previous backup into the new one, which would
// double the size each time.
func TestBackupDoesNotRecurseIntoAPreviousBackup(t *testing.T) {
	cfg, root := resetFixture(t)
	stale := filepath.Join(root, "proj-abc123.backup-20260101-000000")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "state.db"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Put a nested backup INSIDE the state dir too, to prove the walk skips it.
	nested := filepath.Join(cfg.StateDir, "artifacts", "x.backup-20260101-000000")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}

	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := backupTargets(cfg, targets)
	if err != nil {
		t.Fatalf("backupTargets: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "artifacts", "x.backup-20260101-000000")); err == nil {
		t.Error("a nested backup directory was copied into the new backup")
	}
}

// Every scope must say plainly what SURVIVES. A destructive prompt that does not answer
// that is a leap, not a decision.
func TestResetSurvivorNotesSayWhatSurvives(t *testing.T) {
	cfg, _ := resetFixture(t)

	project := strings.Join(resetSurvivorNotes(cfg, ScopeProjectState), " ")
	if !strings.Contains(project, "Other projects") {
		t.Errorf("project-state must say other projects survive: %s", project)
	}
	all := strings.Join(resetSurvivorNotes(cfg, ScopeAllData), " ")
	// It must be precise about its REACH. The command can only take the lease for THIS
	// project, so claiming "every project" would be both false and a promise it cannot
	// safely keep.
	if !strings.Contains(all, "Only THIS project") {
		t.Errorf("all-data must say it reaches only this project: %s", all)
	}
	// Every scope reassures about the one thing that is never at risk.
	for _, scope := range []ResetScope{ScopeProjectState, ScopeAllData} {
		if !strings.Contains(strings.Join(resetSurvivorNotes(cfg, scope), " "), "code") {
			t.Errorf("%s must state that the user's code is untouched", scope)
		}
	}
}
