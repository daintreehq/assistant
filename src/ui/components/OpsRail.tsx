/**
 * The quiet operations rail shown at wide widths beside the transcript. It is a
 * glanceable summary — NOW / ATTENTION / NEXT — not the full operations surface
 * (that's `^O`). A left rule sets it apart without competing for attention.
 */
import { Box, Text } from "ink";
import type { DashboardState } from "../types.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import { SectionLabel, StateBadge, formatDuration } from "../primitives.js";
import { severityTone, toneColor, ui } from "../theme.js";
import { truncate } from "../../utils/text.js";
import { buildAgentRows } from "../presentation/operations.js";

function clock(ms: number): string {
  try {
    return new Date(ms).toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return "—";
  }
}

export function OpsRail({
  dashboard,
  previews = [],
  width,
  now = Date.now(),
}: {
  dashboard: DashboardState;
  previews?: TerminalPreview[];
  width: number;
  now?: number;
}) {
  const agents = buildAgentRows(dashboard.watchers, previews);
  const active = agents.find((a) => a.classification === "still_working") ?? agents[0];
  const topAttention = dashboard.inbox[0];
  const nextTimer = dashboard.timers[0];
  const inner = Math.max(8, width - 2);
  return (
    <Box
      flexDirection="column"
      width={width}
      borderStyle="single"
      borderColor={ui.color.muted}
      borderTop={false}
      borderRight={false}
      borderBottom={false}
      paddingLeft={1}
      gap={1}
    >
      <Box flexDirection="column">
        <SectionLabel>Now</SectionLabel>
        {active ? (
          <>
            <Text>
              <StateBadge tone={active.badge.tone} label={active.badge.label} />
            </Text>
            <Text dimColor>
              {truncate(active.id, inner)}
            </Text>
            <Text dimColor>
              {truncate(active.goal || active.title, inner - 4)} ·{" "}
              {formatDuration(Math.max(0, now - active.startedAt))}
            </Text>
          </>
        ) : (
          <Text dimColor>Standing by</Text>
        )}
      </Box>

      <Box flexDirection="column">
        <SectionLabel>Attention</SectionLabel>
        {topAttention ? (
          <>
            <Text color={toneColor(severityTone(topAttention.severity))}>
              {truncate(topAttention.title, inner)}
            </Text>
            {dashboard.inbox.length > 1 ? (
              <Text dimColor>+{dashboard.inbox.length - 1} more</Text>
            ) : null}
          </>
        ) : (
          <Text dimColor>None</Text>
        )}
      </Box>

      <Box flexDirection="column">
        <SectionLabel>Next</SectionLabel>
        {nextTimer ? (
          <Text dimColor>
            {clock(nextTimer.fireAt)} {truncate(nextTimer.title, inner - 8)}
          </Text>
        ) : (
          <Text dimColor>Nothing scheduled</Text>
        )}
      </Box>
    </Box>
  );
}
