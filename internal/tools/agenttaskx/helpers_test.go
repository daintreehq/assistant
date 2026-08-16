package agenttaskx

import (
	"strings"
	"testing"
)

func TestComputeIdempotencyKeyMirrorsForwardedArgs(t *testing.T) {
	k1 := computeIdempotencyKey("do the thing", "wt1", "claude", "Claude: x")
	k2 := computeIdempotencyKey("do the thing", "wt1", "claude", "Claude: x")
	if k1 != k2 {
		t.Fatalf("key not deterministic: %s vs %s", k1, k2)
	}
	if len(k1) != 16 {
		t.Fatalf("key length = %d, want 16", len(k1))
	}
	// The name (derived from the title) is a forwarded arg, so it MUST change the key —
	// the regression that walled retries was a name change reusing the same requestKey.
	if computeIdempotencyKey("do the thing", "wt1", "claude", "Claude: y") == k1 {
		t.Error("name should affect the key")
	}
	// A different composed prompt changes the key.
	if computeIdempotencyKey("do another thing", "wt1", "claude", "Claude: x") == k1 {
		t.Error("prompt should affect the key")
	}
	// A different agent changes the key.
	if computeIdempotencyKey("do the thing", "wt1", "codex", "Claude: x") == k1 {
		t.Error("agentId should affect the key")
	}
	// An empty worktree must be canonical (the handler normalizes "" the same way).
	if computeIdempotencyKey("t", "", "claude", "n") == computeIdempotencyKey("t", "wt", "claude", "n") {
		t.Error("worktree should affect the key")
	}
}

func TestBuildAgentLaunchNamePrefixSurvivesCap(t *testing.T) {
	long := strings.Repeat("x", 200)
	name := buildAgentLaunchName(long, "claude")
	if len(name) > agentLaunchNameMaxLen {
		t.Fatalf("name exceeds cap: %d", len(name))
	}
	if !strings.HasPrefix(name, "Claude: ") {
		t.Fatalf("prefix lost: %q", name)
	}
	// Whitespace is collapsed and a blank title falls back to "task".
	if got := buildAgentLaunchName("  a\t b  ", "claude"); got != "Claude: a b" {
		t.Errorf("ws collapse = %q", got)
	}
	if got := buildAgentLaunchName("   ", "claude"); got != "Claude: task" {
		t.Errorf("blank fallback = %q", got)
	}
	// agentId is capitalized including the default.
	if got := buildAgentLaunchName("y", "codex"); got != "Codex: y" {
		t.Errorf("agentId prefix = %q", got)
	}
}

// Every surface that describes `title` shows the RENDERED tab label, so the model
// writes the agent name into the title itself and the wrapper prefixes it again —
// "Claude: Claude: prs merge target" (issue #337). Exactly one prefix must survive,
// and only for a leading label that really is this agent's.
func TestBuildAgentLaunchNameStripsRedundantAgentPrefix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		title   string
		agentID string
		want    string
	}{
		// The verbatim ses_6738e54c regression.
		{"exact self-prefix", "Claude: prs merge target", "claude", "Claude: prs merge target"},
		{"any casing", "cLaUdE: prs merge target", "claude", "Claude: prs merge target"},
		{"no space after colon", "Claude:prs merge target", "claude", "Claude: prs merge target"},
		{"messy whitespace", " \tCLAUDE:\n prs\t merge  target ", "claude", "Claude: prs merge target"},
		{"repeated prefixes collapse", "Claude: claude:CLAUDE: foo", "claude", "Claude: foo"},
		{"prefix only falls back to task", "Claude:", "claude", "Claude: task"},
		{"default agent still strips", "CLAUDE: y", "", "Claude: y"},
		{"non-default agent strips its own", "Codex: y", "codex", "Codex: y"},

		// Narrowness: these are plausible task text, not the wrapper's syntax.
		{"another agent's name is kept", "Claude: foo", "codex", "Codex: Claude: foo"},
		{"no colon is not a prefix", "Claude foo", "claude", "Claude: Claude foo"},
		{"space before the colon is not a prefix", "Claude : foo", "claude", "Claude: Claude : foo"},
		{"a later colon is untouched", "fix the claude: parser", "claude", "Claude: fix the claude: parser"},
	} {
		if got := buildAgentLaunchName(tc.title, tc.agentID); got != tc.want {
			t.Errorf("%s: buildAgentLaunchName(%q, %q) = %q, want %q", tc.name, tc.title, tc.agentID, got, tc.want)
		}
	}

	// The stripped title rejoins the SAME add-and-truncate path an unprefixed one
	// takes — one cap implementation, so an over-long prefixed title can never be
	// double-cut or lose its prefix.
	long := strings.Repeat("x", 200)
	if prefixed, bare := buildAgentLaunchName("Claude: "+long, "claude"), buildAgentLaunchName(long, "claude"); prefixed != bare {
		t.Errorf("prefixed long title = %q, want identical to bare %q", prefixed, bare)
	}
}

func TestExtractFieldSources(t *testing.T) {
	// Direct structuredContent field.
	if got := extractField(MCPCallResult{StructuredContent: map[string]any{"terminalId": "t1"}}, "terminalId"); got != "t1" {
		t.Errorf("direct = %q", got)
	}
	// Nested under task.
	nested := MCPCallResult{StructuredContent: map[string]any{"task": map[string]any{"terminalId": "t2"}}}
	if got := extractField(nested, "terminalId"); got != "t2" {
		t.Errorf("nested = %q", got)
	}
	// Text-body regex fallback.
	if got := extractField(MCPCallResult{Text: `started terminalId: term_3a here`}, "terminalId"); got != "term_3a" {
		t.Errorf("text regex = %q", got)
	}
	// Absent everywhere.
	if got := extractField(MCPCallResult{Text: "nothing"}, "terminalId"); got != "" {
		t.Errorf("absent = %q, want empty", got)
	}
}

func TestParseTerminalListBothSources(t *testing.T) {
	// structuredContent terminals + text terminals are merged; id/terminalId both accepted.
	res := MCPCallResult{
		StructuredContent: map[string]any{"terminals": []any{
			map[string]any{"id": "a", "name": "Claude: x", "agentId": "claude", "worktreeId": "wt"},
		}},
		Text: `{"terminals":[{"terminalId":"b","name":"Claude: y"}]}`,
	}
	got := parseTerminalList(res)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].id != "a" || got[0].name != "Claude: x" || got[1].id != "b" {
		t.Errorf("parsed wrong: %+v", got)
	}
}
