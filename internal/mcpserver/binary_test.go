package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// binary_test.go pins the answer to the one problem an MCP server has that a command
// does not: the client launches this process once and holds the pipe, so `make build`
// leaves the server running stale code with no way to say so. It cannot fix that — but
// it must not be mysterious about it.

// newTestBinaryInfo builds a BinaryInfo watching an arbitrary file, so the test does not
// have to replace the test binary itself.
func newTestBinaryInfo(t *testing.T, path, version string) *BinaryInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return &BinaryInfo{version: version, path: path, startSize: fi.Size(), startMod: fi.ModTime()}
}

func TestBinaryStalenessIsDetectedAndActionable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daintree-assistant")
	if err := os.WriteFile(path, []byte("original image"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := newTestBinaryInfo(t, path, "1.2.3")

	fresh := info.Snapshot()
	if fresh.Stale {
		t.Fatal("an untouched binary must not report stale")
	}
	if fresh.Version != "1.2.3" || fresh.Path != path {
		t.Errorf("snapshot = %+v", fresh)
	}

	// Simulate `make build`: the file is replaced with different content and a newer
	// mtime while this process keeps running the old image.
	if err := os.WriteFile(path, []byte("a rebuilt, longer image"), 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	// Defeat the throttle rather than sleeping through it.
	info.mu.Lock()
	info.lastCheck = time.Time{}
	info.mu.Unlock()

	stale := info.Snapshot()
	if !stale.Stale {
		t.Fatal("a replaced binary must report stale — otherwise a developer's fix silently does not take effect")
	}
	if stale.BuiltAt == "" {
		t.Error("a stale report must name the build time so the reader can confirm it is their build")
	}
	// The message must name the remedy: "stale" alone leaves the reader with nothing
	// to do, and session settings cannot fix it.
	msg := stale.StaleMessage()
	if !strings.Contains(msg, "Reconnect") {
		t.Errorf("the stale message must name the remedy, got %q", msg)
	}
	if !strings.Contains(msg, "1.2.3") {
		t.Errorf("the stale message must name the running version, got %q", msg)
	}
}

// TestStalenessIsThrottled: Snapshot runs on every session response, including inside a
// tight polling loop, so it must not stat the filesystem every time.
func TestStalenessIsThrottled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := newTestBinaryInfo(t, path, "test")
	if info.Snapshot().Stale {
		t.Fatal("baseline must be fresh")
	}
	// Replace it, then snapshot again immediately: within the throttle window the
	// cached answer stands.
	if err := os.WriteFile(path, []byte("yy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info.Snapshot().Stale {
		t.Error("a snapshot inside the throttle window must return the cached result")
	}
}

// TestUnresolvableExecutableDoesNotClaimFreshness: staleness is a diagnostic, and a
// server that refused to start because it could not find itself would be worse. But it
// must not assert a fact it cannot check — the empty path is how that shows.
func TestUnresolvableExecutableDoesNotClaimFreshness(t *testing.T) {
	info := &BinaryInfo{version: "test"} // no path, no baseline
	snap := info.Snapshot()
	if snap.Stale {
		t.Error("with no baseline there is nothing to compare, so stale must be false")
	}
	if snap.Path != "" {
		t.Errorf("path = %q, want empty so the absence of a check is visible", snap.Path)
	}
}
