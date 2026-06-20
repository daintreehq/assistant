/**
 * One turn rendered as a run cell: the human's message under a dim YOU label +
 * left accent bar, Daintree's prose under a
 * DAINTREE marker (never the word "assistant"), then the activity branch tree and
 * any in-turn notes. Body prose uses the terminal's own foreground.
 *
 * While the turn is in flight (`state === "active"`) we print the raw text — plus
 * a caret while tokens are actually streaming — so the human sees them land in
 * place. Once the turn finalizes (complete/failed/cancelled) we render the prose
 * through {@link renderMarkdown} so bold/`code`/headings/lists show styled rather
 * than as raw markers. We gate on `state`, NOT `streaming`: a tool call mid-turn
 * stops the caret (`streaming` → false) while the turn stays active, so keying on
 * `streaming` would briefly markdown-render then flip back to raw. Parsing only
 * finalized text means the markdown is built once (the cell commits to Ink
 * <Static>) with no partial-token churn.
 */
import { Box, Text } from "ink";
import type { TurnCell } from "../types.js";
import { glyphs, toneColor, ui, unicodeOk } from "../theme.js";
import { renderMarkdown } from "../markdown.js";
import { ActivityTree } from "./ActivityTree.js";
import { ThinkingDot } from "./ThinkingDot.js";
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
  // Before any output lands, the active turn shows a live "Thinking" line directly
  // under the DAINTREE marker so the human sees Daintree is busy *in the transcript*
  // (not just the composer). It clears the moment real output starts — the first
  // streamed token (assistantText) or the first tool activity ("the tech loading") —
  // so it never doubles up with prose or the activity tree. Gated on state==="active"
  // so it only ever lives in the repainting live region, never a committed <Static>
  // cell (ThinkingDot animates via setInterval; freezing it into Static is forbidden).
  const ascii = !unicodeOk();
  const thinking =
    turn.state === "active" &&
    turn.assistantText.length === 0 &&
    turn.activities.length === 0;
  // Each transcript cell owns the single blank line ABOVE it (marginTop), never
  // below. A leading blank is deterministic — Ink only trims trailing whitespace,
  // so a bottom margin on the last committed cell collapses at the <Static>→live
  // boundary (the gap between startup notes and the first turn vanished). Owning
  // the gap as a leading margin keeps exactly one blank line before every turn,
  // including the first live one, and never doubles with a neighbour's margin.
  return (
    <Box flexDirection="column" marginTop={1}>
      {turn.userText ? (
        <UserMessageCard text={turn.userText} width={width} />
      ) : null}

      {turn.assistantText || turn.streaming || thinking ? (
        <Box flexDirection="column">
          <Text bold color={ui.color.accent}>
            {set.brand} DAINTREE
          </Text>
          {thinking ? (
            <Text dimColor>
              <ThinkingDot ascii={ascii} /> Thinking
            </Text>
          ) : (
            <Text>
              {turn.state === "active"
                ? turn.assistantText
                : renderMarkdown(turn.assistantText)}
              {turn.streaming ? <Text dimColor>▌</Text> : null}
            </Text>
          )}
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
