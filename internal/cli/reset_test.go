package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
	"github.com/daintreehq/assistant/internal/ipc"
)

// resetFixture builds a populated state dir and the config that describes it, with the
// credentials file at the per-user ROOT (one level up) — the real layout, and the one the
// old `rm -rf` got wrong by deleting a sign-in that does not belong to the project.
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
	credPath := credentials.Path(root)
	write(credPath)

	return config.AppConfig{
		StateDir:        stateDir,
		DBPath:          dbPath,
		CredentialsPath: credPath,
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

// The defining property of the project-state scope: the sign-in SURVIVES.
//
// This is the bug the shell version had. "Reset my project" silently destroyed a
// spendable API key the user may not have stored anywhere else, and gave no hint that it
// had happened — they discovered it at the next launch, as a login prompt.
func TestResetProjectStateNeverTouchesCredentials(t *testing.T) {
	cfg, _ := resetFixture(t)

	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	got := paths(targets)
	if contains(got, cfg.CredentialsPath) {
		t.Fatalf("project-state must never remove the sign-in, but targets included it: %v", got)
	}
	// It must still remove the things it promises.
	for _, want := range []string{
		cfg.DBPath,
		cfg.DBPath + "-wal", // a stray WAL beside a deleted DB is how a "clean" dir opens dirty
		cfg.DBPath + "-shm",
		filepath.Join(cfg.StateDir, "artifacts"),
	} {
		if !contains(got, want) {
			t.Errorf("project-state should remove %s, targets: %v", want, got)
		}
	}
}

// The lease files must SURVIVE — deleting owner.lock while holding an flock on it is the
// inode-vs-path hazard this whole command exists to prevent. The lock lives on the open
// descriptor, so unlinking the path lets a second process create a NEW owner.lock,
// acquire it trivially, and start writing the same database while we still believe we own
// it. They hold no state either: a lease file is a pid stamp, recreated on demand.
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

// With an explicit DAINTREE_ASSISTANT_STATE_DIR the sign-in lives INSIDE the state dir,
// so a directory sweep would take it — the same bug as the shell version, reached by a
// different route.
func TestResetProjectStateKeepsCredentialsStoredInsideTheStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "isolated")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.AppConfig{
		StateDir:        stateDir,
		DBPath:          filepath.Join(stateDir, "state.db"),
		CredentialsPath: credentials.Path(stateDir), // the override layout
	}
	for _, f := range []string{"state.db", "credentials.json"} {
		if err := os.WriteFile(filepath.Join(stateDir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	targets, err := resetTargets(cfg, ScopeProjectState)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	got := paths(targets)
	if contains(got, cfg.CredentialsPath) {
		t.Fatalf("project-state removed the sign-in from an overridden state dir: %v", got)
	}
	if !contains(got, cfg.DBPath) {
		t.Errorf("project-state should still remove the database: %v", got)
	}
}

func TestResetCredentialsScopeRemovesOnlyTheSignIn(t *testing.T) {
	cfg, _ := resetFixture(t)

	targets, err := resetTargets(cfg, ScopeCredentials)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].Path != cfg.CredentialsPath {
		t.Fatalf("credentials scope must target exactly the sign-in, got %v", paths(targets))
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Error("the database must still exist — resolving targets must not delete anything")
	}
}

// all-data takes the state dir's CONTENTS and the sign-in, even though the sign-in lives
// one level up at the per-user root.
//
// The contents, never the DIRECTORY: RemoveAll on the state dir would unlink owner.lock
// while this process holds an flock on it, letting a concurrent launch recreate the
// directory and a fresh lock and acquire it — two owners on one database, which is the
// hazard the whole command exists to remove.
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
	if !contains(got, cfg.CredentialsPath) {
		t.Errorf("all-data must remove the sign-in, got %v", got)
	}
}

// With an explicit state-dir override the credentials live INSIDE the state dir, so
// listing them separately would name the same file twice and try to remove it twice.
func TestResetAllDataDoesNotDoubleCountCredentialsInsideTheStateDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.AppConfig{
		StateDir:        dir,
		DBPath:          filepath.Join(dir, "state.db"),
		CredentialsPath: credentials.Path(dir),
	}
	if err := os.WriteFile(cfg.CredentialsPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := resetTargets(cfg, ScopeAllData)
	if err != nil {
		t.Fatalf("resetTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].Path != cfg.CredentialsPath {
		t.Fatalf("want the sign-in named exactly once, got %v", paths(targets))
	}
}

// A "remove my key" scope that quietly writes a plaintext copy of that key into a new
// directory has not removed it — and the user has no idea the copy exists.
func TestBackupNeverCopiesTheSignIn(t *testing.T) {
	cfg, _ := resetFixture(t)

	targets, err := resetTargets(cfg, ScopeAllData)
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
	if _, err := os.Stat(filepath.Join(dest, filepath.Base(cfg.CredentialsPath))); err == nil {
		t.Error("the sign-in was copied into the backup — the key survives a reset that promised to remove it")
	}
}

// A misresolved state dir must never become an unbounded delete. This is the guard the
// shell version approximated with an empty-string check — which would not have caught
// "/" or a relative path resolving to the user's home.
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
		StateDir:        stateDir,
		DBPath:          filepath.Join(stateDir, "state.db"), // never created
		CredentialsPath: credentials.Path(dir),               // never created
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
	for _, want := range []ResetScope{ScopeProjectState, ScopeCredentials, ScopeAllData} {
		if got, ok := ParseResetScope(string(want)); !ok || got != want {
			t.Errorf("ParseResetScope(%q) = %q, %v", want, got, ok)
		}
	}
	for _, bad := range []string{"", "all", "everything", "project", "PROJECT-STATE", "rm -rf"} {
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

// Every scope must say plainly what SURVIVES. "Will my key be gone?" is the question the
// user actually has, and a destructive prompt that does not answer it is a leap, not a
// decision.
func TestResetSurvivorNotesAnswerTheCredentialQuestion(t *testing.T) {
	cfg, _ := resetFixture(t)

	project := strings.Join(resetSurvivorNotes(cfg, ScopeProjectState), " ")
	if !strings.Contains(project, "sign-in is KEPT") {
		t.Errorf("project-state must say the sign-in survives: %s", project)
	}
	all := strings.Join(resetSurvivorNotes(cfg, ScopeAllData), " ")
	// It must be precise about its REACH. The command can only take the lease for THIS
	// project, so claiming "every project" would be both false and a promise it cannot
	// safely keep.
	if !strings.Contains(all, "Only THIS project") {
		t.Errorf("all-data must say it reaches only this project: %s", all)
	}
	creds := strings.Join(resetSurvivorNotes(cfg, ScopeCredentials), " ")
	if !strings.Contains(creds, "KEPT") || !strings.Contains(creds, "login") {
		t.Errorf("credentials scope must say state survives and login is needed: %s", creds)
	}
	// Every scope reassures about the one thing that is never at risk.
	for _, scope := range []ResetScope{ScopeProjectState, ScopeCredentials, ScopeAllData} {
		if !strings.Contains(strings.Join(resetSurvivorNotes(cfg, scope), " "), "code") {
			t.Errorf("%s must state that the user's code is untouched", scope)
		}
	}
}
