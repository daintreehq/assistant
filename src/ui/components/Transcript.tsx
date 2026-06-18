/**
 * The run-oriented transcript. Folds completed turns and the one in-flight turn
 * into a single column, budgeting by RENDERED line count (not item count) so a
 * long answer doesn't silently push the active run off-screen. Committed turns
 * read as stable cells; only the last turn mutates.
 */
import { Box, Text } from "ink";
import type { TranscriptCell } from "../types.js";
import { TurnCellView } from "./TurnCellView.js";
import { glyphs, toneColor, ui } from "../theme.js";

function wrapLines(text: string, width: number): number {
  if (!text) return 0;
  const w = Math.max(1, width);
  return text
    .split("\n")
    .reduce((n, l) => n + Math.max(1, Math.ceil(l.length / w)), 0);
}

/** Rough rendered-height estimate for a cell, used only for the viewport budget. */
function estimateLines(cell: TranscriptCell, width: number): number {
  if (cell.kind === "note") return wrapLines(cell.text, width);
  if (cell.kind === "command")
    return 1 + Math.min(8, wrapLines(cell.text, width));
  let n = 1; // bottom margin
  if (cell.userText) n += 1 + wrapLines(cell.userText, width);
  if (cell.assistantText) n += 1 + wrapLines(cell.assistantText, width - 2);
  else if (cell.streaming) n += 2;
  n += cell.activities.length;
  n += cell.notes.length;
  return Math.max(1, n);
}

/** Take the most recent cells that fit within `height` rendered lines. */
function fitCells(
  cells: TranscriptCell[],
  height: number,
  width: number,
): TranscriptCell[] {
  const out: TranscriptCell[] = [];
  let used = 0;
  for (let i = cells.length - 1; i >= 0; i--) {
    const cost = estimateLines(cells[i], width);
    if (out.length > 0 && used + cost > height) break;
    out.unshift(cells[i]);
    used += cost;
  }
  return out;
}

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
  return (
    <Box marginBottom={1}>
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
    <Box flexDirection="column" marginBottom={1} paddingLeft={1}>
      {cell.title ? (
        <Text bold color={ui.color.info}>
          {cell.title}
        </Text>
      ) : null}
      {cell.text ? <Text dimColor>{cell.text}</Text> : null}
    </Box>
  );
}

export function Transcript({
  cells,
  height,
  width = 72,
  now,
  expanded = false,
  emptyText = "Ask Daintree to inspect worktrees, delegate edits, or watch terminals.",
}: {
  cells: TranscriptCell[];
  height: number;
  width?: number;
  now?: number;
  expanded?: boolean;
  /** Override the empty-state line (the sidebar uses a shorter one). */
  emptyText?: string;
}) {
  const visible = fitCells(cells, Math.max(1, height), width);
  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      {visible.length === 0 ? (
        <Box flexGrow={1} alignItems="center" justifyContent="center">
          <Text dimColor>{emptyText}</Text>
        </Box>
      ) : (
        visible.map((cell) =>
          cell.kind === "turn" ? (
            <TurnCellView
              key={cell.id}
              turn={cell}
              width={width}
              now={now}
              expanded={expanded}
            />
          ) : cell.kind === "note" ? (
            <NoteView key={cell.id} cell={cell} />
          ) : (
            <CommandView key={cell.id} cell={cell} />
          ),
        )
      )}
    </Box>
  );
}
