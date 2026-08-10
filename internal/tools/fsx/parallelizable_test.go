package fsx

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// TestFSReadsParallelizable locks the concurrency opt-in for the read-only filesystem
// family. fs.read/fs.list are independent, BOUNDED snapshot reads with no ordering
// dependency on their batch siblings, and — unlike a DB read serialized behind the
// single-connection store — their disk I/O genuinely overlaps. So a batch of them (the
// common "read these five files" burst) must dispatch concurrently instead of stacking
// one syscall round at a time, exactly like terminal.extract. Every parallelizable tool
// must also be RiskRead, or the runner's double-gate (ParallelSafe) would (correctly)
// refuse to parallelize it.
func TestFSReadsParallelizable(t *testing.T) {
	cases := []struct {
		label string
		par   bool
		risk  domain.RiskClass
	}{
		{"fs.list", newListTool().Parallelizable, newListTool().Risk},
		{"fs.read", newReadTool().Parallelizable, newReadTool().Risk},
	}
	for _, c := range cases {
		if !c.par {
			t.Errorf("%s must be Parallelizable (independent, genuinely-concurrent filesystem read)", c.label)
		}
		if c.risk != domain.RiskRead {
			t.Errorf("%s is Parallelizable but risk=%s, want read (double-gate would reject it)", c.label, c.risk)
		}
	}
}

// TestFSSearchNotParallelizable pins the deliberate exclusion (mirrors extractionx
// pinning awaitAll and contextx pinning terminal.read). fs.search is a full recursive
// project walk that reads file contents and ignores ctx — a heavy, cancellation-blind
// scan. Running several concurrently would redundantly re-walk the whole tree with no
// real overlap win, so it stays serial; opting it in should be a conscious decision
// backed by ctx-aware/limited scanning, not a drive-by.
func TestFSSearchNotParallelizable(t *testing.T) {
	if newSearchTool().Parallelizable {
		t.Error("fs.search must NOT be Parallelizable — a full ctx-unaware recursive walk is a poor parallel citizen (redundant re-walks)")
	}
}
