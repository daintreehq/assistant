package composer

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestRenderHintsSuppressedDuringSearch covers issue #212: during reverse-i-search the
// slash palette is suppressed but the hint row beneath still advertised "/" and "↑" as live
// keys. renderInput already swaps the input line for the reverse-i-search line, so the hint
// row is stale and must be hidden while searching.
func TestRenderHintsSuppressedDuringSearch(t *testing.T) {
	m := newModel()
	p := ViewParams{Width: 80}

	// Not searching: a hint row is present.
	if got := m.renderHints(p); got == "" {
		t.Fatal("expected a non-empty hint row when not searching")
	}

	// Reverse-i-search active: the hint row is fully suppressed, and the search line shows.
	m.searching = true
	if got := m.renderHints(p); got != "" {
		t.Fatalf("hint row must be empty during reverse-i-search, got %q", ansi.Strip(got))
	}
	if view := ansi.Strip(m.View(p)); !strings.Contains(view, "reverse-i-search") {
		t.Fatalf("search view should render the reverse-i-search line, got %q", view)
	}
}
