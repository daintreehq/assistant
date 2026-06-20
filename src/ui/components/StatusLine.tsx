/**
 * A compact status line that prefers CURRENT STATE over inventory counts: what
 * Daintree is doing right now (the active agent, or "Standing by"), then the
 * smallest useful rollup — context pressure, session cost/model while idle,
 * attention count, agent count, permission tier, and the live MCP badge. The
 * attention chip and a high context gauge are the only saturated tokens; everything
 * else stays dim.
 *
 *   ◌ WORKING term_8 · tests running 18s · CTX 42% · agents 2 · sys · MCP
 *   Standing by · SYSTEM · CTX 8% · $0.004 · minimax-m3 · MCP
 */
import { Box, Text } from "ink";
import type { DashboardState, SessionUsage } from "../types.js";
import { StateBadge, formatDuration } from "../primitives.js";
import { severityTone, toneColor, tierShort, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";
import { LIVE_CHROME_MAX_WIDTH } from "../liveChrome.js";

/** Format a running session cost compactly, keeping small amounts legible. */
function formatCost(cost: number): string {
  if (cost <= 0) return "$0.000";
  if (cost < 0.01) return `$${cost.toFixed(4)}`;
  if (cost < 1) return `$${cost.toFixed(3)}`;
  return `$${cost.toFixed(2)}`;
}

export function StatusLine({
  dashboard,
  tier,
  sessionUsage,
  width = 80,
  now = Date.now(),
}: {
  dashboard: DashboardState;
  tier?: string;
  sessionUsage?: SessionUsage;
  width?: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers);
  const active =
    agents.find((a) => a.classification === "still_working") ?? agents[0];
  const attention = dashboard.inbox.length;
  const connected = dashboard.mcp.connected;
  const topSev = dashboard.inbox[0]?.severity ?? "attention";

  // Context pressure: latest estimate over the auto-compact threshold. Only shown
  // once a usage event has arrived (threshold > 0). Dim by default; amber as it
  // nears the threshold, red once compaction is imminent, so a glance reads load.
  const pressure =
    sessionUsage && sessionUsage.contextThreshold > 0
      ? Math.round(
          (sessionUsage.contextTokens / sessionUsage.contextThreshold) * 100,
        )
      : undefined;
  const pressureColor =
    pressure === undefined
      ? undefined
      : pressure >= 90
        ? ui.color.danger
        : pressure >= 75
          ? ui.color.warning
          : undefined;
  const cost = sessionUsage?.costUsd;
  const model = sessionUsage?.lastModel;
  const chromeWidth = Math.min(width, LIVE_CHROME_MAX_WIDTH);
  const activeDuration = active
    ? formatDuration(Math.max(0, now - active.startedAt))
    : "";
  // The model id is the longest right-hand token, so only show it when the line is
  // wide enough to carry it without crowding the active-agent text on the left.
  const showModel = width >= 62 && !!model;
  const showCostModel = !active;
  // Keep the permission tier visible at all times — including during active runs,
  // when the left side carries agent context instead of the idle "Standing by"
  // label. The `system` tier (git/system powers unlocked) gets a saturated danger
  // color so the elevated risk reads at a glance; lower tiers stay dim.
  const tierText = tier ? tierShort(tier) : "";
  const rightTierText = active ? tierText : "";

  const ctxText = pressure !== undefined ? `CTX ${pressure}%` : "";
  const costText = showCostModel && cost !== undefined ? formatCost(cost) : "";

  // Reserve room for the compact rollup so the line never wraps to 2 rows
  // (which would overflow the fixed-height shell and overlap the row above). Each
  // visible segment costs its text plus a 3-char " · " separator.
  const seg = (s: string) => (s ? s.length + 3 : 0);
  const idlePrefixLen = active
    ? 0
    : "Standing by".length + (tier ? 3 + tier.toUpperCase().length : 0);
  const attentionLen = attention > 0 ? 4 + String(attention).length : 0;
  const agentsLen =
    agents.length > 0 ? 3 + "agents ".length + String(agents.length).length : 0;
  const mcpLen = 3 + (connected ? "MCP".length : "DEGRADED".length);
  const modelText =
    showCostModel &&
    showModel &&
    model &&
    idlePrefixLen +
      seg(ctxText) +
      seg(costText) +
      seg(model) +
      attentionLen +
      agentsLen +
      seg(rightTierText) +
      mcpLen <=
      chromeWidth
      ? model
      : "";
  const activeBadgeLen = active
    ? 2 + active.badge.label.toUpperCase().length
    : 0;
  const activeFixedLen = active
    ? activeBadgeLen +
      1 +
      active.id.length +
      (activeDuration ? 1 + activeDuration.length : 0)
    : 0;
  const requiredRollupLen =
    seg(ctxText) +
    seg(costText) +
    seg(modelText) +
    attentionLen +
    seg(rightTierText) +
    mcpLen;
  const showAgents =
    agents.length > 0 &&
    (!active || activeFixedLen + requiredRollupLen + agentsLen <= chromeWidth);
  const visibleAgentsLen = showAgents ? agentsLen : 0;
  const rightLen =
    seg(ctxText) +
    seg(costText) +
    seg(modelText) +
    seg(rightTierText) +
    attentionLen +
    visibleAgentsLen +
    mcpLen;
  const activeGoalRoom = active
    ? chromeWidth - rightLen - activeFixedLen - 3
    : 0;
  const activeGoal =
    active && activeGoalRoom >= 6
      ? truncate(active.goal || active.title, activeGoalRoom)
      : "";

  return (
    // Keep the repainting status row short. A full-width `space-between` row is
    // visually tidy, but on terminal shrink the OLD wide row reflows before Ink's
    // erase pass and the top physical row can survive as a duplicate status line.
    <Box width="100%" maxWidth={LIVE_CHROME_MAX_WIDTH}>
      <Text wrap="truncate">
        {active ? (
          <>
            <StateBadge tone={active.badge.tone} label={active.badge.label} />
            <Text dimColor>
              {" "}
              {active.id}
              {activeGoal ? ` · ${activeGoal}` : ""}
              {activeDuration ? ` ${activeDuration}` : ""}
            </Text>
          </>
        ) : (
          <Text dimColor>
            Standing by{tier ? ` · ${tier.toUpperCase()}` : ""}
          </Text>
        )}
        {ctxText ? (
          <Text dimColor> · </Text>
        ) : null}
        {ctxText ? (
          <Text color={pressureColor} dimColor={pressureColor === undefined}>
            {ctxText}
          </Text>
        ) : null}
        {costText ? (
          <>
            <Text dimColor> · </Text>
            <Text dimColor>{costText}</Text>
          </>
        ) : null}
        {modelText ? (
          <>
            <Text dimColor> · </Text>
            <Text dimColor>{modelText}</Text>
          </>
        ) : null}
        {attention > 0 ? (
          <>
            <Text dimColor> · </Text>
            <Text color={toneColor(severityTone(topSev))}>!{attention}</Text>
          </>
        ) : null}
        {showAgents ? (
          <>
            <Text dimColor> · agents {agents.length}</Text>
          </>
        ) : null}
        {rightTierText ? (
          <>
            <Text dimColor> · </Text>
            <Text
              color={tier === "system" ? ui.color.danger : undefined}
              dimColor={tier !== "system"}
            >
              {rightTierText}
            </Text>
          </>
        ) : null}
        <Text dimColor> · </Text>
        {connected ? (
          <Text color={ui.color.accent}>MCP</Text>
        ) : (
          <Text color={ui.color.warning}>DEGRADED</Text>
        )}
      </Text>
    </Box>
  );
}
