/**
 * The human's turn, rendered as a distinct boxed card: a quiet YOU label over a
 * bordered, dimmer surface. This is the single strongest "who said what" signal
 * in the transcript — Daintree's own prose stays unboxed by contrast. The fill is
 * theme-aware (see {@link userMessageSurface}) so it never paints a bright block
 * on a dark terminal.
 */
import { Box, Text } from "ink";
import { userMessageSurface } from "../theme.js";
import { truncate } from "../../utils/text.js";

export function UserMessageCard({
  text,
  width,
}: {
  text: string;
  width: number;
}) {
  const s = userMessageSurface();
  // Reserve the border (2) + horizontal padding (2); never narrower than a few
  // words so a tight sidebar still reads.
  const inner = Math.max(10, width - 4);
  return (
    <Box flexDirection="column" marginBottom={1}>
      <Text dimColor bold>
        YOU
      </Text>
      <Box
        flexDirection="column"
        borderStyle="round"
        borderColor={s.borderColor}
        paddingX={1}
        backgroundColor={s.backgroundColor}
      >
        {text.split("\n").map((line, i) => (
          <Text key={i} color={s.textColor} dimColor={s.dimText} wrap="truncate">
            {truncate(line, inner)}
          </Text>
        ))}
      </Box>
    </Box>
  );
}
