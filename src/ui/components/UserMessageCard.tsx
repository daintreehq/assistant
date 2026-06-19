/**
 * The human's turn. Instead of a four-sided box, the message is marked by a left
 * accent bar (`▏`, U+258F — a glyph that hugs the cell's left edge, reading as a
 * crisp rule rather than a floating box-drawing line) over a *subtle* fill, with a
 * dim `YOU` label above as a quiet who-said-what anchor. The bar + fill carry the
 * visual weight; the label just helps the eye land without re-introducing the heavy
 * box. Daintree's own prose stays bare by contrast. Both the bar and fill are
 * theme-aware (see {@link userMessageSurface}) so we never paint a bright block —
 * or, in ansi/none modes, any fill at all — on a dark terminal.
 */
import { Box, Text } from "ink";
import { userMessageSurface } from "../theme.js";
import { truncate } from "../../utils/text.js";

/** U+258F LEFT ONE EIGHTH BLOCK — sits flush against the left edge of the cell. */
const BAR = "▏";

export function UserMessageCard({
  text,
  width,
}: {
  text: string;
  width: number;
}) {
  const s = userMessageSurface();
  // Reserve the bar column (1) + the gap/padding (1) + a right breathing margin
  // so the fill is a card, not an edge-to-edge band; never narrower than a few
  // words so a tight sidebar still reads.
  const inner = Math.max(10, width - 4);
  const lines = text.split("\n");
  return (
    <Box flexDirection="column" marginBottom={1}>
      {/* Quiet who-said-what anchor. Dim so it never competes with the bar. */}
      <Text dimColor bold>
        YOU
      </Text>
      <Box flexDirection="row">
        {/* One bar glyph per visual line, as a 1-cell unfilled gutter. */}
        <Box flexDirection="column" flexShrink={0}>
          {lines.map((_, i) => (
            <Text key={i} color={s.barColor}>
              {BAR}
            </Text>
          ))}
        </Box>
        <Box
          flexDirection="column"
          paddingX={1}
          backgroundColor={s.backgroundColor}
          flexShrink={0}
        >
          {lines.map((line, i) => (
            <Text
              key={i}
              color={s.textColor}
              dimColor={s.dimText}
              wrap="truncate"
            >
              {truncate(line, inner)}
            </Text>
          ))}
        </Box>
      </Box>
    </Box>
  );
}
