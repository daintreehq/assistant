package agenttaskx

import (
	"context"
	"testing"
)

// agentRoster builds an agentSettings.get structuredContent payload from ids.
func agentRoster(ids ...string) MCPCallResult {
	agents := map[string]any{}
	for _, id := range ids {
		agents[id] = map[string]any{}
	}
	return MCPCallResult{StructuredContent: map[string]any{"agents": agents}}
}

func TestConfiguredAgentIDsParsesBothSources(t *testing.T) {
	// structuredContent path, returned sorted.
	sc := &scriptMCP{connected: true, agentSettings: agentRoster("claude", "antigravity")}
	if got := configuredAgentIDs(context.Background(), sc); len(got) != 2 || got[0] != "antigravity" || got[1] != "claude" {
		t.Fatalf("structuredContent parse = %v, want [antigravity claude]", got)
	}
	// Text-body fallback (Daintree returns results in text).
	tx := &scriptMCP{connected: true, agentSettings: MCPCallResult{Text: `{"agents":{"codex":{},"gemini":{}}}`}}
	if got := configuredAgentIDs(context.Background(), tx); len(got) != 2 || got[0] != "codex" || got[1] != "gemini" {
		t.Fatalf("text parse = %v", got)
	}
}

func TestConfiguredAgentIDsFailsOpen(t *testing.T) {
	// An error, an IsError result, an empty map, and a bare result all yield nil so
	// the caller proceeds (a discovery hiccup must never block a spawn).
	cases := []*scriptMCP{
		{connected: true, agentSettings: MCPCallResult{IsError: true}},
		{connected: true, agentSettings: agentRoster()},
		{connected: true},
	}
	for i, m := range cases {
		if got := configuredAgentIDs(context.Background(), m); got != nil {
			t.Fatalf("case %d: want nil (fail open), got %v", i, got)
		}
	}
}

func TestResolveAgentID(t *testing.T) {
	roster := &scriptMCP{connected: true, agentSettings: agentRoster("claude", "codex", "antigravity")}
	if ok, _, _ := resolveAgentID(context.Background(), roster, "antigravity"); !ok {
		t.Error("a configured id must resolve ok")
	}
	ok, available, suggestion := resolveAgentID(context.Background(), roster, "antiravity")
	if ok {
		t.Fatal("an unknown id must not resolve ok")
	}
	if suggestion != "antigravity" {
		t.Errorf("suggestion = %q, want antigravity", suggestion)
	}
	if len(available) != 3 {
		t.Errorf("available = %v, want 3 entries", available)
	}
	// Empty roster ⇒ fail open.
	if ok, _, _ := resolveAgentID(context.Background(), &scriptMCP{connected: true}, "whatever"); !ok {
		t.Error("an unreadable roster must fail open")
	}
}

func TestClosestAgentID(t *testing.T) {
	cands := []string{"claude", "codex", "antigravity", "gemini"}
	if got := closestAgentID("antiravity", cands); got != "antigravity" {
		t.Errorf("antiravity -> %q, want antigravity", got)
	}
	if got := closestAgentID("codx", cands); got != "codex" {
		t.Errorf("codx -> %q, want codex", got)
	}
	// A wholly unrelated string is beyond the near-miss threshold ⇒ no suggestion.
	if got := closestAgentID("zzzzzzzzzz", cands); got != "" {
		t.Errorf("unrelated -> %q, want empty", got)
	}
}

// The spawn gate rejects a mis-transcribed agent id BEFORE any agent.launch or saga
// write — the regression that launched a dead "antiravity" terminal.
func TestSpawnRejectsUnknownAgent(t *testing.T) {
	mcp := &scriptMCP{connected: true, agentSettings: agentRoster("claude", "antigravity")}
	st := newSagaStore()
	res := spawn(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
		AgentID: "antiravity", Mode: "explore", Title: "explore it", TaskPrompt: "go",
	})
	if res.Ok || res.Error.Code != codeUnknownAgent {
		t.Fatalf("want UNKNOWN_AGENT, got %+v", res)
	}
	if mcp.launchCount() != 0 {
		t.Fatal("must not call agent.launch for an unknown agent")
	}
	if len(st.launches) != 0 {
		t.Fatal("must not write a saga record for an unknown agent")
	}
	details, _ := res.Error.Details.(map[string]any)
	if details["suggestion"] != "antigravity" {
		t.Fatalf("expected a did-you-mean suggestion, got %+v", details)
	}
}

// A correctly-named agent passes the gate and proceeds to launch.
func TestSpawnAllowsConfiguredAgent(t *testing.T) {
	mcp := &scriptMCP{connected: true, agentSettings: agentRoster("claude", "antigravity"), launchResult: launchOK("term_5")}
	st := newSagaStore()
	res := spawn(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
		AgentID: "antigravity", Mode: "explore", Title: "explore it", TaskPrompt: "go",
	})
	if !res.Ok {
		t.Fatalf("configured agent must spawn, got %+v", res.Error)
	}
	if mcp.lastLaunchArgs()["agentId"] != "antigravity" {
		t.Fatalf("launched wrong agent: %v", mcp.lastLaunchArgs()["agentId"])
	}
}
