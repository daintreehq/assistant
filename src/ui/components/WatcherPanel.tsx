import { Box, Text } from "ink";
import type { WatcherRecord } from "../../schemas.js";
import { theme, watcherBadge } from "../theme.js";
import { truncate } from "../../utils/text.js";

export function WatcherPanel({
  watchers,
  height,
}: {
  watchers: WatcherRecord[];
  height: number;
}) {
  const visible = watchers.slice(0, Math.max(0, height - 1));
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.info}>Watchers</Text>
      {visible.length === 0 ? (
        <Text dimColor>none</Text>
      ) : (
        visible.map((w) => {
          const badge = watcherBadge(w.lastClassification);
          return (
            <Text key={w.id}>
              <Text color={theme.brand}>{w.id}</Text>{" "}
              <Text color={badge.color}>{badge.label}</Text>{" "}
              <Text dimColor>{truncate(w.title, 18)}</Text>
            </Text>
          );
        })
      )}
    </Box>
  );
}
