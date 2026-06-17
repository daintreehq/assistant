import { Box, Text } from "ink";
import type { TimelineItem } from "../types.js";
import { theme } from "../theme.js";

export function MessageBubble({ item }: { item: TimelineItem }) {
  if (item.kind === "user") {
    return (
      <Box marginBottom={1}>
        <Text color="white">
          <Text bold color={theme.info}>
            you{" "}
          </Text>
          {item.text}
        </Text>
      </Box>
    );
  }

  if (item.kind === "assistant") {
    return (
      <Box flexDirection="column" marginBottom={1}>
        <Text bold color={theme.brand}>
          assistant
        </Text>
        <Text>
          {item.text}
          {item.streaming ? <Text color={theme.dim}>▌</Text> : ""}
        </Text>
      </Box>
    );
  }

  if (item.kind === "system") {
    const color =
      item.level === "error"
        ? theme.error
        : item.level === "warn"
          ? theme.warn
          : theme.info;
    return (
      <Box marginBottom={1}>
        <Text color={color}>◆ {item.text}</Text>
      </Box>
    );
  }

  if (item.kind === "command") {
    return (
      <Box flexDirection="column" marginBottom={1} paddingLeft={1}>
        <Text bold color={theme.info}>
          {item.title}
        </Text>
        {item.text ? <Text dimColor>{item.text}</Text> : null}
      </Box>
    );
  }

  return null;
}
