import { Box, Text } from "ink";
import type { TimelineItem } from "../types.js";
import { compactArgs, truncate } from "../../utils/text.js";
import { theme } from "../theme.js";

export function ToolCallRow({ item }: { item: TimelineItem }) {
  if (item.kind !== "tool") return null;
  const status =
    item.ok === undefined ? (
      <Text color={theme.info}>running</Text>
    ) : item.ok ? (
      <Text color={theme.ok}>ok</Text>
    ) : (
      <Text color={theme.error}>error</Text>
    );
  return (
    <Box flexDirection="column" marginBottom={1} paddingLeft={1}>
      <Box justifyContent="space-between">
        <Text dimColor>
          ⚙ {item.name}({compactArgs(item.args, 80)})
        </Text>
        {status}
      </Box>
      {item.summary ? (
        <Text dimColor>↳ {truncate(item.summary, 120)}</Text>
      ) : null}
    </Box>
  );
}
