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
import { collapseLines, snipRule, wrapText } from "../../utils/text.js";

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
  // Wrap the full message into visual lines, then collapse a long one to a
  // head/snip/tail view (first few + a "+N lines" rule + last few), the way Claude
  // Code abbreviates a long block. Short messages pass through untouched. One bar
  // is rendered per row below, so the gutter stays aligned with whatever we show.
  const rows = collapseLines(wrapText(text, inner));
  return (
    <Box flexDirection="column" marginBottom={1}>
      {/* Quiet who-said-what anchor. Dim so it never competes with the bar. */}
      <Text dimColor bold>
        YOU
      </Text>
      <Box flexDirection="row">
        {/* One bar glyph per rendered row, as a 1-cell unfilled gutter. */}
        <Box flexDirection="column" flexShrink={0}>
          {rows.map((_, i) => (
            <Text key={i} color={s.barColor}>
              {BAR}
            </Text>
          ))}
        </Box>
        <Box
          flexDirection="column"
          paddingX={1}
          backgroundColor={s.backgroundColor}
          // Must stay shrinkable. `inner` is budgeted from the `width` prop, which
          // lags the live terminal during a resize (Daintree animates the pane on
          // show/hide). If this filled body could NOT shrink, a stale-wide card would
          // overflow the live edge and the terminal would autowrap the *filled* row —
          // orphaning a copy into scrollback the same way the status line used to.
          // Letting yoga shrink it to the live parent keeps the fill within bounds;
          // wrap="truncate" on the lines below clips the text. In steady state the
          // content already fits, so this never engages and the card looks identical.
          flexShrink={1}
        >
          {rows.map((row, i) =>
            row.kind === "snip" ? (
              // The snip marker is the divider: a dim, centered "+N lines" rule.
              <Text key={i} color={s.textColor} dimColor wrap="truncate">
                {snipRule(row.hidden, inner)}
              </Text>
            ) : (
              <Text
                key={i}
                color={s.textColor}
                dimColor={s.dimText}
                wrap="truncate"
              >
                {row.text}
              </Text>
            ),
          )}
        </Box>
      </Box>
    </Box>
  );
}
