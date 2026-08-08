package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// credentialsSubpath is the GLOBAL (per-machine, not per-project) backend
// credential file under the home dir. Login deliberately does not repeat per
// project: StateDir is ProjectIDToDir-scoped, so the credential file lives as a
// sibling of the two existing global roots (stateRootSubpath, logDirSubpath).
var credentialsSubpath = filepath.Join(".daintree", "credentials.json")

// Credentials is the persisted backend login: which endpoint to talk to and the
// API key sent as a bearer token. The key is opaque to the CLI (today the user's
// own OpenRouter key, later a subscription key) — it is stored and sent verbatim.
// The endpoint is persisted explicitly (even when it is the default) so a future
// change of the code default can never silently retarget an existing key.
type Credentials struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
}

// Complete reports whether the record carries both halves of a usable login.
func (c Credentials) Complete() bool {
	return strings.TrimSpace(c.Endpoint) != "" && strings.TrimSpace(c.APIKey) != ""
}

// DefaultCredentialsPath returns ~/.daintree/credentials.json ("" when the home
// dir cannot be resolved). Tests isolate it by pointing HOME at a temp dir.
func DefaultCredentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, credentialsSubpath)
}

// LoadCredentials reads the credential file. A missing file (or an empty path)
// is the normal "not logged in" state: zero value, ok=false, nil error. A file
// that exists but cannot be read or parsed returns an error so callers can
// distinguish "never logged in" from "login is corrupt" (the login flow offers
// to redo it; non-interactive paths surface the error).
func LoadCredentials(path string) (Credentials, bool, error) {
	if path == "" {
		return Credentials{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, false, nil
		}
		return Credentials{}, false, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, false, fmt.Errorf("parse credentials %s: %w", path, err)
	}
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.APIKey = strings.TrimSpace(c.APIKey)
	// A persisted endpoint is validated exactly like a typed one: a hand-edited
	// file must not smuggle shapes the login prompt rejects (userinfo/query/
	// fragment — /status prints this URL) past the gate. Invalid ⇒ error, so the
	// login flow offers repair instead of the launcher trusting the value.
	if c.Endpoint != "" {
		norm, err := NormalizeEndpoint(c.Endpoint)
		if err != nil {
			return Credentials{}, false, fmt.Errorf("credentials %s: %w", path, err)
		}
		c.Endpoint = norm
	}
	return c, c.Complete(), nil
}

// SaveCredentials writes the credential file atomically (temp file + rename in
// the same dir), creating the parent dir 0700 and the file 0600 — the key is a
// secret on disk, owner-only like the state dir. The temp file is removed on
// every failure path.
func SaveCredentials(path string, c Credentials) error {
	if path == "" {
		return fmt.Errorf("save credentials: no path resolved (home dir unknown)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write credentials: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// NormalizeEndpoint validates and canonicalizes a backend endpoint URL: absolute
// http/https with a host, no userinfo/query/fragment (an endpoint later shown by
// /status must not be able to smuggle a secret), trailing slashes stripped. A
// path prefix is allowed (reverse-proxy mounts).
func NormalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("endpoint is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("endpoint must be http(s), got %q", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("endpoint has no host: %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("endpoint must be a bare base URL (no credentials, query, or fragment): %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}
