package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ipc"
	"github.com/daintreehq/assistant/internal/supervisor"
)

// reset.go is the SAFE replacement for `rm -rf`-ing the state directory.
//
// The Makefile's db-reset target used to remove the whole state root from the shell,
// which is wrong in four separate ways and quietly so:
//
//   - It unlinks owner.lock while a live process still holds a flock on that INODE. The
//     lock survives on the open descriptor, the next process creates a DIFFERENT file and
//     acquires it trivially, and the single-owner invariant — the thing standing between
//     two processes and one SQLite database — is gone with no error anywhere.
//   - It leaves a running process writing to an unlinked database, so work that appears
//     to succeed lands in a file nothing can ever open again.
//   - It removes the daemon's socket and lock while the daemon is alive, leaving an
//     unreachable process holding project state.
//   - It says nothing about what is being destroyed: memories, watchers, workflows, async
//     operations, artifacts, the audit trail, and the conversation all go together.
//
// Every one of those is a consequence of doing the work in the shell, where none of the
// invariants are visible. So the reset moves into the CLI, which knows the exact paths,
// can stop the daemon, can TAKE the owner lease before touching anything, and can say
// what it is about to remove.

// ResetScope names what a reset removes.
type ResetScope string

const (
	// ScopeProjectState removes this project's supervision and conversation state.
	// The common case: a schema change or a wedged project.
	ScopeProjectState ResetScope = "project-state"
	// ScopeAllData removes everything this CLI has ever written for this project.
	//
	// There is no separate credentials scope any more, because there is no stored
	// credential: the backend holds its own upstream key and the CLI never writes one.
	ScopeAllData ResetScope = "all-data"
)

// resetScopes is the parse table and the help text, in the order they are offered.
var resetScopes = []struct {
	Scope ResetScope
	Help  string
}{
	{ScopeProjectState, "this project's conversation, memories, watchers, timers, async work, workflows, artifacts and audit trail"},
	{ScopeAllData, "everything this CLI has written for this project (other projects are untouched — run it in each)"},
}

// ParseResetScope maps a subcommand word to a scope.
func ParseResetScope(s string) (ResetScope, bool) {
	for _, r := range resetScopes {
		if string(r.Scope) == strings.TrimSpace(s) {
			return r.Scope, true
		}
	}
	return "", false
}

// ResetOptions tunes a reset run beyond the shared CLI Options.
type ResetOptions struct {
	// Yes skips the interactive confirmation. Required for a non-TTY run, where there is
	// nobody to ask — a scripted reset must be explicit about being destructive rather
	// than silently proceeding because stdin happened not to be a terminal.
	Yes bool
	// NoBackup skips the timestamped backup. The default is to BACK UP: a reset is
	// usually a recovery step taken under uncertainty, and a copy costs a few megabytes
	// against permanently losing a week of memories and an audit trail.
	NoBackup bool
}

// RunReset is the `daintree-assistant reset <scope>` subcommand.
//
// The sequence is deliberate and each step exists because the shell version skipped it:
// resolve paths in Go (never re-derive them in make) → stop the daemon → TAKE the owner
// lease → show exactly what will be removed → confirm → back up → remove only the
// requested scope → release. The lease is the load-bearing step: holding it proves no
// cockpit or daemon is mid-write, and releasing it afterwards lets the next launch
// rebuild cleanly.
func RunReset(ctx context.Context, opts Options, scope ResetScope, ropts ResetOptions) int {
	r := render.Stdout()
	cfg, err := config.LoadConfig(overridesFromOptions(opts))
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}

	targets, err := resetTargets(cfg, scope)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	if len(targets) == 0 {
		r.Line("Nothing to remove — no state exists for this scope.")
		return domain.OneShotExitCode.Success
	}

	r.Line("Daintree Assistant — reset " + string(scope))
	r.Line("")
	r.Line("This will permanently remove:")
	var totalBytes int64
	for _, t := range targets {
		size := pathSize(t.Path)
		totalBytes += size
		r.Line(fmt.Sprintf("  %-28s %s", t.Label, r.Gray(t.Path)))
	}
	r.Line("")

	// Say what SURVIVES, not just what dies. "Will my key be gone?" is the question a
	// user actually has, and answering it up front is what makes the destructive choice
	// an informed one rather than a leap.
	for _, line := range resetSurvivorNotes(cfg, scope) {
		r.Line("  " + r.Gray(line))
	}
	r.Line("")

	if !ropts.Yes {
		if !stdinIsTTY() {
			r.Error("reset needs confirmation. Re-run with --yes (there is no terminal here to ask on).")
			return domain.OneShotExitCode.Error
		}
		ok, cerr := confirmReset(r, scope)
		if cerr != nil {
			r.Error(cerr.Error())
			return domain.OneShotExitCode.Error
		}
		if !ok {
			r.Line("Cancelled — nothing was removed.")
			return domain.OneShotExitCode.Success
		}
	}

	// Stop the daemon FIRST. It holds the owner lease whenever no cockpit is attached,
	// so without this the acquire below would simply time out — and worse, a daemon that
	// survived the reset would keep an open handle to a database we are about to delete.
	if err := supervisor.RequestShutdown(ctx, cfg.StateDir); err != nil && !errors.Is(err, ipc.ErrNoDaemon) {
		r.Line(r.Gray("could not stop the supervisor daemon: " + err.Error()))
	}

	// Take the owner lease. This is the whole reason the command exists: acquiring it
	// proves nothing else is mid-write, and failing to acquire it is the ONLY safe
	// outcome when something is — far better than deleting a database out from under a
	// live cockpit. SpawnDaemon is false: spawning a supervisor to immediately delete its
	// state would be absurd.
	own, err := supervisor.AcquireOwnership(ctx, cfg, supervisor.AcquireOptions{
		SpawnDaemon: false,
		Version:     buildVersion,
		WaitFor:     15 * time.Second,
		Log:         func(m string) { r.Line(r.Gray(m)) },
	})
	if err != nil {
		r.Error("another Daintree Assistant is using this project's state — close it (or run `daintree-assistant daemon stop`) and try again.\n  " + err.Error())
		return domain.OneShotExitCode.Error
	}
	defer own.Release()

	// Re-resolve the targets now that we hold the lease.
	//
	// The list above was a PREVIEW, taken before the barrier and possibly minutes before
	// the confirmation came back. A live owner keeps writing in the meantime: a WAL and
	// SHM appear and disappear, the daemon writes its descriptor and log. Deleting the
	// stale snapshot would leave exactly those files behind — a reset that reports success
	// and leaves the database's sidecars pointing at nothing, which is how a "clean" state
	// dir opens dirty on the next launch.
	targets, err = resetTargets(cfg, scope)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	if len(targets) == 0 {
		r.Line("Nothing to remove — the state was already gone.")
		return domain.OneShotExitCode.Success
	}

	if !ropts.NoBackup {
		backup, berr := backupTargets(cfg, targets)
		if berr != nil {
			// A failed backup ABORTS. The user asked for a safety net; proceeding without
			// one after promising it would be the single worst outcome here.
			r.Error("backup failed, so nothing was removed: " + berr.Error())
			return domain.OneShotExitCode.Error
		}
		if backup != "" {
			r.Line("Backed up to " + backup)
		}
	}

	var failed []string
	for _, t := range targets {
		if err := os.RemoveAll(t.Path); err != nil {
			failed = append(failed, t.Path+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		r.Error("some paths could not be removed:\n  " + strings.Join(failed, "\n  "))
		return domain.OneShotExitCode.Error
	}

	r.Line(fmt.Sprintf("Removed %d item(s), %s.", len(targets), humanBytes(totalBytes)))
	for _, line := range resetNextSteps(scope) {
		r.Line(r.Gray(line))
	}
	return domain.OneShotExitCode.Success
}

// assertSafeStateDir refuses a path that must never be handed to RemoveAll.
//
// The load-bearing check is PROVENANCE, not shape. Shape checks alone (absolute, not a
// root, not $HOME) accept any ordinary nested directory — and DAINTREE_ASSISTANT_STATE_DIR
// is an env var a developer sets by hand, so
//
//	DAINTREE_ASSISTANT_STATE_DIR="$PWD" make db-reset
//
// would enumerate .git, go.mod, and every source file for deletion, with the Make target
// supplying --yes, while the confirmation cheerfully printed "your code is untouched".
// That is the single worst thing this command could do, and no amount of path-shape
// reasoning prevents it: the path IS well-formed. The directory has to prove it belongs
// to the assistant before its contents are swept.
//
// The remaining checks bound the damage from a path that somehow passes:
//
//	absolute        a relative path resolves against the working directory — i.e. inside
//	                the bound repository, which is the last place to start deleting.
//	not a root      "/" or a volume root; and at least two segments deep, so "/tmp" or
//	                "/Users" never qualifies.
//	not $HOME, not the project, and not an ANCESTOR of either.
//	a real dir      not a symlink: RemoveAll on a symlinked directory removes the LINK, so
//	                a reset would report success while deleting nothing, and every later
//	                launch would recreate state somewhere unexpected. Checked on the
//	                resolved path so an ancestor symlink cannot hide the relationship
//	                tests above.
func assertSafeStateDir(dir string, cfg config.AppConfig) error {
	cleaned := filepath.Clean(strings.TrimSpace(dir))
	if cleaned == "" || cleaned == "." || cleaned == string(filepath.Separator) {
		return fmt.Errorf("refusing to reset: %q is not a safe state directory", dir)
	}
	if !filepath.IsAbs(cleaned) {
		return fmt.Errorf("refusing to reset: %q is a relative path — it would resolve inside the current project", dir)
	}
	if filepath.Dir(cleaned) == cleaned {
		return fmt.Errorf("refusing to reset: %q is a filesystem root", dir)
	}
	if segs := strings.Split(strings.Trim(cleaned, string(filepath.Separator)), string(filepath.Separator)); len(segs) < 2 {
		return fmt.Errorf("refusing to reset: %q is too close to the filesystem root", dir)
	}

	// Resolve symlinks in EVERY component before comparing. An ancestor symlink would
	// otherwise let a path that is textually unrelated to $HOME or the project resolve
	// straight into one.
	resolved := cleaned
	if r, err := filepath.EvalSymlinks(cleaned); err == nil {
		resolved = r
	}
	if info, err := os.Lstat(cleaned); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to reset: %q is a symlink — resolve it and pass the real path", dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("refusing to reset: %q is not a directory", dir)
		}
	}
	for _, forbidden := range []struct{ path, what string }{
		{homeDir(), "your home directory"},
		{cfg.ProjectPath, "the project directory"},
	} {
		if forbidden.path == "" {
			continue
		}
		f := filepath.Clean(forbidden.path)
		if r, err := filepath.EvalSymlinks(f); err == nil {
			f = r
		}
		if resolved == f {
			return fmt.Errorf("refusing to reset: %q is %s", dir, forbidden.what)
		}
		if isAncestor(resolved, f) {
			return fmt.Errorf("refusing to reset: %q contains %s", dir, forbidden.what)
		}
	}

	return assertLooksLikeStateDir(cleaned, cfg)
}

// assertLooksLikeStateDir requires positive evidence that a directory is the assistant's.
//
// A directory qualifies if it is EMPTY (a state dir that has not been used yet — nothing
// to lose either way) or if it contains something only this CLI writes: the database, the
// stored sign-in, or a lease file. A source repository contains none of those, which is
// exactly the case this exists to stop.
//
// Deliberately not a marker file: that would need writing on every launch and would still
// leave every existing state directory unprotected until its next run. Recognising the
// artefacts the assistant already creates works immediately, for every install.
func assertLooksLikeStateDir(dir string, cfg config.AppConfig) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing directory is fine — there is nothing to remove, and the caller's
		// os.Stat filter will produce an empty target list.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read state dir: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	for _, e := range entries {
		n := e.Name()
		if n == "state.db" || strings.HasPrefix(n, "state.db-") ||
			n == ipc.OwnerLockName || n == ipc.DaemonLockName {
			return nil
		}
	}
	return fmt.Errorf("refusing to reset: %q does not look like a Daintree Assistant state directory "+
		"(no state.db or lease file). Check DAINTREE_ASSISTANT_STATE_DIR — pointing it at a "+
		"source directory would delete that directory's contents", dir)
}

// isAncestor reports whether dir contains child (or is it).
func isAncestor(dir, child string) bool {
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// homeDir returns the user's home directory, or "" when it cannot be resolved.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// resetTarget is one path a reset will remove.
type resetTarget struct {
	Label string
	Path  string
}

// resetTargets resolves the exact paths for a scope, skipping ones that do not exist.
//
// Paths come from the RESOLVED config, never re-derived: the Makefile duplicating
// config's project-slug logic in shell was how it ended up able to point at the wrong
// directory (or, with a whitespace-only override, at nothing at all).
func resetTargets(cfg config.AppConfig, scope ResetScope) ([]resetTarget, error) {
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		return nil, errors.New("refusing to reset: the state directory did not resolve")
	}
	// Defence against a catastrophic override. DAINTREE_ASSISTANT_STATE_DIR is a trusted
	// env var, but a typo in it turns this command into an unbounded delete, and the cost
	// of being wrong here is somebody's home directory.
	if err := assertSafeStateDir(stateDir, cfg); err != nil {
		return nil, err
	}

	var out []resetTarget
	add := func(label, path string) {
		if path == "" {
			return
		}
		if _, err := os.Stat(path); err == nil {
			out = append(out, resetTarget{Label: label, Path: path})
		}
	}

	switch scope {
	case ScopeProjectState:
		// Everything in the state dir EXCEPT the preserved set, rather than an allowlist
		// of known filenames.
		//
		// An allowlist looked tidier and was wrong: the moment something new is written
		// here — a cache, an export, a second database — it silently survives every
		// reset, and "reset project state" quietly stops meaning what it says. Enumerating
		// what must be KEPT is a much shorter and far more stable list, and a new file
		// defaults to being cleaned rather than to being missed.
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			return nil, fmt.Errorf("read state dir: %w", err)
		}
		for _, e := range entries {
			name := e.Name()
			if preservedByProjectStateReset(name, cfg) {
				continue
			}
			add(resetLabelFor(name), filepath.Join(stateDir, name))
		}

	case ScopeAllData:
		// The state dir's CONTENTS — never the directory itself.
		//
		// RemoveAll(stateDir) would unlink owner.lock along with everything else, while
		// this process holds an flock on it. The lock lives on the open descriptor, so a
		// concurrent launch could recreate the directory and a fresh owner.lock and
		// acquire it immediately — two processes owning one project, which is the exact
		// hazard this command was written to remove. ipc.FileLock.Release documents that
		// the lock file must stay on disk. Leaving an empty state dir with two zero-byte
		// lease files behind costs nothing; breaking single-owner costs correctness.
		entries, err := os.ReadDir(stateDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read state dir: %w", err)
		}
		for _, e := range entries {
			if e.Name() == ipc.OwnerLockName || e.Name() == ipc.DaemonLockName {
				continue
			}
			add(resetLabelFor(e.Name()), filepath.Join(stateDir, e.Name()))
		}

	default:
		return nil, fmt.Errorf("unknown reset scope %q", scope)
	}
	return out, nil
}

// preservedByProjectStateReset reports whether a state-dir entry survives a
// project-state reset.
//
// The LEASE FILES and the control socket, for one reason. Deleting owner.lock while
// holding an flock on it is the inode-vs-path hazard this command exists to prevent: the
// lock lives on the open descriptor, so unlinking the path lets a second process create a
// NEW owner.lock, acquire it trivially, and start writing the same database while we
// still believe we own it. They also hold no state — a lease file is a pid stamp,
// recreated on demand — so removing them buys nothing and risks the one invariant that
// matters.
func preservedByProjectStateReset(name string, cfg config.AppConfig) bool {
	switch name {
	case ipc.OwnerLockName, ipc.DaemonLockName:
		return true
	}
	// The control socket is derived, not a fixed name, so compare the resolved path.
	if sock, err := ipc.SocketPathFor(cfg.StateDir); err == nil && filepath.Base(sock) == name {
		return true
	}
	// A previous backup, if one was ever written inside the state dir. Removing it would
	// destroy the safety net from the last reset at the exact moment someone is reaching
	// for it.
	return strings.Contains(name, ".backup-")
}

// resetLabelFor gives a state-dir entry a human name for the removal preview, so the
// confirmation reads as a list of things rather than a list of filenames.
func resetLabelFor(name string) string {
	switch {
	case name == "state.db":
		return "database"
	case strings.HasPrefix(name, "state.db-"):
		return "database (" + strings.TrimPrefix(name, "state.db-") + ")"
	case name == "artifacts":
		return "artifacts"
	default:
		return name
	}
}

// resetSurvivorNotes says what a scope does NOT touch.
func resetSurvivorNotes(cfg config.AppConfig, scope ResetScope) []string {
	notes := []string{"Your code, your worktrees, and Daintree itself are untouched."}
	switch scope {
	case ScopeProjectState:
		notes = append(notes, "Other projects' state is untouched.")
	case ScopeAllData:
		notes = append(notes,
			// Being precise here matters: the command can only take the lease for THIS
			// project, so reaching into sibling project directories would mean deleting
			// state a live process might be writing. Saying "every project" would have
			// been both false and a promise this cannot safely keep.
			"Only THIS project's state is removed. Other projects keep theirs — run this in each.",
			"Debug logs (~/.daintree/logs) are separate and are not touched.")
	}
	return notes
}

// resetNextSteps tells the user what to do now.
func resetNextSteps(ResetScope) []string {
	return []string{"Start the assistant normally — a fresh state is created on launch."}
}

// backupTargets copies the targets into a timestamped sibling directory and returns its
// path ("" when there was nothing worth copying).
//
// A COPY, not a move: the removal below is what deletes the originals, and a move would
// leave the state dir half-emptied if it failed partway. The backup lands beside the
// state dir rather than inside it, so an all-data reset does not delete its own backup.
func backupTargets(cfg config.AppConfig, targets []resetTarget) (string, error) {
	// Exclusive creation, with a collision suffix. MkdirAll on a second-resolution name
	// silently MERGES with an existing backup from the same second — overwriting the very
	// files it exists to preserve — and would happily follow a pre-created symlink at that
	// path out of the directory.
	stamp := time.Now().UTC().Format("20060102-150405")
	base := filepath.Clean(cfg.StateDir) + ".backup-" + stamp
	dest := base
	for i := 1; ; i++ {
		err := os.Mkdir(dest, 0o700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("create backup dir: %w", err)
		}
		if i > 50 {
			return "", fmt.Errorf("create backup dir: %s and 50 variants already exist", base)
		}
		dest = fmt.Sprintf("%s-%d", base, i)
	}
	copied := 0
	for _, t := range targets {
		// Locks and sockets are process-lifetime artifacts; copying them would be
		// meaningless at best and, for a socket, an error.
		if strings.HasSuffix(t.Path, ".lock") || strings.HasSuffix(t.Path, ".sock") {
			continue
		}
		if err := copyPath(t.Path, filepath.Join(dest, filepath.Base(t.Path))); err != nil {
			return "", err
		}
		copied++
	}
	if copied == 0 {
		_ = os.Remove(dest)
		return "", nil
	}
	return dest, nil
}

// copyPath copies a file or directory tree, preserving 0600/0700 owner-only modes.
//
// Lstat, and irregular entries are SKIPPED rather than followed. A symlink in the state
// dir would otherwise be dereferenced — copying whatever it points at, possibly something
// enormous or entirely outside the tree — and a socket or FIFO would either block on open
// or error out and abort a reset for no good reason. None of them carry recoverable
// state, which is what a backup is for.
func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil
	}
	if !info.IsDir() {
		// Streamed, not ReadFile: state.db is routinely hundreds of megabytes once a
		// project has history, and reading it whole to write it whole doubles peak memory
		// for no benefit.
		in, rerr := os.Open(src)
		if rerr != nil {
			return rerr
		}
		defer in.Close()
		out, werr := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if werr != nil {
			return werr
		}
		if _, cerr := io.Copy(out, in); cerr != nil {
			out.Close()
			return cerr
		}
		return out.Close()
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// Never recurse into a previous backup — an all-data reset run twice would
		// otherwise copy backup-into-backup and grow without bound.
		if strings.Contains(e.Name(), ".backup-") {
			continue
		}
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// confirmReset asks for a typed confirmation.
//
// Typed, not y/N, and matching the friction the tool layer applies to its own
// destructive actions: this removes durable state with no undo beyond the backup, and a
// reflexive keypress is not consent for that.
func confirmReset(r *render.Renderer, scope ResetScope) (bool, error) {
	want := string(scope)
	fmt.Printf("Type %q to confirm (anything else cancels): ", want)
	var got string
	if _, err := fmt.Scanln(&got); err != nil {
		// A bare newline (no token) is a cancel, not a failure.
		if strings.Contains(err.Error(), "unexpected newline") {
			return false, nil
		}
		return false, nil
	}
	return strings.TrimSpace(got) == want, nil
}

// pathSize returns the total bytes at a path (0 on any error — this only ever feeds a
// human-readable summary, never a decision).
func pathSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// ResetUsage renders the scope list for --help, in the surrounding block's column
// layout (two spaces, then a 20-char name column).
//
// The one-line summaries here are deliberately short; the full "what survives" story is
// printed by the command itself, right before it asks for confirmation, which is the
// moment the user actually needs it.
func ResetUsage() string {
	var b strings.Builder
	b.WriteString("  reset <scope>       remove local state safely (stops the daemon, takes the\n")
	b.WriteString("                      owner lease, backs up first)\n")
	for _, s := range resetScopes {
		b.WriteString(fmt.Sprintf("    %-18s%s\n", s.Scope, resetScopeSummary(s.Scope)))
	}
	return b.String()
}

// resetScopeSummary is the one-line help form. Separate from the long Help text so the
// usage block stays inside a terminal width.
func resetScopeSummary(s ResetScope) string {
	switch s {
	case ScopeProjectState:
		return "this project's conversation and supervision state"
	case ScopeAllData:
		return "everything this CLI has written for this project"
	}
	return ""
}
