import { Box, Text } from "ink";
import type { QueueEvent } from "../../schemas.js";
import { glyph, severityColor, topSeverity } from "../theme.js";

/**
 * A sticky one-line banner that sits directly above the status line, in the
 * user's eyeline next to where they type. It renders only when the inbox has
 * items — there is no empty state. The rolled-up count points the user at the
 * `^O` operations overlay for the full queue.
 */
export function AttentionBanner({ events }: { events: QueueEvent[] }) {
  if (events.length === 0) return null;
  const sev = topSeverity(events) ?? "attention";
  const n = events.length;
  return (
    <Box justifyContent="space-between">
      <Text color={severityColor(sev)}>
        {glyph.attention} {n} {n === 1 ? "item needs" : "items need"} attention
      </Text>
      <Text dimColor>^O ops</Text>
    </Box>
  );
}
