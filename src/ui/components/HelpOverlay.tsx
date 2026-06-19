import { Box, Text } from "ink";
import { BrandMark, KeyHint } from "../primitives.js";
import { ui } from "../theme.js";
import { overlayEntries } from "../../commandRegistry.js";

// Derived from the shared command registry so the overlay can't drift from the
// commands the handlers actually accept (issue #50).
const COMMANDS: Array<[string, string]> = overlayEntries();

const KEYS: Array<[string, string]> = [
  ["^O", "operations surface"],
  ["^X", "expand tool detail in the transcript"],
  ["Tab", "complete a slash command"],
  ["Esc", "return home"],
  ["^C", "shut down cleanly"],
];

// Composer editing — the readline/native-text-field set the input understands.
const EDIT_KEYS: Array<[string, string]> = [
  ["↑ ↓", "recall previous prompts (at line edges)"],
  ["⌥← ⌥→", "move by word (also ^← ^→)"],
  ["Home End", "start / end of line (also ^A ^E)"],
  ["⌥⌫", "delete previous word (also ^W)"],
  ["^U", "delete the whole line (also ⌘⌫)"],
  ["^K", "delete to end of line"],
  ["^Y", "restore the last killed text"],
  ["\\ ⏎", "newline without sending"],
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
      <Box marginTop={1} flexDirection="column">
        <Text color={ui.color.muted}>editing</Text>
        {EDIT_KEYS.map(([k, v]) => (
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
