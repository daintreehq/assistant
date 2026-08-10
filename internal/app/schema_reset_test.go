package app

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/storage"
)

// These tests cover the BACKUP side of the stale-schema recovery (app_test.go's
// TestCreateSchemaResetRecovery covers authorise/decline/error routing):
// an authorised reset must move the old DB aside — never delete it — and report
// the backup path; a failed backup must refuse the reset entirely.
// stampStaleStateDB lives in app_test.go.

func staleCreateOpts(dir string) CreateOptions {
	return CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
	}
}

// TestCreateStaleSchemaBacksUpBeforeReset: an authorised stale-schema reset must
// MOVE the old database aside (timestamped backup, original bytes intact) — never
// delete it — surface the backup path via OnSchemaReset, and boot on a fresh DB.
func TestCreateStaleSchemaBacksUpBeforeReset(t *testing.T) {
	dir := t.TempDir()
	path := stampStaleStateDB(t, dir)

	var gotBackup string
	opts := staleCreateOpts(dir)
	opts.OnSchemaStale = func(have, want int) (bool, error) {
		if have != 1 {
			t.Errorf("OnSchemaStale have = %d, want 1", have)
		}
		return true, nil
	}
	opts.OnSchemaReset = func(backupPath string) { gotBackup = backupPath }

	a, err := Create(opts)
	if err != nil {
		t.Fatalf("Create after authorised reset: %v", err)
	}
	defer a.Shutdown()

	if gotBackup == "" {
		t.Fatal("OnSchemaReset was not invoked with the backup path")
	}
	if !strings.Contains(gotBackup, ".bak-v1-") {
		t.Errorf("backup path should carry the old version + timestamp, got %q", gotBackup)
	}
	// The backup IS the old database: still stamped with the stale baseline (the
	// failed pre-backup Open may have touched the journal-mode header, so a
	// byte-exact compare would be wrong — the schema stamp is the identity).
	raw, rerr := sql.Open("sqlite", gotBackup)
	if rerr != nil {
		t.Fatalf("open backup: %v", rerr)
	}
	defer raw.Close()
	var backupVersion int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&backupVersion); err != nil {
		t.Fatalf("read backup user_version: %v", err)
	}
	if backupVersion != 1 {
		t.Errorf("backup user_version = %d, want the original stale 1", backupVersion)
	}
	// The fresh DB at the original path is live (the App's store is open on it).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh DB missing at %s: %v", path, err)
	}
}

// NOTE on the failed-backup case: Create refuses to reset when storage.BackupDB
// errors (it returns before any open/rebuild; the path contains no delete step at
// all). The failure behaviour itself — rename fails → error returned, original
// files untouched — is proven in storage's TestBackupDBFailureLeavesFilesUntouched;
// it cannot be arranged end-to-end here because any directory restriction that
// breaks the backup rename also breaks sqlite's own journal handling, failing
// storage.Open with a NON-stale error before the stale branch is ever reached.

// TestCreateStaleSchemaNoHandlerKeepsLoudError: the non-interactive path is
// unchanged — no OnSchemaStale handler means the typed stale error propagates and
// the on-disk DB is not touched (no reset, no backup).
func TestCreateStaleSchemaNoHandlerKeepsLoudError(t *testing.T) {
	dir := t.TempDir()
	path := stampStaleStateDB(t, dir)

	_, err := Create(staleCreateOpts(dir))
	var stale *storage.SchemaStaleError
	if !errors.As(err, &stale) {
		t.Fatalf("want *storage.SchemaStaleError, got %T: %v", err, err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("stale DB must remain in place on the no-handler path: %v", serr)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			t.Fatalf("no backup may be taken without an authorised reset, found %s", e.Name())
		}
	}
}
