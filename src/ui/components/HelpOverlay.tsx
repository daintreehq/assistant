import { Box, Text } from "ink";
import { BrandMark, KeyHint } from "../primitives.js";
import { ui } from "../theme.js";

const COMMANDS: Array<[string, string]> = [
  ["/status", "connection, project, models, tier"],
  ["/inbox [sev]", "queued watcher/timer events"],
  ["/watchers /timers", "supervised agents / scheduled ops"],
  ["/audit [n]", "recent tool calls"],
  ["/tools [q]", "list/search tools"],
  ["/permissions <tier>", "supervisor | operator | system"],
  ["/recipes [sub]", "loaded · reload · load <id…> · clear"],
  ["/compact", "summarize the conversation"],
  ["/doctor", "environment check"],
  ["/reconnect", "retry the Daintree connection"],
  ["/quit", "exit"],
];

const KEYS: Array<[string, string]> = [
  ["PgUp/PgDn", "scroll history"],
  ["Home/End", "top / latest"],
  ["^O", "full operations surface"],
  ["^X", "expand tool detail in the transcript"],
  ["Tab", "complete a slash command"],
  ["Esc", "latest / return home"],
  ["^C", "shut down cleanly"],
];

export function HelpOverlay({ width = 72 }: { width?: number }) {
  return (
    <Box
      flexDirection="column"
      borderStyle="round"
      borderColor={ui.color.accent}
      paddingX={2}
      paddingY={1}
      width={width}
    >
      <BrandMark label="DAINTREE — help" />
      <Box marginTop={1} flexDirection="column">
        {COMMANDS.map(([k, v]) => (
          <Text key={k}>
            <Text color={ui.color.info}>{k.padEnd(20)}</Text>
            <Text dimColor>{v}</Text>
          </Text>
        ))}
      </Box>
      <Box marginTop={1} flexDirection="column">
        {KEYS.map(([k, v]) => (
          <Box key={k}>
            <Box width={20}>
              <KeyHint keyName={k} action="" />
            </Box>
            <Text dimColor>{v}</Text>
          </Box>
        ))}
      </Box>
      <Box marginTop={1}>
        <Text dimColor>
          I supervise Daintree and delegate to visible agents — I never edit
          files directly.
        </Text>
      </Box>
    </Box>
  );
}
