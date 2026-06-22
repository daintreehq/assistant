package agenttaskx

import (
	"strings"
	"testing"
)

func TestComputeIdempotencyKeyDeterministicAndIdentityScoped(t *testing.T) {
	k1 := computeIdempotencyKey("do the thing", "wt1", "claude", "edit")
	k2 := computeIdempotencyKey("do the thing", "wt1", "claude", "edit")
	if k1 != k2 {
		t.Fatalf("key not deterministic: %s vs %s", k1, k2)
	}
	if len(k1) != 16 {
		t.Fatalf("key length = %d, want 16", len(k1))
	}
	// A different mode changes the key.
	if computeIdempotencyKey("do the thing", "wt1", "claude", "explore") == k1 {
		t.Error("mode should affect the key")
	}
	// An empty worktree must be canonical (the handler normalizes "" the same way).
	if computeIdempotencyKey("t", "", "claude", "edit") == computeIdempotencyKey("t", "wt", "claude", "edit") {
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
