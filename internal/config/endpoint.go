package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// maxEndpointFileBytes bounds the read. The path is not attacker-chosen, but it can be
// a symlink to something that is not a small regular file, and a startup that hangs on a
// FIFO or eats a huge file is a bad way to learn that.
const maxEndpointFileBytes = 64 << 10

// LoadBackendURL reads the stored endpoint. It returns "" when nothing is stored, and a
// non-nil error when something IS stored but could not be used.
//
// The two cases are separated deliberately, and the reason is privacy rather than
// tidiness. Collapsing them means an unreadable or corrupt file silently resolves to the
// DEPLOYED backend: someone who chose local on purpose gets their next conversation sent
// to a remote host because of a permissions error they were never shown. The caller
// still falls back to the default in both cases (a preference must never brick a launch,
// least of all the `/backend` command that exists to rewrite it), but with the error in
// hand it can say so.
func LoadBackendURL(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil // no preference — the ordinary case
		}
		return "", fmt.Errorf("read endpoint: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read endpoint: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read endpoint: %s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxEndpointFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read endpoint: %w", err)
	}
	if len(raw) > maxEndpointFileBytes {
		return "", fmt.Errorf("read endpoint: %s is larger than %d bytes", path, maxEndpointFileBytes)
	}
	var s storedEndpoint
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("read endpoint: %s is not valid JSON: %w", path, err)
	}
	return strings.TrimSpace(s.BackendURL), nil
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
