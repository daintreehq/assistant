package agenttaskx

import (
	"testing"

	"github.com/daintreehq/assistant/internal/tools/worktreepin"
)

func TestFreshFactCohortUsesMessageWorktreeWithoutBorrowingClaude(t *testing.T) {
	ids := []string{"claude", "antigravity", "opencode", "grok", "codex"}
	pin := worktreepin.New()
	pin.Offer("wt-message", "/message", "message", true)
	mcp := &scriptMCP{
		connected:   true,
		agentRoster: agentRoster(ids...),
		listResult: terminalListResult(map[string]any{
			"id": "claude-user-work", "agentId": "claude", "name": "Claude: Fact",
			"worktreeId": "wt-original",
		}),
	}
	deps := Deps{MCP: mcp, DB: newSagaStore(), WorktreePin: pin, DaemonActive: func() bool { return true }}
	for _, id := range ids {
		mcp.launchResult = launchOK("new-" + id)
		a := spawnArgs{AgentID: id, Mode: "explore", Title: "Fact", TaskPrompt: "Give one unique fact off the top of your head, unrelated to this codebase. Do not explore files. Respond quickly."}
		if res := runSpawn(deps, a); !res.Ok {
			t.Fatalf("%s: %+v", id, res.Error)
		}
		if launch := mcp.lastLaunchArgs(); launch["worktreeId"] != "wt-message" || launch["agentId"] != id {
			t.Fatalf("%s launch: %+v", id, launch)
		}
	}
	if mcp.launchCount() != len(ids) {
		t.Fatalf("launched %d agents", mcp.launchCount())
	}
	for _, call := range mcp.calls {
		if call.name == "terminal.sendCommand" || call.name == "terminal.write" {
			t.Fatalf("borrowed a terminal: %+v", call)
		}
	}
	// Explicit work elsewhere (including a job follow-up) still overrides the default.
	mcp.launchResult = launchOK("explicit")
	a := spawnArgs{AgentID: "claude", Mode: "explore", Title: "Explicit task", TaskPrompt: "Follow up", WorktreeID: "wt-original"}
	if res := runSpawn(deps, a); !res.Ok {
		t.Fatal(res.Error)
	}
	if mcp.lastLaunchArgs()["worktreeId"] != "wt-original" {
		t.Fatal("explicit target was overridden")
	}
}
