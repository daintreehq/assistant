import { Box, Text } from "ink";
import type { QueueEvent } from "../../schemas.js";
import { glyphs, severityTone, toneColor, topSeverity } from "../theme.js";
import { truncate } from "../../utils/text.js";

/**
 * A one-line attention strip directly above the composer, in the user's eyeline.
 * It names the MOST URGENT event (a title is far more useful than a bare count)
 * and rolls the rest into "· N more", pointing at `^O` for the full queue. It
 * renders only when the inbox has items — there is no empty state.
 */
export function AttentionBanner({
  events,
  width = 72,
}: {
  events: QueueEvent[];
  width?: number;
}) {
  if (events.length === 0) return null;
  const top = events[0];
  const sev = top.severity ?? topSeverity(events) ?? "attention";
  const more = events.length - 1;
  const set = glyphs();
  return (
    <Box justifyContent="space-between">
      <Text color={toneColor(severityTone(sev))}>
        {set.attention} {truncate(top.title ?? "needs attention", width - 18)}
        {more > 0 ? <Text dimColor> · {more} more</Text> : null}
      </Text>
      <Text dimColor>^O inspect</Text>
    </Box>
  );
}
