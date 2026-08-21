package mcpserver

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// binary.go answers a problem that is specific to being an MCP server rather than a
// command: an MCP client launches this process once and holds the pipe for its whole
// session. It has no way to restart the server when the developer runs `make build`,
// and MCP has no "I have been replaced, please reconnect" notification.
//
// So the process cannot fix the situation — but it can refuse to be MYSTERIOUS about
// it. Every session response reports whether the executable on disk has changed since
// this process started, which turns "why is my fix not taking effect" into a stated
// fact the caller can act on.
//
// Everything else a client might want to change — project, backend endpoint, tier, MCP
// credentials, a fresh conversation — is deliberately a per-session argument rather than
// server state, so it needs no restart at all. A stale binary is the ONLY condition that
// genuinely requires reconnecting.

// BinaryInfo watches this process's own executable for replacement.
type BinaryInfo struct {
	version string
	path    string
	// startSize/startMod are the identity of the image we are RUNNING. A rebuild
	// replaces the file, so either changing means the file on disk is no longer us.
	startSize int64
	startMod  time.Time

	mu         sync.Mutex
	lastCheck  time.Time
	lastResult ServerInfo
}

// ServerInfo is the reportable state of the server process.
type ServerInfo struct {
	Version string `json:"version"`
	Path    string `json:"path,omitempty"`
	// Stale is true when the executable on disk differs from the running image.
	Stale bool `json:"staleBinary"`
	// BuiltAt is the on-disk executable's modification time when stale, so the reader
	// can confirm it matches the build they just ran.
	BuiltAt string `json:"binaryBuiltAt,omitempty"`
}

// StaleMessage is the warning text for a replaced binary. It names the remedy, because
// "stale" alone leaves the reader with nothing to do.
func (s ServerInfo) StaleMessage() string {
	return fmt.Sprintf(
		"The daintree-assistant binary was rebuilt at %s but this MCP server is still running the older image (version %q). "+
			"Reconnect the MCP server to pick up the new build; session settings alone will not.", s.BuiltAt, s.Version)
}

// staleCheckInterval throttles the stat. A tool response should not stat the filesystem
// on every call in a tight polling loop, and a rebuild noticed a few seconds late costs
// nothing.
const staleCheckInterval = 5 * time.Second

// NewBinaryInfo captures the running image's identity. A failure to resolve or stat the
// executable is not fatal — staleness is a diagnostic, and a server that refused to
// start because it could not find itself would be strictly worse.
func NewBinaryInfo(version string) *BinaryInfo {
	b := &BinaryInfo{version: version}
	path, err := os.Executable()
	if err != nil {
		return b
	}
	b.path = path
	if fi, err := os.Stat(path); err == nil {
		b.startSize = fi.Size()
		b.startMod = fi.ModTime()
	}
	return b
}

// Snapshot reports the server's state, re-stating the executable at most once per
// staleCheckInterval.
func (b *BinaryInfo) Snapshot() ServerInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if !b.lastCheck.IsZero() && now.Sub(b.lastCheck) < staleCheckInterval {
		return b.lastResult
	}
	b.lastCheck = now

	info := ServerInfo{Version: b.version, Path: b.path}
	// No baseline (unresolvable executable) means we cannot tell, and claiming "fresh"
	// would be a guess dressed as a fact. Report not-stale but keep the path empty so
	// the absence is visible.
	if b.path != "" && !b.startMod.IsZero() {
		if fi, err := os.Stat(b.path); err == nil {
			if fi.Size() != b.startSize || !fi.ModTime().Equal(b.startMod) {
				info.Stale = true
				info.BuiltAt = fi.ModTime().Format(time.RFC3339)
			}
		}
	}
	b.lastResult = info
	return info
}
