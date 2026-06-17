import { Box, Text } from "ink";
import type { QueueEvent } from "../../schemas.js";
import { severityColor, theme } from "../theme.js";
import { truncate } from "../../utils/text.js";

export function InboxPanel({
  events,
  height,
}: {
  events: QueueEvent[];
  height: number;
}) {
  const visible = events.slice(0, Math.max(0, height - 1));
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.info}>Inbox</Text>
      {visible.length === 0 ? (
        <Text dimColor>empty</Text>
      ) : (
        visible.map((e) => (
          <Text key={e.id} color={severityColor(e.severity)}>
            {e.severity[0]} {truncate(e.title, 30)}
            {e.count > 1 ? <Text dimColor> ×{e.count}</Text> : null}
          </Text>
        ))
      )}
    </Box>
  );
}
