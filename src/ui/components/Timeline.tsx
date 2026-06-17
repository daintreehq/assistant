import { Box, Text } from "ink";
import type { TimelineItem } from "../types.js";
import { MessageBubble } from "./MessageBubble.js";
import { ToolCallRow } from "./ToolCallRow.js";

export function Timeline({
  items,
  height,
}: {
  items: TimelineItem[];
  height: number;
}) {
  // Show the most recent items that fit; the dashboard is a live cockpit, not a
  // scrollback buffer (use --no-alt-screen / --classic for full history).
  const visible = items.slice(-Math.max(1, height - 1));
  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      {visible.length === 0 ? (
        <Box flexGrow={1} alignItems="center" justifyContent="center">
          <Text dimColor>
            Ask Daintree to inspect worktrees, spawn agents, or watch terminals.
          </Text>
        </Box>
      ) : (
        visible.map((item) =>
          item.kind === "tool" ? (
            <ToolCallRow key={item.id} item={item} />
          ) : (
            <MessageBubble key={item.id} item={item} />
          ),
        )
      )}
    </Box>
  );
}
