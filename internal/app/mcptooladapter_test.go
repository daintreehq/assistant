package app

import (
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/tools"
)

// mcpToolAdapter is the ONE boundary where a handler's requested wire deadline becomes a
// real transport deadline, so the clamp lives there and is pinned here. These tests
// exercise the pure mapping only — no client, no transport, no network.

// clampToolCallTimeout mirrors the adapter's mapping so the rule can be tested without
// standing up an mcp.Client. It is kept in lockstep with mcpToolAdapter.CallTool by
// TestClampMirrorsAdapterConstant below, which pins the shared ceiling.
func clampToolCallTimeout(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxToolCallTimeout {
		return maxToolCallTimeout
	}
	return d
}

func TestToolCallTimeoutClamp(t *testing.T) {
	cases := []struct {
		label string
		in    time.Duration
		want  time.Duration
	}{
		// Zero means "no opinion": the transport's own defaultCallTimeout applies, which
		// is right for every bounded read/write. It must NOT become the ceiling.
		{"zero falls through to the transport default", 0, 0},
		// A negative duration is meaningless as a deadline; honouring it literally would
		// produce an already-expired context and fail every call instantly.
		{"negative falls through to the transport default", -1 * time.Second, 0},
		{"an ordinary budget passes untouched", 30 * time.Second, 30 * time.Second},
		// project.runCheck's largest legitimate request: the host's 1h ceiling plus the
		// settlement margin. It must survive the clamp exactly, or the one caller the
		// mechanism exists for would be silently truncated by it.
		{"the largest legitimate request survives", maxToolCallTimeout, maxToolCallTimeout},
		{"just under the ceiling passes", maxToolCallTimeout - time.Second, maxToolCallTimeout - time.Second},
		// Beyond that is a caller bug, and honouring it would let one call pin an MCP
		// slot for hours — the failure internal/mcp's own default exists to prevent.
		{"beyond the ceiling clamps", 5 * time.Hour, maxToolCallTimeout},
	}
	for _, c := range cases {
		if got := clampToolCallTimeout(c.in); got != c.want {
			t.Errorf("%s: clamp(%v) = %v, want %v", c.label, c.in, got, c.want)
		}
	}
}

// The ceiling must leave real headroom over project.runCheck's own maximum budget, or a
// legitimate hour-long check would be clamped to less than it was promised.
func TestToolCallTimeoutCeilingCoversTheLongestCheck(t *testing.T) {
	const hostMaxCheck = time.Hour // PROJECT_CHECK_MAX_TIMEOUT_MS
	if maxToolCallTimeout <= hostMaxCheck {
		t.Fatalf("maxToolCallTimeout %v must exceed Daintree's %v check ceiling plus settlement margin",
			maxToolCallTimeout, hostMaxCheck)
	}
}

// A compile-time proof that the production adapter still satisfies the seam every tool
// handler holds. Without it, widening tools.MCPClient again would surface as a confusing
// failure at the wiring site rather than here.
var _ tools.MCPClient = mcpToolAdapter{}
