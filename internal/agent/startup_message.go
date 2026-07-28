package agent

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/prompts"
)

const (
	startupAgentCatalogByteBudget = 16 * 1024
	startupAgentCatalogMaxRows    = 512
	startupInstructionsByteBudget = 16 * 1024
	startupProjectIDMaxRunes      = 256
	startupProjectNameMaxRunes    = 512
	startupProjectPathMaxRunes    = 4096
	startupProjectStatusMaxRunes  = 64
	startupAgentDisplayMaxRunes   = 512
	startupWorktreeIDMaxRunes     = 4096
	startupWorktreePathMaxRunes   = 4096
	startupWorktreeBranchMaxRunes = 512
	startupWorktreeTitleMaxRunes  = 512
	startupWorktreeURLMaxRunes    = 4096
	startupWorktreeStatusMaxRunes = 64
	startupWorktreeCommitMaxRunes = 1024
)

// buildStartupContext deterministically projects the stable splash-time snapshot onto
// the backend's dedicated, cache-friendly startup channel. Unlike the former framed
// user-role message, this value never enters visible history, persistence, or skill
// selection as user-authored text. Agent order and identifiers are retained exactly;
// all other externally supplied strings are normalized and capped before strict backend
// validation.
func buildStartupContext(pc prompts.MainPromptContext) backend.StartupContext {
	project := pc.Project
	if project == nil && (pc.ProjectID != "" || pc.ProjectPath != "") {
		project = &prompts.ProjectContext{ID: pc.ProjectID, Path: pc.ProjectPath}
	}

	startup := backend.StartupContext{
		ProjectInstructions: startupInstructions(pc.ProjectInstructions),
	}
	if project != nil {
		startup.Project = &backend.ProjectSnapshot{
			ID:                    startupLine(project.ID, startupProjectIDMaxRunes),
			Name:                  startupLine(project.Name, startupProjectNameMaxRunes),
			Path:                  startupLine(project.Path, startupProjectPathMaxRunes),
			Status:                startupLine(project.Status, startupProjectStatusMaxRunes),
			DaintreeConfigPresent: copyOptionalBool(project.DaintreeConfigPresent),
			InRepoSettings:        copyOptionalBool(project.InRepoSettings),
		}
	}
	if pc.AgentRoster != nil {
		startup.AgentRoster = buildAgentRosterSnapshot(pc.AgentRoster)
	}
	return startup
}

func buildAgentRosterSnapshot(roster *prompts.AgentRosterContext) *backend.AgentRosterSnapshot {
	agents := make([]backend.AgentSnapshot, 0, len(roster.Agents))
	// Include array delimiters and separators in the byte accounting. Rows that do not
	// fit are omitted whole: an exact registered id is either present byte-for-byte or
	// absent, never truncated into an identifier Daintree cannot launch.
	usedBytes := len("[]")
	for _, agent := range roster.Agents {
		if len(agents) == startupAgentCatalogMaxRows {
			break
		}
		if strings.TrimSpace(agent.ID) == "" {
			continue
		}
		source, ok := startupAgentSource(agent.Source)
		if !ok {
			continue
		}
		row := backend.AgentSnapshot{
			ID:             agent.ID,
			DisplayName:    startupLine(agent.DisplayName, startupAgentDisplayMaxRunes),
			Source:         source,
			Availability:   startupAgentAvailability(agent.Availability),
			Installed:      copyOptionalBool(agent.Installed),
			Launchable:     copyOptionalBool(agent.Launchable),
			Pinned:         copyOptionalBool(agent.Pinned),
			ToolbarVisible: copyOptionalBool(agent.ToolbarVisible),
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			continue
		}
		cost := len(encoded)
		if len(agents) > 0 {
			cost++ // comma between JSON rows
		}
		if usedBytes+cost > startupAgentCatalogByteBudget {
			continue
		}
		agents = append(agents, row)
		usedBytes += cost
	}

	total := roster.TotalCount
	if total < len(roster.Agents) {
		total = len(roster.Agents)
	}
	return &backend.AgentRosterSnapshot{
		Agents:               agents,
		Complete:             roster.Complete,
		AvailabilityComplete: roster.AvailabilityComplete,
		TotalCount:           total,
	}
}

func startupAgentSource(value string) (string, bool) {
	switch strings.TrimSpace(value) {
	case "built-in":
		return "built-in", true
	case "user":
		return "user", true
	case "plugin":
		return "plugin", true
	default:
		return "", false
	}
}

func startupAgentAvailability(value string) string {
	switch strings.TrimSpace(value) {
	case "missing":
		return "missing"
	case "installed":
		return "installed"
	case "ready":
		return "ready"
	case "blocked":
		return "blocked"
	case "unauthenticated":
		return "unauthenticated"
	default:
		return ""
	}
}

// buildCurrentWorktreeSnapshot preserves the live read's three states. nil is an
// unavailable read, Present=false becomes {current:null}, and Present=true carries the
// complete useful worktree row.
func buildCurrentWorktreeSnapshot(worktree *prompts.WorktreeContext) *backend.CurrentWorktreeSnapshot {
	if worktree == nil {
		return nil
	}
	result := &backend.CurrentWorktreeSnapshot{}
	if !worktree.Present {
		return result
	}
	result.Current = &backend.WorktreeSnapshot{
		ID:          startupLine(worktree.ID, startupWorktreeIDMaxRunes),
		Path:        startupLine(worktree.Path, startupWorktreePathMaxRunes),
		Branch:      startupLine(worktree.Branch, startupWorktreeBranchMaxRunes),
		IsMain:      worktree.IsMain,
		IssueNumber: copyOptionalInt(worktree.IssueNumber),
		IssueTitle:  startupLine(worktree.IssueTitle, startupWorktreeTitleMaxRunes),
		PRNumber:    copyOptionalInt(worktree.PRNumber),
		PRTitle:     startupLine(worktree.PRTitle, startupWorktreeTitleMaxRunes),
		PRURL:       startupLine(worktree.PRURL, startupWorktreeURLMaxRunes),
		Status:      startupLine(worktree.Status, startupWorktreeStatusMaxRunes),
		LastCommit:  startupLine(worktree.LastCommit, startupWorktreeCommitMaxRunes),
	}
	return result
}

func startupLine(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	return clampRunes(value, maxRunes)
}

func startupInstructions(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' {
			return r
		}
		return -1
	}, value)
	return strings.TrimSpace(clampUTF8Bytes(value, startupInstructionsByteBudget))
}

func clampUTF8Bytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func copyOptionalBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyOptionalInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
