package e2e

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The embedded host, booted against a state database from an older schema baseline.
//
// This is the shape of every upgrade for a user who has run the assistant before, and
// it was fatal: the schema bump to 12 (the runbook_run_state rename) left every
// existing install refused at boot with
//
//	host:error  "database schema is stale (version 11, current 12) — run 'make db-reset' …"
//
// followed by a clean exit(0). Daintree reports that as "The assistant engine exited
// before it was ready", and there is no way forward from inside the app: `make` is a
// developer target that does not exist in an install, and the host had no channel to
// offer the reset the interactive terminal performs automatically.
//
// Driven through the real BINARY rather than app.Create, because the bug was pure
// WIRING — every piece of the recovery (SchemaStaleError, BackupDB, the OnSchemaStale
// hook) already existed and worked; the host simply never passed the hooks. A test
// that called the hooks directly would have passed throughout.
func TestHostBootsAgainstStaleSchema(t *testing.T) {
	bin := buildBinary(t)

	stateDir := t.TempDir()
	dbPath := filepath.Join(stateDir, "state.db")
	stampStaleHostDB(t, dbPath)

	frames, stderr := runHostBoot(t, bin, stateDir)

	var types []string
	ready := false
	for _, f := range frames {
		types = append(types, f.Type)
		switch f.Type {
		case "host:ready":
			ready = true
		case "host:error":
			t.Fatalf("host refused to boot against a stale schema: %s", f.Message)
		}
	}
	if !ready {
		t.Fatalf("host never signalled ready (frames: %v)\nstderr:\n%s", types, stderr)
	}

	// Rebuilt in place, at the CURRENT baseline — the whole point of the recovery.
	if got := userVersionOf(t, dbPath); got == 1 {
		t.Fatalf("database was left at the stale baseline (user_version=%d)", got)
	}

	// And the old state is MOVED, never destroyed. A recovery that silently deleted a
	// user's timers, watchers, memories and history would be a worse failure than the
	// refusal it replaces.
	backups, err := filepath.Glob(dbPath + ".bak-v*")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("want exactly one backup of the previous database, got %v", backups)
	}

	// Said out loud on stderr. The reset happens inside app.Create, before a session
	// exists to carry a frame, so stderr is the only channel it can use — and a state
	// reset the user is never told about is indistinguishable from data loss.
	if !strings.Contains(stderr, "older version") || !strings.Contains(stderr, "backed up to") {
		t.Fatalf("the reset was not reported on stderr:\n%s", stderr)
	}
}

// stampStaleHostDB writes a sqlite file carrying an OLDER non-zero baseline, which is
// what Open trips on. The exact number does not matter, only that it is non-zero and
// below the current baseline — pinning the real previous value here would make this
// test need editing on every future bump for no added coverage.
func stampStaleHostDB(t *testing.T, path string) {
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
}

func userVersionOf(t *testing.T, path string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

type hostFrame struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// runHostBoot spawns `host --stdio`, sends one descriptor, and collects frames until
// the process settles or the deadline passes.
//
// stdin is held OPEN for the duration and closed only at the end: the host treats EOF
// on stdin as a shutdown request, so a harness that writes the descriptor and closes
// gets a clean exit(0) that looks exactly like the failure under test.
func runHostBoot(t *testing.T, bin, stateDir string) ([]hostFrame, string) {
	t.Helper()

	cmd := exec.Command(bin, "host", "--stdio")
	cmd.Env = append(os.Environ(),
		"DAINTREE_ASSISTANT_STATE_DIR="+stateDir,
		"DAINTREE_ASSISTANT_LOG_DIR="+filepath.Join(stateDir, "logs"),
		// The descriptor's projectId and this must agree, or the host refuses the
		// handshake as a binding mismatch before any of this is reached.
		"DAINTREE_PROJECT_ID=p_stale",
		"DAINTREE_WINDOW_ID=1",
		"DAINTREE_ASSISTANT_TIER=system",
		// Deliberately unreachable: booting must not depend on a backend, and pointing
		// at a real one would make this test do billable work.
		"DAINTREE_BACKEND_URL=http://127.0.0.1:59999",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	descriptor, err := json.Marshal(map[string]any{
		"sessionId":       "ses_stale",
		"windowId":        1,
		"projectId":       "p_stale",
		"cwd":             t.TempDir(),
		"tier":            "system",
		"protocolVersion": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(append(descriptor, '\n')); err != nil {
		t.Fatal(err)
	}

	frames := make(chan hostFrame, 32)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var f hostFrame
			if json.Unmarshal(scanner.Bytes(), &f) == nil && f.Type != "" {
				frames <- f
			}
		}
	}()

	var collected []hostFrame
	deadline := time.After(30 * time.Second)
	settled := false
	for !settled {
		select {
		case f, ok := <-frames:
			if !ok {
				settled = true
				break
			}
			collected = append(collected, f)
			// Both terminal outcomes for a boot: nothing more is coming either way.
			if f.Type == "host:ready" || f.Type == "host:shutdown" {
				settled = true
			}
		case <-deadline:
			settled = true
		}
	}

	_ = stdin.Close()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return collected, stderr.String()
}
