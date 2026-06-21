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
	// The TS array has 57 entries (the spec prose's "60" is loose; the source
	// array — DOCUMENTED_MCP_TOOL_NAMES — is 57). Match the source verbatim.
	if len(DocumentedMCPToolNames) != 57 {
		t.Fatalf("got %d tool names, want 57", len(DocumentedMCPToolNames))
	}
	if DocumentedMCPToolNames[0] != "actions.getContext" || DocumentedMCPToolNames[56] != "worktree.list" {
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
	if !strings.Contains(out, "Active worktree: (unknown — read with context.snapshot)") {
		t.Fatal("worktree fallback missing")
	}
	if !strings.Contains(out, "Models: large=L, small=S") {
		t.Fatal("models line missing")
	}
	// connected + scheduler active → no degraded/dormant notes.
	if strings.Contains(out, "NOTE:") {
		t.Fatal("unexpected NOTE when fully connected/active")
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
