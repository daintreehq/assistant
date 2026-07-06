package contextx

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestSummarizeParallelizable locks terminal.summarize's concurrency opt-in. It is the
// taught DEFAULT for relaying agent output and carries the same cost profile as
// terminal.extract (a small-model call, seconds each), so a cohort relay (one
// summarize per agent in one batch) must dispatch concurrently — leaving it serial
// stacked N backend round-trips (the exact regression a real session hit: five
// summarize calls at ~2s each running back-to-back). It has no wait/barrier mode, so
// the opt-in is unconditionally safe. terminal.read stays NOT opted in for now — a
// raw MCP read is milliseconds, so there is nothing to win.
func TestSummarizeParallelizable(t *testing.T) {
	summarize := newSummarizeTool(Deps{})
	if !summarize.Parallelizable {
		t.Error("terminal.summarize must be Parallelizable (independent per-call snapshot read + small-model call)")
	}
	// Double-gate invariant: an opted-in tool must be read-risk, or the runner's
	// RiskRead gate would (correctly) refuse to parallelize it.
	if summarize.Risk != domain.RiskRead {
		t.Errorf("terminal.summarize is Parallelizable but risk=%s, want read (double-gate would reject it)", summarize.Risk)
	}
	// Pin the deliberate exclusion too (mirrors extractionx pinning awaitAll): a raw
	// MCP read is milliseconds, so parallelizing terminal.read buys nothing — flipping
	// it should be a conscious decision, not a drive-by.
	read := newReadTool(Deps{})
	if read.Parallelizable {
		t.Error("terminal.read must NOT be Parallelizable yet — deliberate exclusion; opt it in consciously if ever needed")
	}
}
