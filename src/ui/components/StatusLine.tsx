/**
 * A single status line that prefers CURRENT STATE over inventory counts. Left:
 * what Daintree is doing right now (the active agent, or "Standing by"). Right: a
 * compact rollup — attention count, agent count, and the live MCP badge. The
 * attention chip is the only saturated token; everything else stays dim.
 *
 *   ◌ WORKING term_8 · tests running 18s            !1 · agents 2 · MCP
 *   Standing by                                     OPERATOR · MCP CONNECTED
 */
import { Box, Text } from "ink";
import type { DashboardState } from "../types.js";
import { StateBadge, formatDuration } from "../primitives.js";
import { severityTone, toneColor, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";

export function StatusLine({
  dashboard,
  tier,
  width = 80,
  now = Date.now(),
}: {
  dashboard: DashboardState;
  tier?: string;
  width?: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers);
  const active =
    agents.find((a) => a.classification === "still_working") ?? agents[0];
  const attention = dashboard.inbox.length;
  const connected = dashboard.mcp.connected;
  const topSev = dashboard.inbox[0]?.severity ?? "attention";

  // Reserve room for the right-hand rollup so the line never wraps to 2 rows
  // (which would overflow the fixed-height shell and overlap the row above).
  const rightLen = (attention > 0 ? 6 : 0) + (agents.length > 0 ? 10 : 0) + 10;
  const leftRoom = Math.max(12, width - rightLen);

  return (
    <Box justifyContent="space-between">
      <Box>
        {active ? (
          <Text wrap="truncate">
            <StateBadge tone={active.badge.tone} label={active.badge.label} />
            <Text dimColor>
              {" "}
              {active.id} ·{" "}
              {truncate(active.goal || active.title, Math.max(6, leftRoom - active.id.length - 14))}{" "}
              {formatDuration(Math.max(0, now - active.startedAt))}
            </Text>
          </Text>
        ) : (
          <Text dimColor wrap="truncate">
            Standing by{tier ? ` · ${tier.toUpperCase()}` : ""}
          </Text>
        )}
      </Box>
      <Box>
        {attention > 0 ? (
          <Text color={toneColor(severityTone(topSev))}>
            !{attention}
            <Text dimColor> · </Text>
          </Text>
        ) : null}
        {agents.length > 0 ? (
          <Text dimColor>agents {agents.length} · </Text>
        ) : null}
        {connected ? (
          <Text color={ui.color.accent}>MCP</Text>
        ) : (
          <Text color={ui.color.warning}>DEGRADED</Text>
        )}
      </Box>
    </Box>
  );
}
