/**
 * A compact footer, not a second dashboard. The ledger owns work detail and the
 * host terminal owns scrollback; this line just carries the tier and the
 * smallest useful health rollup.
 */
import { Box, Text } from "ink";
import type { DashboardState } from "../types.js";
import { severityTone, toneColor, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";

export function StatusLine({
  dashboard,
  tier,
  width = 80,
}: {
  dashboard: DashboardState;
  tier?: string;
  width?: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers);
  const attention = dashboard.inbox.length;
  const connected = dashboard.mcp.connected;
  const topSev = dashboard.inbox[0]?.severity ?? "attention";
  const runs = dashboard.workflowRuns ?? [];

  const rightParts = [
    attention > 0 ? `!${attention}` : "",
    runs.length > 0 ? `runs ${runs.length}` : "",
    agents.length > 0 ? `agents ${agents.length}` : "",
    dashboard.timers.length > 0 ? `tmr ${dashboard.timers.length}` : "",
    connected ? "MCP" : "DEGRADED",
  ].filter(Boolean);
  const rightText = rightParts.join(" · ");
  const leftText = `${tier ? `${tier.toUpperCase()} · ` : ""}scroll for history`;
  const leftRoom = Math.max(8, width - rightText.length - 3);

  return (
    <Box justifyContent="space-between" width={width} height={1} overflow="hidden">
      <Text dimColor wrap="truncate">
        {truncate(leftText, leftRoom)}
      </Text>
      <Box>
        {attention > 0 ? (
          <Text color={toneColor(severityTone(topSev))}>
            !{attention}
            <Text dimColor> · </Text>
          </Text>
        ) : null}
        {runs.length > 0 ? (
          <Text dimColor>runs {runs.length} · </Text>
        ) : null}
        {agents.length > 0 ? (
          <Text dimColor>agents {agents.length} · </Text>
        ) : null}
        {dashboard.timers.length > 0 ? (
          <Text dimColor>tmr {dashboard.timers.length} · </Text>
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
