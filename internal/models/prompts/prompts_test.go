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
	// 59 verified Daintree MCP tool names (agentSettings.get was added so the spawn
	// wrapper can validate agentId against the configured roster; terminal.close backs
	// the terminal.close wrapper that retires spawned cohorts). Match verbatim.
	if len(DocumentedMCPToolNames) != 59 {
		t.Fatalf("got %d tool names, want 59", len(DocumentedMCPToolNames))
	}
	if DocumentedMCPToolNames[0] != "actions.getContext" || DocumentedMCPToolNames[58] != "worktree.list" {
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

// TestSessionEndedWatchersNote covers the one-time "the prior session ended and these
// watchers stopped" NOTE: it appears only when titles are present, pluralizes/counts
// correctly, lists titles, caps the inline list with "+N more", and flattens embedded
// newlines so it stays a single line.
func TestSessionEndedWatchersNote(t *testing.T) {
	base := MainPromptContext{
		Tier: domain.TierOperator, MCPConnected: true, SchedulerActive: true,
	}

	// None → no NOTE.
	if out := BuildRuntimeContextMessage(base); strings.Contains(out, "previous session ended") {
		t.Fatal("unexpected session-ended NOTE when no watchers carried over")
	}

	// One → singular noun + verb, count 1, the title quoted.
	one := base
	one.SessionEndedWatchers = []string{"deploy log"}
	out := BuildRuntimeContextMessage(one)
	if !strings.Contains(out, "NOTE: 1 watcher was stopped because the previous session ended: \"deploy log\".") {
		t.Fatalf("singular session-ended NOTE missing/wrong:\n%s", out)
	}
	if !strings.Contains(out, "do NOT resume automatically; offer to re-create them") {
		t.Fatalf("re-create guidance missing:\n%s", out)
	}

	// Many → plural noun, capped inline list + "+N more" (7 titles, cap 5 → 2 more).
	many := base
	many.SessionEndedWatchers = []string{"w1", "w2", "w3", "w4", "w5", "w6", "w7"}
	out = BuildRuntimeContextMessage(many)
	if !strings.Contains(out, "NOTE: 7 watchers were stopped") {
		t.Fatalf("plural count wrong:\n%s", out)
	}
	if !strings.Contains(out, "\"w5\", +2 more.") {
		t.Fatalf("title cap/+N more wrong:\n%s", out)
	}
	if strings.Contains(out, "\"w6\"") || strings.Contains(out, "\"w7\"") {
		t.Fatalf("titles past the cap must be collapsed into +N more:\n%s", out)
	}

	// Embedded newline in a title must be flattened (else it breaks message[1]).
	nl := base
	nl.SessionEndedWatchers = []string{"line1\nline2"}
	out = BuildRuntimeContextMessage(nl)
	noteLine := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "NOTE: 1 watcher was stopped") {
			noteLine = ln
		}
	}
	if noteLine == "" {
		t.Fatalf("flattened single-line NOTE not found:\n%s", out)
	}
	if !strings.Contains(noteLine, "\"line1 line2\"") {
		t.Fatalf("title newline not flattened to a space:\n%s", noteLine)
	}
}

// TestConfiguredAgentsLine covers the startup configured-agents roster line in
// message[1]: it renders the ids when present with the honest "configured subset, not
// the full catalog" framing, sits between the worktree fact and the MCP/Models lines,
// and is omitted entirely when none are configured (an empty roster must never mislead).
func TestConfiguredAgentsLine(t *testing.T) {
	base := MainPromptContext{Tier: domain.TierOperator, MCPConnected: true, SchedulerActive: true}

	// None → no line.
	if out := BuildRuntimeContextMessage(base); strings.Contains(out, "Configured agents:") {
		t.Fatalf("configured-agents line must be omitted when none are configured:\n%s", out)
	}

	// Present → the ids render with the honest framing + spawn pointer.
	withAgents := base
	withAgents.ConfiguredAgentIDs = []string{"claude", "codex", "antigravity"}
	out := BuildRuntimeContextMessage(withAgents)
	if !strings.Contains(out, "Configured agents: claude, codex, antigravity") {
		t.Fatalf("configured-agents ids missing:\n%s", out)
	}
	if !strings.Contains(out, "NOT the full installed catalog") {
		t.Fatalf("honest configured-subset framing missing:\n%s", out)
	}
	if !strings.Contains(out, "agentTask.spawnForEdits") {
		t.Fatalf("spawn pointer missing:\n%s", out)
	}
	// Ordering: after the worktree fact, before the MCP/Models lines.
	wtIdx := strings.Index(out, "Active worktree:")
	agIdx := strings.Index(out, "Configured agents:")
	mcpIdx := strings.Index(out, "Daintree MCP:")
	if !(wtIdx >= 0 && wtIdx < agIdx && agIdx < mcpIdx) {
		t.Fatalf("configured-agents line out of order (worktree=%d agents=%d mcp=%d):\n%s", wtIdx, agIdx, mcpIdx, out)
	}
}

// TestActiveWorktreeLine covers the three worktree states message[1] distinguishes now
// that the label is fetched at connect: a known label (the branch) renders verbatim; the
// explicit "no worktree" sentinel renders as-is; and an empty value (read not attempted
// or failed) falls back to the "(unknown — read with context.snapshot)" placeholder.
func TestActiveWorktreeLine(t *testing.T) {
	base := MainPromptContext{Tier: domain.TierOperator, MCPConnected: true, SchedulerActive: true}

	known := base
	known.ActiveWorktree = "feature/issue-230"
	if out := BuildRuntimeContextMessage(known); !strings.Contains(out, "Active worktree: feature/issue-230") {
		t.Fatalf("known worktree branch not rendered:\n%s", out)
	}

	none := base
	none.ActiveWorktree = "(none — not in a worktree)"
	if out := BuildRuntimeContextMessage(none); !strings.Contains(out, "Active worktree: (none — not in a worktree)") {
		t.Fatalf("explicit no-worktree sentinel not rendered:\n%s", out)
	}

	// Empty → the unknown placeholder (degraded / not fetched).
	if out := BuildRuntimeContextMessage(base); !strings.Contains(out, "Active worktree: (unknown — read with context.snapshot)") {
		t.Fatalf("empty worktree must fall back to unknown placeholder:\n%s", out)
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
