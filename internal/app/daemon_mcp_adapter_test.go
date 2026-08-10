package app

import (
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/daemon"
)

// TestDaemonReadCallOptions asserts the daemon's read-only MCP seam is wired with
// the documented cadence: a 20s per-attempt timeout and 2 retries (CODE-REVIEW MED
// fix). Without these a watcher/PR-watcher read could hang until the outer tick ctx
// and fail on the first transient transport blip. The values must track the daemon
// cadence constants, not a hardcoded duplicate.
func TestDaemonReadCallOptions(t *testing.T) {
	opts := daemonReadCallOptions()

	wantTimeout := 20 * time.Second
	if opts.Timeout != wantTimeout {
		t.Errorf("read CallOptions.Timeout = %v, want %v", opts.Timeout, wantTimeout)
	}
	if opts.Retries != 2 {
		t.Errorf("read CallOptions.Retries = %d, want 2", opts.Retries)
	}

	// Tie the assertion to the daemon constants so a change to either side is caught.
	if opts.Timeout != time.Duration(daemon.McpReadTimeoutMS)*time.Millisecond {
		t.Errorf("Timeout %v should equal daemon.McpReadTimeoutMS (%dms)", opts.Timeout, daemon.McpReadTimeoutMS)
	}
	if opts.Retries != daemon.McpReadMaxRetries {
		t.Errorf("Retries %d should equal daemon.McpReadMaxRetries (%d)", opts.Retries, daemon.McpReadMaxRetries)
	}
}
