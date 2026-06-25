package prompts

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// The base prompt is the cached prefix and must remain byte-stable. These anchor
// checks fail loudly if the text drifts (catching an accidental edit), and verify
// the "\n\n" joiner between identity and the MCP reference.
func TestBaseSystemPromptStable(t *testing.T) {
	if !strings.HasPrefix(BaseSystemPrompt, "You are the **Daintree Assistant** — Daintree's local operations officer.") {
		t.Fatal("base prompt prefix drifted")
	}
	if !strings.Contains(BaseSystemPrompt, "\n\n# Daintree integration reference (verified)") {
		t.Fatal("MCP reference joiner drifted")
	}
	// The negative-example tool names must survive verbatim.
	if !strings.Contains(BaseSystemPrompt, "There is NO terminal.listStatus and NO terminal.waitForAny.") {
		t.Fatal("negative-example tools missing")
	}
	if !strings.Contains(BaseSystemPrompt, "Never format output as a markdown table.") {
		t.Fatal("no-table rule missing")
	}
}

func TestDocumentedMCPToolNames(t *testing.T) {
	// 60 verified Daintree MCP tool names (agentSettings.get was added so the spawn
	// wrapper can validate agentId against the configured roster; terminal.close backs
	// the terminal.close wrapper that retires spawned cohorts; terminal.rename backs the
	// terminal.rename wrapper that retitles terminals). Match verbatim.
	if len(DocumentedMCPToolNames) != 60 {
		t.Fatalf("got %d tool names, want 60", len(DocumentedMCPToolNames))
	}
	if DocumentedMCPToolNames[0] != "actions.getContext" || DocumentedMCPToolNames[59] != "worktree.list" {
		t.Fatal("tool name order drifted")
	}
}

func TestBuildRuntimeContextMessage(t *testing.T) {
	ctx := MainPromptContext{
		Tier: domain.TierOperator, ProjectPath: "/p", MCPConnected: true,
		MCPStatusLine: "connected", LargeModel: "L", SmallModel: "S",
		SchedulerActive: true,
	}
	out := BuildRuntimeContextMessage(ctx)
	if !strings.HasPrefix(out, "# Runtime context\n") {
		t.Fatal("header missing")
	}
	if !strings.Contains(out, "Project id: (none)") {
		t.Fatal("empty project id fallback missing")
	}
	if !strings.Contains(out, "Models: large=L, small=S") {
		t.Fatal("models line missing")
	}
	// connected + scheduler active → no degraded/dormant notes.
	if strings.Contains(out, "NOTE:") {
		t.Fatal("unexpected NOTE when fully connected/active")
	}
	// Volatile mid-session state moved OUT of message[1] into the uncached footer
	// (issue #263): the runtime context must no longer carry the worktree line, the
	// pinned-memory block, or the session-ended-watchers NOTE — those would bust the
	// prefix cache on every change. They are tested in internal/agent/footer_test.go now.
	withVolatile := BuildRuntimeContextMessage(MainPromptContext{
		Tier: domain.TierOperator, MCPConnected: true, SchedulerActive: true,
	})
	for _, banned := range []string{"Active worktree:", "# Pinned memories", "previous session ended"} {
		if strings.Contains(withVolatile, banned) {
			t.Fatalf("message[1] must not carry volatile state %q (it moved to the footer):\n%s", banned, withVolatile)
		}
	}

	degraded := BuildRuntimeContextMessage(MainPromptContext{Tier: domain.TierSystem})
	if !strings.Contains(degraded, "Daintree MCP is NOT connected") {
		t.Fatal("degraded note missing")
	}
	if !strings.Contains(degraded, "the scheduler is NOT running") {
		t.Fatal("dormant note missing")
	}

	withInstr := BuildRuntimeContextMessage(MainPromptContext{
		Tier: domain.TierSupervisor, MCPConnected: true, SchedulerActive: true,
		ProjectInstructions: "BE NICE",
	})
	if !strings.Contains(withInstr, "# Project instructions\n") || !strings.HasSuffix(withInstr, "BE NICE") {
		t.Fatal("project instructions section missing")
	}
}

// TestConfiguredAgentsLine covers the startup configured-agents roster line in
// message[1]: it renders the ids when present with the honest "configured subset, not
// the full catalog" framing, sits between the project-id fact and the MCP/Models lines,
// and is omitted entirely when none are configured (an empty roster must never mislead).
func TestConfiguredAgentsLine(t *testing.T) {
	base := MainPromptContext{Tier: domain.TierOperator, MCPConnected: true, SchedulerActive: true}

	// None → no line.
	if out := BuildRuntimeContextMessage(base); strings.Contains(out, "Configured agents:") {
		t.Fatalf("configured-agents line must be omitted when none are configured:\n%s", out)
	}

	// Present → the ids render with the spawn-gate framing + spawn pointer.
	withAgents := base
	withAgents.ConfiguredAgentIDs = []string{"claude", "codex", "antigravity"}
	out := BuildRuntimeContextMessage(withAgents)
	if !strings.Contains(out, "Configured agents: claude, codex, antigravity") {
		t.Fatalf("configured-agents ids missing:\n%s", out)
	}
	// Framing must match the spawn gate (accepts these ids, rejects others) and note the
	// roster is the configured subset — never claim it's the full catalog or that
	// unconfigured agents are spawnable.
	if !strings.Contains(out, "agentTask.spawnForEdits accepts") || !strings.Contains(out, "must be configured first") {
		t.Fatalf("spawn-gate framing missing:\n%s", out)
	}
	// Ordering: after the project-id fact, before the MCP/Models lines.
	pidIdx := strings.Index(out, "Project id:")
	agIdx := strings.Index(out, "Configured agents:")
	mcpIdx := strings.Index(out, "Daintree MCP:")
	if !(pidIdx >= 0 && pidIdx < agIdx && agIdx < mcpIdx) {
		t.Fatalf("configured-agents line out of order (projectId=%d agents=%d mcp=%d):\n%s", pidIdx, agIdx, mcpIdx, out)
	}

	// An id carrying an embedded newline must be flattened to one line — a raw newline
	// would inject a stray heading into message[1].
	nl := base
	nl.ConfiguredAgentIDs = []string{"claude\n# fake heading", "codex"}
	out = BuildRuntimeContextMessage(nl)
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "# fake heading") {
			t.Fatalf("agent id newline not flattened — message[1] line injection:\n%s", out)
		}
	}
	if !strings.Contains(out, "claude # fake heading, codex") {
		t.Fatalf("flattened agent id not rendered on one line:\n%s", out)
	}
}

func TestBuildSkillMessages(t *testing.T) {
	if BuildSkillCatalogMessage(nil) != "" {
		t.Fatal("empty catalog must be empty string")
	}
	cat := BuildSkillCatalogMessage([]SkillMetadata{{ID: "x", Summary: "s", WhenToUse: "w"}})
	if !strings.Contains(cat, "- x — s\n  When to use: w") {
		t.Fatalf("catalog entry malformed: %s", cat)
	}

	empty := BuildLoadedSkillsMessage(RenderedSkillBundle{})
	if !strings.HasPrefix(empty, "# Loaded skills\nNo task-specific skills") {
		t.Fatalf("empty loaded-skills fallback malformed: %s", empty)
	}
	loaded := BuildLoadedSkillsMessage(RenderedSkillBundle{Items: []LoadedSkill{
		{ID: "a", Version: "1", Title: "T", Body: "B"},
	}})
	if !strings.Contains(loaded, "## Skill 1: T\nSkill id: a\nVersion: 1\nB") {
		t.Fatalf("loaded skill body malformed: %s", loaded)
	}
}

func TestBuildSubAgentUserPrompts(t *testing.T) {
	w := BuildWatcherUserPrompt(WatcherUserArgs{Goal: "g"})
	if !strings.Contains(w, "agentState=unknown") || !strings.Contains(w, "Previous classification: none") ||
		!strings.Contains(w, "(no output captured)") {
		t.Fatalf("watcher fallbacks missing: %s", w)
	}
	e := BuildExtractorUserPrompt(ExtractorUserArgs{Instruction: "i", Format: "json", TerminalIDs: []string{"t1", "t2"}})
	if !strings.HasPrefix(e, "Source terminals: t1, t2") {
		t.Fatalf("extractor plural header wrong: %s", e)
	}
	if !strings.Contains(e, "(no schema provided — infer a reasonable JSON value)") {
		t.Fatalf("extractor json schema fallback missing: %s", e)
	}
	e2 := BuildExtractorUserPrompt(ExtractorUserArgs{Instruction: "i", Format: "text", TerminalIDs: nil})
	if !strings.HasPrefix(e2, "Source terminal: unknown") {
		t.Fatalf("extractor singular fallback wrong: %s", e2)
	}
}
