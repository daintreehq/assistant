package agenttaskx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// blockingMCP blocks CallTool until its context is done, then records the observed ctx
// error so a test can prove the roster read's bound surfaced as a CANCEL (not a
// deadline) — the property that keeps a slow agent.listAvailable from degrading the shared
// mcp.Client connection.
type blockingMCP struct{ ctxErr error }

func (m *blockingMCP) Connected() bool { return true }
func (m *blockingMCP) CallTool(ctx context.Context, _ string, _ map[string]any) (MCPCallResult, error) {
	<-ctx.Done()
	m.ctxErr = ctx.Err()
	return MCPCallResult{}, ctx.Err()
}

// errMCP returns a fixed transport error for any call (exercises the err!=nil fail-open).
type errMCP struct{}

func (errMCP) Connected() bool { return true }
func (errMCP) CallTool(context.Context, string, map[string]any) (MCPCallResult, error) {
	return MCPCallResult{}, errBoom("transport down")
}

// TestRegisteredAgentIDsTimeoutIsCancel asserts the roster read bounds itself with a
// CANCEL, not a deadline: on timeout the context error the MCP layer observes is
// context.Canceled (so mcp.Client does NOT degrade the connection on a slow read), and
// the caller falls open to nil. A non-nil transport error also fails open.
func TestRegisteredAgentIDsTimeoutIsCancel(t *testing.T) {
	defer func(prev time.Duration) { agentRosterTimeout = prev }(agentRosterTimeout)
	agentRosterTimeout = 20 * time.Millisecond

	b := &blockingMCP{}
	if got := RegisteredAgentIDs(context.Background(), b); got != nil {
		t.Fatalf("a timed-out roster read must fail open to nil, got %v", got)
	}
	if !errors.Is(b.ctxErr, context.Canceled) {
		t.Fatalf("roster timeout must surface as context.Canceled (so mcp.Client does not degrade), got %v", b.ctxErr)
	}

	if got := RegisteredAgentIDs(context.Background(), errMCP{}); got != nil {
		t.Fatalf("a transport error must fail open to nil, got %v", got)
	}
}

// agentRoster builds an agent.listAvailable structuredContent payload from ids.
func agentRoster(ids ...string) MCPCallResult {
	agents := make([]any, 0, len(ids))
	for _, id := range ids {
		agents = append(agents, map[string]any{"id": id})
	}
	return MCPCallResult{StructuredContent: map[string]any{"agents": agents}}
}

func agentRosterWithAvailability(states map[string]string) MCPCallResult {
	agents := make([]any, 0, len(states))
	for id, availability := range states {
		agents = append(agents, map[string]any{"id": id, "availability": availability})
	}
	return MCPCallResult{StructuredContent: map[string]any{"agents": agents}}
}

func TestRegisteredAgentIDsParsesBothSources(t *testing.T) {
	// structuredContent path, returned sorted.
	sc := &scriptMCP{connected: true, agentRoster: agentRoster("claude", "antigravity")}
	if got := RegisteredAgentIDs(context.Background(), sc); len(got) != 2 || got[0] != "antigravity" || got[1] != "claude" {
		t.Fatalf("structuredContent parse = %v, want [antigravity claude]", got)
	}
	// Text-body fallback (Daintree returns results in text).
	tx := &scriptMCP{connected: true, agentRoster: MCPCallResult{Text: `{"agents":[{"id":"codex"},{"id":"gemini"}]}`}}
	if got := RegisteredAgentIDs(context.Background(), tx); len(got) != 2 || got[0] != "codex" || got[1] != "gemini" {
		t.Fatalf("text parse = %v", got)
	}
}

func TestRegisteredAgentIDsFailsOpen(t *testing.T) {
	// An error, an IsError result, an empty map, and a bare result all yield nil so
	// the caller proceeds (a discovery hiccup must never block a spawn).
	cases := []*scriptMCP{
		{connected: true, agentRoster: MCPCallResult{IsError: true}},
		{connected: true, agentRoster: agentRoster()},
		{connected: true},
	}
	for i, m := range cases {
		if got := RegisteredAgentIDs(context.Background(), m); got != nil {
			t.Fatalf("case %d: want nil (fail open), got %v", i, got)
		}
	}
}

func TestResolveAgentID(t *testing.T) {
	roster := &scriptMCP{connected: true, agentRoster: agentRoster("claude", "codex", "antigravity")}
	if ok, _, _, _ := resolveAgentID(context.Background(), roster, "antigravity"); !ok {
		t.Error("a configured id must resolve ok")
	}
	ok, available, suggestion, unavailable := resolveAgentID(context.Background(), roster, "antiravity")
	if ok {
		t.Fatal("an unknown id must not resolve ok")
	}
	if unavailable != "" {
		t.Fatalf("unknown id reported unavailable state %q", unavailable)
	}
	if suggestion != "antigravity" {
		t.Errorf("suggestion = %q, want antigravity", suggestion)
	}
	if len(available) != 3 {
		t.Errorf("available = %v, want 3 entries", available)
	}
	// Empty roster ⇒ fail open.
	if ok, _, _, _ := resolveAgentID(context.Background(), &scriptMCP{connected: true}, "whatever"); !ok {
		t.Error("an unreadable roster must fail open")
	}
}

func TestResolveAgentIDSeparatesRegisteredFromLaunchable(t *testing.T) {
	for _, state := range []string{"missing", "installed", "blocked"} {
		t.Run(state, func(t *testing.T) {
			roster := &scriptMCP{connected: true, agentRoster: agentRosterWithAvailability(map[string]string{
				"claude": "ready", "codex": state, "gemini": "ready",
			})}
			ok, available, suggestion, unavailable := resolveAgentID(context.Background(), roster, "codex")
			if ok || unavailable != state || suggestion != "" {
				t.Fatalf("resolution = ok:%v unavailable:%q suggestion:%q", ok, unavailable, suggestion)
			}
			if strings.Join(available, ",") != "claude,codex,gemini" {
				t.Fatalf("registered roster was partial: %q", available)
			}
		})
	}
	for _, state := range []string{"ready", "unauthenticated"} {
		t.Run(state, func(t *testing.T) {
			roster := &scriptMCP{connected: true, agentRoster: agentRosterWithAvailability(map[string]string{"codex": state})}
			if ok, _, _, unavailable := resolveAgentID(context.Background(), roster, "codex"); !ok || unavailable != "" {
				t.Fatalf("launchable state %q did not resolve: ok=%v unavailable=%q", state, ok, unavailable)
			}
		})
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
	mcp := &scriptMCP{connected: true, agentRoster: agentRoster("claude", "antigravity")}
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
	mcp := &scriptMCP{connected: true, agentRoster: agentRoster("claude", "antigravity"), launchResult: launchOK("term_5")}
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

func TestSpawnRejectsRegisteredButUnavailableAgent(t *testing.T) {
	mcp := &scriptMCP{
		connected:   true,
		agentRoster: agentRosterWithAvailability(map[string]string{"codex": "missing"}),
	}
	st := newSagaStore()
	res := spawn(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
		AgentID: "codex", Mode: "edit", Title: "edit it", TaskPrompt: "go",
	})
	if res.Ok || res.Error.Code != codeAgentUnavailable {
		t.Fatalf("want AGENT_UNAVAILABLE, got %+v", res)
	}
	if mcp.launchCount() != 0 || len(st.launches) != 0 {
		t.Fatalf("unavailable agent reached launch/saga: launches=%d sagas=%d", mcp.launchCount(), len(st.launches))
	}
}
