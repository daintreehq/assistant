/**
 * One turn rendered as a run cell: the human's message under a dim YOU label +
 * left accent bar, Daintree's prose under a
 * DAINTREE marker (never the word "assistant"), then the activity branch tree and
 * any in-turn notes. Body prose uses the terminal's own foreground.
 */
import { Box, Text } from "ink";
import type { TurnCell } from "../types.js";
import { glyphs, toneColor, ui } from "../theme.js";
import { ActivityTree } from "./ActivityTree.js";
import { UserMessageCard } from "./UserMessageCard.js";
import { truncate } from "../../utils/text.js";

/**
 * Worst-case rendered height of the compact fallback below, in rows. Kept in sync
 * with {@link CompactTurnCellView}'s layout so the viewport budget in Transcript's
 * `fitCells` can reserve exactly this much for an oversized turn. Update both
 * together if the compact layout gains or loses a row.
 */
export const COMPACT_TURN_LINES = 6;

/**
 * A turn that is too tall for the viewport, rendered as a fixed-height summary
 * instead of being clipped mid-card. Shows the same YOU / DAINTREE markers (so the
 * "who said what" signal and existing assertions survive) with a single truncated
 * line each, then a dim hint that the full turn is scrollable. No border (a clipped
 * round border is exactly the bug we're avoiding), no activity tree, no notes.
 */
function CompactTurnCellView({ turn, width }: { turn: TurnCell; width: number }) {
  const set = glyphs();
  const inner = Math.max(10, width - 2);
  const firstLine = (s: string) => truncate(s.split("\n")[0] ?? "", inner);
  return (
    <Box flexDirection="column" marginBottom={1}>
      {turn.userText ? (
        <>
          <Text dimColor bold>
            YOU
          </Text>
          <Text wrap="truncate">{firstLine(turn.userText)}</Text>
        </>
      ) : null}

      {turn.assistantText || turn.streaming ? (
        <>
          <Text bold color={ui.color.accent}>
            {set.brand} DAINTREE
          </Text>
          <Text wrap="truncate">
            {turn.assistantText ? firstLine(turn.assistantText) : ""}
            {turn.streaming ? <Text dimColor>▌</Text> : null}
          </Text>
        </>
      ) : null}

      <Text dimColor wrap="truncate">
        {set.continuation}… truncated to fit — widen or grow the terminal to read
        it all
      </Text>
    </Box>
  );
}

export function TurnCellView({
  turn,
  width,
  now,
  expanded = false,
  compact = false,
}: {
  turn: TurnCell;
  width: number;
  now?: number;
  expanded?: boolean;
  /** Render the fixed-height summary instead of the full card (see Transcript). */
  compact?: boolean;
}) {
  const set = glyphs();
  if (compact) return <CompactTurnCellView turn={turn} width={width} />;
  return (
    <Box flexDirection="column" marginBottom={1}>
      {turn.userText ? (
        <UserMessageCard text={turn.userText} width={width} />
      ) : null}

      {turn.assistantText || turn.streaming ? (
        <Box flexDirection="column">
          <Text bold color={ui.color.accent}>
            {set.brand} DAINTREE
          </Text>
          <Text>
            {turn.assistantText}
            {turn.streaming ? <Text dimColor>▌</Text> : null}
          </Text>
        </Box>
      ) : null}

      <ActivityTree
        activities={turn.activities}
        width={width}
        now={now}
        expanded={expanded}
      />

      {turn.notes.map((n) => {
        const tone =
          n.level === "error"
            ? "danger"
            : n.level === "warn"
              ? "warning"
              : "active";
        const sym =
          n.level === "error"
            ? set.failed
            : n.level === "warn"
              ? set.attention
              : set.bullet;
        return (
          <Text key={n.id}>
            <Text color={toneColor(tone)}>
              {set.continuation}
              {sym}{" "}
            </Text>
            {n.text}
          </Text>
        );
      })}
    </Box>
  );
}
