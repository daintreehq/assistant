package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A simple table must render as an ALIGNED GRID (column separators present), not be
// flattened to a "Label: value" record list, and every line must fit the cockpit
// content width. This locks in the native-table behavior (we used to flatten all
// tables); the base prompt is what keeps the model to tables this small.
func TestSimpleTableRendersAsGrid(t *testing.T) {
	const width = 56
	src := strings.Join([]string{
		"| Agent | Mine | Peer | Total |",
		"|-------|------|------|-------|",
		"| Codex | 1 | 2 | 3 |",
		"| AntiGravity | 0 | 2 | 2 |",
		"| OpenCode | 1 | 0 | 1 |",
		"| Claude | 0 | 0 | 0 |",
	}, "\n")

	out := New(noColorTheme()).Render(src, width, false).Plain

	if !strings.Contains(out, "│") {
		t.Fatalf("expected an aligned grid with column separators, got:\n%s", out)
	}
	// A flattened record list would emit "Total: 3" label lines; a real table never does.
	if strings.Contains(out, "Total: ") {
		t.Fatalf("table was flattened to a record list, want a grid:\n%s", out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(ln); w > width {
			t.Fatalf("line exceeds content width %d (got %d): %q", width, w, ln)
		}
	}
}
