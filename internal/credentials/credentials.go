// Package credentials persists the assistant's sign-in: which backend endpoint to
// talk to, and the caller's API key for it.
//
// The key is credential material with real cost attached — the backend holds no
// upstream credential of its own, so the bearer token the CLI sends IS the key that
// funds every model call the turn makes (see docs/BACKEND.md). It therefore lives in
// a 0600 file under a 0700 dir, is never logged, and never rides a project .env
// (which a bound repo could plant).
//
// Scope is PER-USER, not per-project: one sign-in serves every project the assistant
// is launched against, so the file sits at the state ROOT rather than in the
// per-project subdir. The one exception is an explicitly overridden state dir
// (DAINTREE_ASSISTANT_STATE_DIR / --state-dir), where the credentials follow the
// override so tests and benchmarks stay fully isolated from the real sign-in.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the credentials file's basename within the resolved directory.
const FileName = "credentials.json"

// Credentials is the stored sign-in. Both fields are required for a usable
// sign-in; a file carrying only one is treated as incomplete by Valid.
type Credentials struct {
	// BaseURL is the backend endpoint this sign-in is for. Stored alongside the key
	// because the two travel together: a key valid against the official endpoint says
	// nothing about a local one, and switching endpoints is a re-login.
	BaseURL string `json:"backend_url"`
	// APIKey is the caller's own API key, sent as the bearer token on every backend
	// request. NEVER log this — use Redact for anything human-visible.
	APIKey string `json:"api_key"`
}

// Valid reports whether the credentials are complete enough to use.
func (c Credentials) Valid() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.APIKey) != ""
}

// Redact renders a key for display: the first 8 and last 4 characters with the
// middle elided. Short keys collapse entirely rather than leaking a large fraction
// of themselves. Used by doctor and the login confirmation — never print the raw key.
func Redact(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(none)"
	}
	if len(key) <= 16 {
		return "****"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

// Path returns the credentials file path for a resolved directory.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// MaxKeyLength mirrors the backend's own bearer-token ceiling.
const MaxKeyLength = 4096

// ValidateKeyShape mirrors the backend's structural check (printable non-space ASCII,
// bounded length). Applying it at the input prompt turns an obvious mis-paste — a URL
// with a space in it, smart quotes from a chat client — into a readable message at the
// point of entry instead of an opaque 401 a round trip later.
//
// Deliberately NO prefix check: the key's issuer is not our business (an OpenRouter key
// today, a subscription key later), only that it can ride an HTTP header safely.
func ValidateKeyShape(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("a key is required")
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("key is too long (%d bytes, max %d)", len(key), MaxKeyLength)
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7E {
			return errors.New("key contains spaces or non-ASCII characters — check for a stray paste")
		}
	}
	return nil
}

// NormalizeBaseURL accepts what a human types and returns a usable base URL. A bare
// host gets https:// (the deployed edge redirects http→https anyway), and a trailing
// slash is trimmed so path joins never produce a double slash.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("a URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (use http or https)", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("URL is missing a host")
	}
	// Plain HTTP is allowed ONLY to the local machine. Every request carries the API
	// key as a bearer token, and that key is spendable — sending it unencrypted to a
	// remote host would hand it to anything on the path. Local development is the one
	// case where there is no network to eavesdrop on.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("refusing http:// for remote host %q — it would send your API key unencrypted; use https://", u.Hostname())
	}
	// Credentials in the URL itself would be a second, unmanaged copy of a secret,
	// and one that leaks anywhere the endpoint is displayed (/auth, doctor, logs).
	if u.User != nil {
		return "", errors.New("URL must not contain a username or password")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Load reads the credentials file. A missing file is NOT an error — it returns the
// zero Credentials and false, which is the ordinary "not signed in yet" state that
// every entry point must handle. A malformed file returns an ERROR alongside the
// zero value, so a caller can report what is wrong; config treats that as signed out
// (rather than fatal) precisely so `login` — whose atomic save overwrites the bad
// file — stays reachable.
func Load(path string) (Credentials, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credentials{}, false, nil
		}
		return Credentials{}, false, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, false, fmt.Errorf("parse credentials at %s: %w", path, err)
	}
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.APIKey = strings.TrimSpace(c.APIKey)
	if !c.Valid() {
		return Credentials{}, false, nil
	}
	return c, true, nil
}

// Save writes the credentials 0600 under a 0700 dir.
//
// The write goes to a temp file in the SAME dir and is renamed into place, so an
// interrupted save can never leave a half-written file that the next launch would
// reject as malformed. The temp file is created 0600 from the start — never widen
// then narrow, which would expose the key for the window in between.
func Save(path string, c Credentials) error {
	if !c.Valid() {
		return errors.New("refusing to save incomplete credentials (need both endpoint and key)")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create credentials temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: harmless once the rename has consumed the temp file.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure credentials temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install credentials: %w", err)
	}
	return nil
}

// isLoopbackHost reports whether a hostname addresses this machine. Covers the literal
// names as well as any address in 127.0.0.0/8 and ::1, so 127.0.0.2 and friends work.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Delete removes the credentials file. A missing file is not an error — logging out
// twice succeeds both times.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove credentials: %w", err)
	}
	return nil
}
