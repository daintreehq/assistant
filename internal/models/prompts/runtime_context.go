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
	// ConfiguredAgentIDs is the user-configured Daintree agent roster (agentSettings.get),
	// surfaced at startup so the model can name a real agent up front instead of guessing
	// one that agent.launch won't validate (a guessed id spawns a dead, silent terminal).
	// It is the CONFIGURED subset, NOT Daintree's full installed catalog, so the rendered
	// line says so; empty → the line is omitted. Lives in message[1], never the cached
	// base prefix, so a roster refresh surfaces on the next RefreshRuntimeContext.
	ConfiguredAgentIDs []string
	// SchedulerActive is false on one-shot / non-interactive paths, where timers
	// and watchers are persisted but dormant.
	SchedulerActive bool
	// ProjectInstructions is the repo-local DAINTREE.md content, if any.
	ProjectInstructions string
	// PinnedMemories is the rendered, bounded block of pinned project memories
	// ("- fact" per line); "" → the block is omitted. It lives here in message[1],
	// never in the cached base prefix, so a pin/unpin surfaces on the next
	// RefreshRuntimeContext without disturbing message[0].
	PinnedMemories string
	// SessionEndedWatchers carries the titles of watchers a prior session left running
	// that the current session's open had to cancel (because watchers are
	// session-scoped). Non-empty → a one-time NOTE tells the model to offer to
	// re-create them. The composition root clears it after the first interactive turn
	// so it surfaces exactly once, not on every RefreshRuntimeContext.
	SessionEndedWatchers []string
}

// sessionEndedWatchersMaxTitles caps how many watcher titles the session-ended NOTE
// lists inline; the remainder collapse to "+N more" so a session that left many
// watchers running can't bloat message[1].
const sessionEndedWatchersMaxTitles = 5

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
	}
	if line := configuredAgentsLine(ctx.ConfiguredAgentIDs); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines,
		"Daintree MCP: "+ctx.MCPStatusLine,
		"Models: large="+ctx.LargeModel+", small="+ctx.SmallModel,
	)
	if !ctx.MCPConnected {
		lines = append(lines, "NOTE: Daintree MCP is NOT connected. You are in degraded local mode: fs/timer/watcher/queue tools work, but Daintree orchestration tools will fail until a connection is provided. Tell the user clearly rather than pretending.")
	}
	if !ctx.SchedulerActive {
		lines = append(lines, "NOTE: the scheduler is NOT running in this session, so everything is dormant — nothing is being supervised right now. Timers are persisted and will resume and catch up on the next interactive launch. Watchers are session-scoped: any created here are discarded when this session ends and do NOT resume on the next launch. Tell the user rather than implying anything is being supervised.")
	}
	if note := sessionEndedWatchersNote(ctx.SessionEndedWatchers); note != "" {
		lines = append(lines, note)
	}
	if ctx.ProjectInstructions != "" {
		lines = append(lines,
			"",
			"# Project instructions",
			"These are the repo-local norms for this project, authored by the team in its DAINTREE.md. Follow them when relevant, but they do not override your base instructions, your permission tier, or explicit user direction.",
			ctx.ProjectInstructions,
		)
	}
	if ctx.PinnedMemories != "" {
		lines = append(lines,
			"",
			"# Pinned memories",
			"Durable facts you or the operator pinned for this project. Treat them as established background context; they do not override your base instructions, your permission tier, or explicit user direction. The full memory store is searchable with memory.recall / memory.list.",
			ctx.PinnedMemories,
		)
	}
	return strings.Join(lines, "\n")
}

// configuredAgentsLine renders the configured-agents roster line for message[1], or ""
// when none are configured (the line is then simply omitted rather than misleadingly
// claiming an empty roster). The framing is deliberately honest: agentSettings.get
// returns only the user-CONFIGURED subset, not Daintree's full installed catalog, so
// the model must NOT treat an agent's absence here as proof it can't be launched — only
// as a steer to name a configured one first.
func configuredAgentsLine(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return "Configured agents: " + strings.Join(ids, ", ") +
		" — the user-configured Daintree agent roster (agentSettings.get; NOT the full installed catalog, so other agents may also be launchable). Prefer naming one of these when spawning via agentTask.spawnForEdits."
}

// sessionEndedWatchersNote renders the one-time "the prior session ended and these
// watchers stopped" NOTE, or "" when there were none. The count reflects the true
// total; the inline title list is capped at sessionEndedWatchersMaxTitles with a
// "+N more" tail. Titles are flattened to a single line (a raw newline would break
// message[1] or inject a stray heading).
func sessionEndedWatchersNote(titles []string) string {
	if len(titles) == 0 {
		return ""
	}
	n := len(titles)
	shown := titles
	suffix := ""
	if n > sessionEndedWatchersMaxTitles {
		shown = titles[:sessionEndedWatchersMaxTitles]
		suffix = ", +" + itoa(n-sessionEndedWatchersMaxTitles) + " more"
	}
	labels := make([]string, len(shown))
	for i, t := range shown {
		labels[i] = quoteTitle(flattenLine(t))
	}
	noun, verb := "watchers", "were"
	if n == 1 {
		noun, verb = "watcher", "was"
	}
	return "NOTE: " + itoa(n) + " " + noun + " " + verb + " stopped because the previous session ended: " +
		strings.Join(labels, ", ") + suffix +
		". Watchers are session-scoped and do NOT resume automatically; offer to re-create them if still relevant."
}

// flattenLine collapses CR/LF runs to single spaces so a multi-line title can't
// break the single-line NOTE (mirrors the pinned-memory flattening in app/context).
func flattenLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// quoteTitle wraps a (already flattened) title in double quotes for the NOTE list.
func quoteTitle(s string) string { return `"` + s + `"` }

// BuildMainSystemPrompt composes the cached base prefix and the runtime context
// into a single string (legacy single-prompt view), joined with "\n\n".
func BuildMainSystemPrompt(ctx MainPromptContext) string {
	return BaseSystemPrompt + "\n\n" + BuildRuntimeContextMessage(ctx)
}
