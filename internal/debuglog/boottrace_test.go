package debuglog

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// resetBootTrace clears the once-latched env resolution so each test observes its
// own DAINTREE_ASSISTANT_BOOT_TRACE value.
func resetBootTrace() {
	bootTraceOnce = sync.Once{}
	bootTracePath = ""
}

func TestBootTraceDisabledWritesNothing(t *testing.T) {
	resetBootTrace()
	t.Setenv("DAINTREE_ASSISTANT_BOOT_TRACE", "")
	BootTrace("phase.a") // must be a pure no-op — nothing to assert beyond not panicking
}

func TestBootTraceAppendsPhaseLines(t *testing.T) {
	resetBootTrace()
	path := filepath.Join(t.TempDir(), "trace.tsv")
	t.Setenv("DAINTREE_ASSISTANT_BOOT_TRACE", path)

	BootTrace("phase.a")
	BootTrace("phase.b")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	var lastMs int64 = -1
	for i, want := range []string{"phase.a", "phase.b"} {
		ms, phase, ok := strings.Cut(lines[i], "\t")
		if !ok || phase != want {
			t.Fatalf("line %d = %q, want ms\\t%s", i, lines[i], want)
		}
		n, err := strconv.ParseInt(ms, 10, 64)
		if err != nil || n < 0 {
			t.Fatalf("line %d ms field %q not a non-negative int", i, ms)
		}
		if n < lastMs {
			t.Fatalf("timestamps must be monotonic: %d then %d", lastMs, n)
		}
		lastMs = n
	}
}
