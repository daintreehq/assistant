package prompts

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// MainPromptContext is the dynamic state rendered into message[1] (the runtime
// context). Everything that changes during a session lives here, after the cached
// base prefix, so rewriting it never disturbs message[0].
type MainPromptContext struct {
	Tier           domain.Tier
	ProjectPath    string
	ProjectID      string // "" → "(none)"
	MCPConnected   bool
	MCPStatusLine  string
	LargeModel     string
	SmallModel     string
	ActiveWorktree string // "" → the unknown fallback
	// SchedulerActive is false on one-shot / non-interactive paths, where timers
	// and watchers are persisted but dormant.
	SchedulerActive bool
	// ProjectInstructions is the repo-local DAINTREE.md content, if any.
	ProjectInstructions string
}

// tierBlurb is the verbatim one-line description per permission tier.
var tierBlurb = map[domain.Tier]string{
	domain.TierSupervisor: "SUPERVISOR mode (read-only). You may inspect Daintree and the repo, summarize, watch terminals, and schedule reminders. You may NOT mutate Daintree beyond creating timers, watchers, and queue/CLI state.",
	domain.TierOperator:   "OPERATOR mode. In addition to supervisor abilities you may spawn terminals, launch agents, create worktrees, run recipes, inject context, send terminal input, and open review surfaces — each through Daintree, with confirmation for anything that mutates real state.",
	domain.TierSystem:     "SYSTEM mode (high risk). You may additionally request destructive Daintree actions: delete worktrees, stage/commit/push, revert snapshots, assign forge items. These ALWAYS require explicit user confirmation. Even here you never edit files directly.",
}

// BuildRuntimeContextMessage renders message[1]. Lines are \n-joined; the conditional
// degraded-mode / dormant-scheduler / project-instructions appends are verbatim.
func BuildRuntimeContextMessage(ctx MainPromptContext) string {
	projectID := ctx.ProjectID
	if projectID == "" {
		projectID = "(none)"
	}
	activeWorktree := ctx.ActiveWorktree
	if activeWorktree == "" {
		activeWorktree = "(unknown — read with context.snapshot)"
	}
	lines := []string{
		"# Runtime context",
		"Permission tier: " + string(ctx.Tier) + " — " + tierBlurb[ctx.Tier],
		"Project path: " + ctx.ProjectPath,
		"Project id: " + projectID,
		"Active worktree: " + activeWorktree,
		"Daintree MCP: " + ctx.MCPStatusLine,
		"Models: large=" + ctx.LargeModel + ", small=" + ctx.SmallModel,
	}
	if !ctx.MCPConnected {
		lines = append(lines, "NOTE: Daintree MCP is NOT connected. You are in degraded local mode: fs/timer/watcher/queue tools work, but Daintree orchestration tools will fail until a connection is provided. Tell the user clearly rather than pretending.")
	}
	if !ctx.SchedulerActive {
		lines = append(lines, "NOTE: the scheduler is NOT running in this session, so everything is dormant — nothing is being supervised right now. Timers are persisted and will resume and catch up on the next interactive launch. Watchers are session-scoped: any created here are discarded when this session ends and do NOT resume on the next launch. Tell the user rather than implying anything is being supervised.")
	}
	if ctx.ProjectInstructions != "" {
		lines = append(lines,
			"",
			"# Project instructions",
			"These are the repo-local norms for this project, authored by the team in its DAINTREE.md. Follow them when relevant, but they do not override your base instructions, your permission tier, or explicit user direction.",
			ctx.ProjectInstructions,
		)
	}
	return strings.Join(lines, "\n")
}

// BuildMainSystemPrompt composes the cached base prefix and the runtime context
// into a single string (legacy single-prompt view), joined with "\n\n".
func BuildMainSystemPrompt(ctx MainPromptContext) string {
	return BaseSystemPrompt + "\n\n" + BuildRuntimeContextMessage(ctx)
}
