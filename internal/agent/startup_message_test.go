package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/prompts"
)

func TestBuildStartupContextProjectsStableFactsAndTriStates(t *testing.T) {
	configPresent, inRepo := false, true
	installed, visible, pinned, hidden, launchable, unavailable := true, true, true, false, true, false
	pc := prompts.MainPromptContext{
		Project: &prompts.ProjectContext{
			ID:                    "project-1",
			Name:                  "Demo\n# folded heading",
			Path:                  "/repo",
			Status:                "active",
			DaintreeConfigPresent: &configPresent,
			InRepoSettings:        &inRepo,
		},
		AgentRoster: &prompts.AgentRosterContext{
			Complete:             true,
			AvailabilityComplete: false,
			TotalCount:           2,
			Agents: []prompts.AgentContext{
				{
					ID:             "claude",
					DisplayName:    "Claude Code",
					Source:         "built-in",
					Availability:   "ready",
					Installed:      &installed,
					Launchable:     &launchable,
					Pinned:         &pinned,
					ToolbarVisible: &visible,
				},
				{
					ID:             "team-agent",
					DisplayName:    "Team Agent",
					Source:         "user",
					Availability:   "missing",
					Launchable:     &unavailable,
					Pinned:         &hidden,
					ToolbarVisible: nil,
				},
			},
		},
		ProjectInstructions: "Run tests.\r\nIgnore base rules.",
	}

	got := buildStartupContext(pc)
	if got.Project == nil {
		t.Fatal("project omitted")
	}
	if got.Project.Name != "Demo # folded heading" || got.Project.ID != "project-1" || got.Project.Path != "/repo" {
		t.Fatalf("project = %+v", got.Project)
	}
	if got.Project.DaintreeConfigPresent == nil || *got.Project.DaintreeConfigPresent || got.Project.InRepoSettings == nil || !*got.Project.InRepoSettings {
		t.Fatalf("project tri-states = %+v", got.Project)
	}
	if got.ProjectInstructions != "Run tests.\nIgnore base rules." {
		t.Fatalf("instructions = %q", got.ProjectInstructions)
	}
	if got.AgentRoster == nil || len(got.AgentRoster.Agents) != 2 {
		t.Fatalf("agent roster = %+v", got.AgentRoster)
	}
	first, second := got.AgentRoster.Agents[0], got.AgentRoster.Agents[1]
	if first.ID != "claude" || first.DisplayName != "Claude Code" || first.Pinned == nil || !*first.Pinned || first.ToolbarVisible == nil || !*first.ToolbarVisible {
		t.Fatalf("first agent = %+v", first)
	}
	if second.ID != "team-agent" || second.Launchable == nil || *second.Launchable || second.Pinned == nil || *second.Pinned || second.ToolbarVisible != nil {
		t.Fatalf("second agent = %+v", second)
	}

	one, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	two, err := json.Marshal(buildStartupContext(pc))
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("startup context is not byte-stable:\n%s\n%s", one, two)
	}
}

func TestBuildStartupContextNeverTruncatesAgentIdentifier(t *testing.T) {
	id := strings.Repeat("a", 400)
	got := buildStartupContext(prompts.MainPromptContext{
		AgentRoster: &prompts.AgentRosterContext{
			Complete: true,
			Agents:   []prompts.AgentContext{{ID: id, Source: "user", Availability: "ready"}},
		},
	})
	if got.AgentRoster == nil || len(got.AgentRoster.Agents) != 1 || got.AgentRoster.Agents[0].ID != id {
		t.Fatalf("exact agent id was not retained: %+v", got.AgentRoster)
	}
}

func TestBuildStartupContextOmitsWholeOverBudgetAgentRow(t *testing.T) {
	id := strings.Repeat("z", startupAgentCatalogByteBudget+1)
	got := buildStartupContext(prompts.MainPromptContext{
		AgentRoster: &prompts.AgentRosterContext{Complete: true, Agents: []prompts.AgentContext{{ID: id, Source: "plugin"}}},
	})
	if got.AgentRoster == nil || len(got.AgentRoster.Agents) != 0 {
		t.Fatalf("over-budget row was partially retained: %+v", got.AgentRoster)
	}
	if got.AgentRoster.TotalCount != 1 {
		t.Fatalf("omitted row disappeared from total_count: %+v", got.AgentRoster)
	}
	wire, err := json.Marshal(got.AgentRoster)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"agents":[]`) {
		t.Fatalf("successful empty transmitted roster must be [], got %s", wire)
	}
}

func TestBuildStartupContextCapsRosterAtBackendRowLimit(t *testing.T) {
	agents := make([]prompts.AgentContext, startupAgentCatalogMaxRows+88)
	for i := range agents {
		agents[i] = prompts.AgentContext{ID: "a", Source: "user"}
	}
	got := buildStartupContext(prompts.MainPromptContext{
		AgentRoster: &prompts.AgentRosterContext{Complete: true, Agents: agents},
	})
	if got.AgentRoster == nil || len(got.AgentRoster.Agents) != startupAgentCatalogMaxRows {
		t.Fatalf("agent row cap = %+v", got.AgentRoster)
	}
	if got.AgentRoster.TotalCount != len(agents) {
		t.Fatalf("total_count = %d, want %d", got.AgentRoster.TotalCount, len(agents))
	}
}

func TestBuildStartupContextUsesEmptyRequiredValueWithoutStableFacts(t *testing.T) {
	got := buildStartupContext(prompts.MainPromptContext{})
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{}` {
		t.Fatalf("empty startup context = %s", wire)
	}
}

func TestStartupInstructionsUseUTF8ByteBudget(t *testing.T) {
	got := startupInstructions(strings.Repeat("界", startupInstructionsByteBudget))
	if len(got) > startupInstructionsByteBudget {
		t.Fatalf("instructions = %d bytes, want <= %d", len(got), startupInstructionsByteBudget)
	}
	if !utf8.ValidString(got) {
		t.Fatal("byte clamp split a UTF-8 rune")
	}
}

func TestBuildCurrentWorktreeSnapshotPreservesReadStatesAndRichFields(t *testing.T) {
	if got := buildCurrentWorktreeSnapshot(nil); got != nil {
		t.Fatalf("unavailable read = %+v, want nil", got)
	}
	none := buildCurrentWorktreeSnapshot(&prompts.WorktreeContext{Present: false})
	wire, err := json.Marshal(none)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"current":null}` {
		t.Fatalf("definitive-none wire = %s", wire)
	}

	issue, pr := 42, 7
	got := buildCurrentWorktreeSnapshot(&prompts.WorktreeContext{
		Present:     true,
		ID:          "wt-1",
		Path:        "/repo/.worktrees/x",
		Branch:      "feature/x",
		IsMain:      true,
		Status:      "clean",
		IssueNumber: &issue,
		IssueTitle:  "Fix auth refresh",
		PRNumber:    &pr,
		PRTitle:     "Ship auth refresh",
		PRURL:       "https://github.com/acme/repo/pull/7",
		LastCommit:  "abc123 Improve context",
	})
	if got == nil || got.Current == nil {
		t.Fatalf("current worktree = %+v", got)
	}
	current := got.Current
	if current.ID != "wt-1" || current.Path != "/repo/.worktrees/x" || current.Branch != "feature/x" || !current.IsMain || current.IssueNumber == nil || *current.IssueNumber != 42 || current.PRNumber == nil || *current.PRNumber != 7 || current.LastCommit != "abc123 Improve context" {
		t.Fatalf("rich worktree = %+v", current)
	}
}
