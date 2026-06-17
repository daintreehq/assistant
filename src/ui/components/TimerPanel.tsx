import { Box, Text } from "ink";
import type { TimerRecord } from "../../schemas.js";
import { theme } from "../theme.js";
import { truncate } from "../../utils/text.js";

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

export function TimerPanel({
  timers,
  height,
}: {
  timers: TimerRecord[];
  height: number;
}) {
  const visible = timers.slice(0, Math.max(0, height - 1));
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text color={theme.info}>Timers</Text>
      {visible.length === 0 ? (
        <Text dimColor>none</Text>
      ) : (
        visible.map((t) => (
          <Text key={t.id}>
            <Text color={theme.brand}>{clock(t.fireAt)}</Text>{" "}
            <Text dimColor>{truncate(t.title, 26)}</Text>
          </Text>
        ))
      )}
    </Box>
  );
}
