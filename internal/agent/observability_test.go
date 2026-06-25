package agent

import (
	"strings"
	"testing"
)

// TestRecordToolFailureBucketsByName proves the session-cumulative tally accumulates
// per tool name across calls and is independent per name.
func TestRecordToolFailureBucketsByName(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if got := s.recordToolFailure("terminal.read"); got != 1 {
		t.Fatalf("first terminal.read failure count = %d, want 1", got)
	}
	if got := s.recordToolFailure("terminal.read"); got != 2 {
		t.Fatalf("second terminal.read failure count = %d, want 2", got)
	}
	if got := s.recordToolFailure("fs.read"); got != 1 {
		t.Fatalf("first fs.read failure count = %d, want 1 (buckets are per-name)", got)
	}
	counts := s.ToolFailureCounts()
	if counts["terminal.read"] != 2 || counts["fs.read"] != 1 {
		t.Fatalf("ToolFailureCounts = %v, want terminal.read:2 fs.read:1", counts)
	}
}

// TestToolFailureCountsZeroValue proves the getter is safe on a fresh session (the
// map is lazy-init'd in recordToolFailure, nil until the first failure).
func TestToolFailureCountsZeroValue(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if counts := s.ToolFailureCounts(); len(counts) != 0 {
		t.Fatalf("fresh session ToolFailureCounts = %v, want empty", counts)
	}
}

// TestToolFailureCountsReturnsCopy proves the getter hands back an independent copy:
// mutating it must not corrupt the session's internal tally.
func TestToolFailureCountsReturnsCopy(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.recordToolFailure("fs.read")
	snapshot := s.ToolFailureCounts()
	snapshot["fs.read"] = 999
	snapshot["injected"] = 7
	again := s.ToolFailureCounts()
	if again["fs.read"] != 1 {
		t.Fatalf("internal fs.read tally = %d, want 1 (caller mutation leaked back)", again["fs.read"])
	}
	if _, ok := again["injected"]; ok {
		t.Fatal("a key added to the returned copy leaked into the session map")
	}
}

// TestCompactionDepthCounts proves the depth getter tracks each compaction and resets
// on /clear (the compaction chain is destroyed).
func TestCompactionDepthCounts(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if got := s.CompactionDepth(); got != 0 {
		t.Fatalf("fresh session depth = %d, want 0", got)
	}
	s.Compact("first")
	s.Compact("second")
	if got := s.CompactionDepth(); got != 2 {
		t.Fatalf("depth after two compactions = %d, want 2", got)
	}
	s.Clear()
	if got := s.CompactionDepth(); got != 0 {
		t.Fatalf("depth after /clear = %d, want 0 (chain destroyed)", got)
	}
}

// TestCompactionNotePrefixEmbedsDepth proves the prefix tags the depth while keeping
// the "compacted summary" substring other code/tests key on.
func TestCompactionNotePrefixEmbedsDepth(t *testing.T) {
	p := compactionNotePrefix(3)
	if !strings.Contains(p, "compacted summary") {
		t.Fatalf("prefix %q lost the 'compacted summary' framing", p)
	}
	if !strings.Contains(p, "depth 3") {
		t.Fatalf("prefix %q does not embed the depth", p)
	}
}
