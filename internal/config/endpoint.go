package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// endpoint.go stores ONE preference: which Daintree backend this user talks to.
//
// It is deliberately not a revived credentials file, and the difference is the whole
// point. There is no credential here and never will be — the backend holds its own
// upstream key. What this file holds is a choice a developer makes once and expects to
// survive a restart: run against a local backend day to day, switch to the deployed one
// occasionally to check it. Making that choice per-invocation (an exported variable, a
// flag on every launch) is the kind of friction that gets worked around with a shell
// alias nobody else can see.
//
// It is PER-USER, at the state ROOT, because the endpoint is a property of the person
// and their machine rather than of any one project — the same reasoning the old sign-in
// used. An explicit state-dir override moves it, so tests, benchmarks and harnesses
// neither read nor clobber a developer's real choice.

// EndpointFileName is the stored-preference file.
const EndpointFileName = "endpoint.json"

// storedEndpoint is the on-disk shape. A struct rather than a bare string so a second
// preference can join it later without a format break.
type storedEndpoint struct {
	BackendURL string `json:"backend_url"`
}

// EndpointPath returns the preference file path for a resolved directory.
func EndpointPath(dir string) string { return filepath.Join(dir, EndpointFileName) }

// LoadBackendURL reads the stored endpoint, or "" when none is stored.
//
// A missing OR malformed file resolves to "" rather than an error, and that is
// deliberate: this is a preference, not state. Erroring here would make an unparseable
// file brick every launch — including the `/backend` command that exists to rewrite it —
// leaving "delete this file yourself" as the only way out of a stray keystroke.
// Resolving to "" falls back to the deployed default, which always works.
func LoadBackendURL(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s storedEndpoint
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s.BackendURL)
}

// SaveBackendURL writes the stored endpoint atomically.
//
// Atomic rename, not a truncating write: a crash mid-write would otherwise leave a
// half-file that LoadBackendURL reads as "nothing stored", silently reverting a choice
// the user believes they made. 0600 on the file and 0700 on the directory for
// consistency with everything else this CLI writes — nothing secret lives here, but a
// state root with one world-readable file in it invites the next one to be too.
func SaveBackendURL(path, backendURL string) error {
	backendURL = strings.TrimSpace(backendURL)
	if backendURL == "" {
		return errors.New("no endpoint to save")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	body, err := json.MarshalIndent(storedEndpoint{BackendURL: backendURL}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode endpoint: %w", err)
	}
	tmp, err := os.CreateTemp(dir, EndpointFileName+".*")
	if err != nil {
		return fmt.Errorf("write endpoint: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write endpoint: %w", err)
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write endpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write endpoint: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write endpoint: %w", err)
	}
	return nil
}

// ForgetBackendURL removes the stored endpoint. A missing file is not an error —
// resetting to the default twice succeeds both times.
func ForgetBackendURL(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove endpoint: %w", err)
	}
	return nil
}
