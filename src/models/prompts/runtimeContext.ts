/**
 * The runtime context system message — message[1].
 *
 * Everything that changes during a session lives here, after the cached base
 * prefix: the permission tier, project path/id, active worktree, MCP status, and
 * model ids. Rewriting this message does not disturb message[0], so the prompt
 * cache prefix survives tier changes and MCP (re)connects.
 */
import type { Tier } from "../../schemas.js";

export interface MainPromptContext {
  tier: Tier;
  projectPath: string;
  projectId?: string;
  mcpConnected: boolean;
  mcpStatusLine: string;
  largeModel: string;
  smallModel: string;
  activeWorktree?: string;
  /**
   * Whether the foreground scheduler is running in this session. False on
   * one-shot / non-interactive paths, where timers and watchers are persisted
   * but dormant until the next interactive launch.
   */
  schedulerActive: boolean;
}

const TIER_BLURB: Record<Tier, string> = {
  supervisor:
    "SUPERVISOR mode (read-only). You may inspect Daintree and the repo, summarize, watch terminals, and schedule reminders. You may NOT mutate Daintree beyond creating timers, watchers, and queue/CLI state.",
  operator:
    "OPERATOR mode. In addition to supervisor abilities you may spawn terminals, launch agents, create worktrees, run recipes, inject context, send terminal input, and open review surfaces — each through Daintree, with confirmation for anything that mutates real state.",
  system:
    "SYSTEM mode (high risk). You may additionally request destructive Daintree actions: delete worktrees, stage/commit/push, revert snapshots, assign forge items. These ALWAYS require explicit user confirmation. Even here you never edit files directly.",
};

export function buildRuntimeContextMessage(ctx: MainPromptContext): string {
  const lines = [
    "# Runtime context",
    `Permission tier: ${ctx.tier} — ${TIER_BLURB[ctx.tier]}`,
    `Project path: ${ctx.projectPath}`,
    `Project id: ${ctx.projectId ?? "(none)"}`,
    `Active worktree: ${ctx.activeWorktree ?? "(unknown — read with context.snapshot)"}`,
    `Daintree MCP: ${ctx.mcpStatusLine}`,
    `Models: large=${ctx.largeModel}, small=${ctx.smallModel}`,
  ];
  if (!ctx.mcpConnected) {
    lines.push(
      "NOTE: Daintree MCP is NOT connected. You are in degraded local mode: fs/timer/watcher/queue tools work, but Daintree orchestration tools will fail until a connection is provided. Tell the user clearly rather than pretending.",
    );
  }
  if (!ctx.schedulerActive) {
    lines.push(
      "NOTE: the scheduler is NOT running in this session, so everything is dormant — nothing is being supervised right now. Timers are persisted and will resume and catch up on the next interactive launch. Watchers are session-scoped: any created here are discarded when this session ends and do NOT resume on the next launch. Tell the user rather than implying anything is being supervised.",
    );
  }
  return lines.join("\n");
}
