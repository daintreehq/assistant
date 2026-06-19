/**
 * The run-oriented transcript renderer. In the inline cockpit the transcript is
 * NOT a fixed-height viewport any more — completed turns are committed to the
 * terminal's native scrollback (via <Static> in ControlRoom) and only the
 * in-flight turn lives in the repainting region. So this file's job shrank to two
 * things: a single-cell renderer ({@link CellView}) shared by both regions, and a
 * thin list wrapper kept for the gallery/tests. Committed turns read as stable
 * cells; only the last turn mutates.
 */
import { Box, Text } from "ink";
import type { TranscriptCell } from "../types.js";
import { TurnCellView } from "./TurnCellView.js";
import { glyphs, toneColor, ui } from "../theme.js";

function NoteView({
  cell,
}: {
  cell: Extract<TranscriptCell, { kind: "note" }>;
}) {
  const set = glyphs();
  const tone =
    cell.level === "error"
      ? "danger"
      : cell.level === "warn"
        ? "warning"
        : "active";
  const sym =
    cell.level === "error"
      ? set.failed
      : cell.level === "warn"
        ? set.attention
        : set.bullet;
  // Leading gap (marginTop), matching every other cell — see TurnCellView for why
  // the blank line is owned above the cell rather than below it.
  return (
    <Box marginTop={1}>
      <Text>
        <Text color={toneColor(tone)}>
          {set.continuation}
          {sym}{" "}
        </Text>
        {cell.text}
      </Text>
    </Box>
  );
}

function CommandView({
  cell,
}: {
  cell: Extract<TranscriptCell, { kind: "command" }>;
}) {
  return (
    <Box flexDirection="column" marginTop={1} paddingLeft={1}>
      {cell.title ? (
        <Text bold color={ui.color.info}>
          {cell.title}
        </Text>
      ) : null}
      {cell.text ? <Text dimColor>{cell.text}</Text> : null}
    </Box>
  );
}

/**
 * Render a single transcript cell. Used directly by ControlRoom for both the
 * committed (<Static>) history and the live tail, so a cell looks identical
 * whether it has scrolled into terminal scrollback or is still repainting.
 */
export function CellView({
  cell,
  width,
  now,
  expanded = false,
}: {
  cell: TranscriptCell;
  width: number;
  now?: number;
  expanded?: boolean;
}) {
  if (cell.kind === "turn")
    return (
      <TurnCellView turn={cell} width={width} now={now} expanded={expanded} />
    );
  if (cell.kind === "note") return <NoteView cell={cell} />;
  return <CommandView cell={cell} />;
}

/**
 * A plain top-to-bottom list of cells with an empty-state hint. No height budget
 * or tail-clipping: the inline cockpit relies on native scrollback, so every cell
 * is rendered and the terminal owns what's off-screen. Retained for the gallery
 * and component tests.
 */
export function Transcript({
  cells,
  width = 72,
  now,
  expanded = false,
  emptyText = "Ask Daintree to inspect worktrees, delegate edits, or watch terminals.",
}: {
  cells: TranscriptCell[];
  /** Accepted for back-compat; the inline transcript no longer clips by height. */
  height?: number;
  width?: number;
  now?: number;
  expanded?: boolean;
  /** Override the empty-state line (the sidebar uses a shorter one). */
  emptyText?: string;
}) {
  return (
    <Box flexDirection="column">
      {cells.length === 0 ? (
        <Text dimColor>{emptyText}</Text>
      ) : (
        cells.map((cell) => (
          <CellView
            key={cell.id}
            cell={cell}
            width={width}
            now={now}
            expanded={expanded}
          />
        ))
      )}
    </Box>
  );
}
