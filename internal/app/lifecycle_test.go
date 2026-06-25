package app

import (
	"context"
	"strings"
	"testing"
)

// TestParseCurrentWorktreeLabel covers how a worktree.getCurrent result becomes the
// message[1] label across the response shapes Daintree can return: a structured object
// (branch preferred, then id, then path), an explicit null (→ the no-worktree sentinel),
// the JSON text-body fallback Daintree often uses, and absent/malformed payloads (→ "",
// which the renderer turns into the "(unknown)" placeholder).
func TestParseCurrentWorktreeLabel(t *testing.T) {
	wt := func(fields map[string]any) any { return map[string]any{"worktree": fields} }

	cases := []struct {
		name       string
		structured any
		text       string
		want       string
	}{
		{"branch preferred", wt(map[string]any{"branch": "feature/x", "id": "wt_1", "path": "/p"}), "", "feature/x"},
		{"id fallback when no branch", wt(map[string]any{"id": "wt_1", "path": "/p"}), "", "wt_1"},
		{"path fallback when no branch/id", wt(map[string]any{"path": "/p/q"}), "", "/p/q"},
		{"explicit null structured", map[string]any{"worktree": nil}, "", noActiveWorktreeLabel},
		{"text body branch wins", nil, `{"worktree":{"branch":"main","id":"wt_2"}}`, "main"},
		{"text body id fallback", nil, `{"worktree":{"id":"wt_3"}}`, "wt_3"},
		{"text body null", nil, `{"worktree":null}`, noActiveWorktreeLabel},
		{"absent key structured", map[string]any{"other": 1}, "", ""},
		{"empty everything", nil, "", ""},
		{"malformed text", nil, "not json", ""},
		{"whitespace-only fields fall through to empty", wt(map[string]any{"branch": "  ", "id": " "}), "", ""},
		{"structured empty-label falls to text body", wt(map[string]any{}), `{"worktree":{"branch":"dev"}}`, "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCurrentWorktreeLabel(tc.structured, tc.text); got != tc.want {
				t.Errorf("parseCurrentWorktreeLabel(%v, %q) = %q, want %q", tc.structured, tc.text, got, tc.want)
			}
		})
	}
}

// TestRefreshStartupContextNotConnectedClears asserts the not-connected refresh clears
// the startup cache, so message[1] drops stale configured-agents / worktree facts when a
// session goes degraded rather than carrying a prior connection's roster forward.
func TestRefreshStartupContextNotConnectedClears(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.rosterMu.Lock()
	a.cachedAgentIDs = []string{"claude"}
	a.cachedActiveWorktree = "feature/x"
	a.rosterMu.Unlock()

	a.refreshStartupContext(context.Background(), false)

	a.rosterMu.RLock()
	gotIDs, gotWt := a.cachedAgentIDs, a.cachedActiveWorktree
	a.rosterMu.RUnlock()
	if gotIDs != nil || gotWt != "" {
		t.Fatalf("not-connected refresh must clear the cache, got ids=%v wt=%q", gotIDs, gotWt)
	}

	// And it propagates: after RefreshRuntimeContext, message[1] drops the stale roster
	// line. The worktree is no longer in message[1] (it moved to the footer, issue #263),
	// so the cleared cache simply makes the footer seam report "" until the next connect.
	a.Session.RefreshRuntimeContext(a.PromptContext())
	msg := a.Session.Messages()[1].ContentToText()
	if strings.Contains(msg, "Configured agents:") {
		t.Fatalf("degraded message[1] must not carry a stale roster line:\n%s", msg)
	}
	if strings.Contains(msg, "Active worktree:") {
		t.Fatalf("worktree must not appear in message[1] (it moved to the footer):\n%s", msg)
	}
	if got := a.activeWorktreeForFooter(); got != "" {
		t.Fatalf("a cleared cache must make the footer worktree seam empty, got %q", got)
	}
}
