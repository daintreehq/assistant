import { Box, Text } from "ink";
import type { DashboardState } from "../types.js";
import { glyph, severityColor, theme, topSeverity } from "../theme.js";

/**
 * The single status line that replaces the whole Operations Deck. One row of
 * packed count chips, each omitted at zero so the line never reads "0 0 0":
 *
 *   › 2  ! 1  ⏱ 1                                    ⚠ degraded
 *
 * When nothing is active it collapses to one calm token (`· watching`) instead
 * of a wall of empty sections — present and reassuring, not noisy. Only the
 * attention chip is saturated (colored by top severity); terminal/timer chips
 * stay dim because they are informational, not alarming.
 */
export function StatusLine({
  dashboard,
}: {
  dashboard: DashboardState;
}) {
  const terminals = dashboard.watchers.length;
  const inbox = dashboard.inbox.length;
  const timers = dashboard.timers.length;
  const degraded = !dashboard.mcp.connected;
  const idle = terminals === 0 && inbox === 0 && timers === 0;
  const sev = topSeverity(dashboard.inbox) ?? "attention";

  return (
    <Box justifyContent="space-between">
      <Box>
        {idle ? (
          <Text color={theme.brand} dimColor>
            {"· watching"}
          </Text>
        ) : (
          <Text>
            {terminals > 0 ? (
              <Text dimColor>
                {glyph.active} {terminals}
                {"   "}
              </Text>
            ) : null}
            {inbox > 0 ? (
              <Text color={severityColor(sev)}>
                {glyph.attention} {inbox}
                {"   "}
              </Text>
            ) : null}
            {timers > 0 ? <Text dimColor>⏱ {timers}</Text> : null}
          </Text>
        )}
      </Box>
      {degraded ? <Text color={theme.warn}>⚠ degraded</Text> : null}
    </Box>
  );
}
