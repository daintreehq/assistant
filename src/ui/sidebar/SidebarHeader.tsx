import { Box, Text } from "ink";
import type { HeaderStatus } from "./model.js";
import { theme } from "../theme.js";

/**
 * Two-line status capsule for sidebar mode:
 *   Daintree                             ● live
 *   assistant        mcp ✓  op · 4w · 2!
 */
export function SidebarHeader({ status }: { status: HeaderStatus }) {
  return (
    <Box flexDirection="column">
      <Box justifyContent="space-between">
        <Text bold color={theme.brand}>
          Daintree
        </Text>
        <Text color={status.live ? theme.ok : theme.warn}>
          ● {status.liveLabel}
        </Text>
      </Box>
      <Box justifyContent="space-between">
        <Text dimColor>{status.project}</Text>
        <Text dimColor>
          mcp <Text color={status.mcpOk ? theme.ok : theme.warn}>{status.mcpOk ? "✓" : "!"}</Text>
          {"  "}
          <Text color="white">{status.tier}</Text>
          {" · "}
          <Text color={status.watcherCount ? theme.info : theme.dim}>{status.watcherCount}w</Text>
          {" · "}
          <Text color={status.attentionCount ? theme.warn : theme.dim}>
            {status.attentionCount}!
          </Text>
        </Text>
      </Box>
    </Box>
  );
}
