import { Box, Text } from "ink";
import { theme } from "../theme.js";

const ROWS: Array<[string, string]> = [
  ["/status", "MCP connection, project, models, tier"],
  ["/inbox [sev]", "queued watcher/timer events"],
  ["/watchers /timers", "active watchers / scheduled timers"],
  ["/audit [n]", "recent tool calls"],
  ["/tools [q]", "list/search tools"],
  ["/permissions <tier>", "supervisor | operator | system"],
  ["/compact", "summarize the conversation"],
  ["/doctor", "environment check"],
  ["/quit", "exit"],
  ["", ""],
  ["?", "toggle this help"],
  ["^O", "toggle the operations deck"],
  ["^C", "shut down cleanly"],
];

export function HelpOverlay() {
  return (
    <Box
      position="absolute"
      marginLeft={4}
      marginTop={2}
      borderStyle="round"
      borderColor={theme.brand}
      paddingX={2}
      paddingY={1}
      flexDirection="column"
    >
      <Text bold color={theme.brand}>
        Daintree Assistant — help
      </Text>
      <Box marginTop={1} flexDirection="column">
        {ROWS.map(([k, v], i) => (
          <Text key={i}>
            <Text color={theme.info}>{k.padEnd(20)}</Text>
            <Text dimColor>{v}</Text>
          </Text>
        ))}
      </Box>
      <Box marginTop={1}>
        <Text dimColor>I supervise Daintree and spawn agents — I never edit files directly.</Text>
      </Box>
    </Box>
  );
}
