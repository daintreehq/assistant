/**
 * One turn rendered as a run cell: a quiet YOU marker, Daintree's prose under a
 * DAINTREE marker (never the word "assistant"), then the activity branch tree and
 * any in-turn notes. Body prose uses the terminal's own foreground.
 */
import { Box, Text } from "ink";
import type { TurnCell } from "../types.js";
import { glyphs, toneColor, ui } from "../theme.js";
import { ActivityTree } from "./ActivityTree.js";
import { UserMessageCard } from "./UserMessageCard.js";

export function TurnCellView({
  turn,
  width,
  now,
  expanded = false,
}: {
  turn: TurnCell;
  width: number;
  now?: number;
  expanded?: boolean;
}) {
  const set = glyphs();
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
