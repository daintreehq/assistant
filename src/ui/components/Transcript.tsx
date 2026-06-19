/**
 * The run-oriented transcript. Folds completed turns and the one in-flight turn
 * into a single column, budgeting by RENDERED line count (not item count) so a
 * long answer doesn't silently push the active run off-screen. Committed turns
 * read as stable cells; only the last turn mutates.
 */
import { Box, Text } from "ink";
import type { TranscriptCell } from "../types.js";
import { TurnCellView, COMPACT_TURN_LINES } from "./TurnCellView.js";
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
  // The user card (UserMessageCard) is a YOU label + its own bottom margin around
  // the text — 2 rows of chrome — and it truncates each source line to one row (the
  // accent-bar gutter is one bar glyph per line) rather than wrapping, so cost is
  // line count + 2. Counting only `1 + wrapLines` undercounts the YOU label/margin
  // and could let a turn estimate under the compact threshold yet render over it.
  if (cell.userText) n += 2 + cell.userText.split("\n").length;
  if (cell.assistantText) n += 1 + wrapLines(cell.assistantText, width - 2);
  else if (cell.streaming) n += 2;
  n += cell.activities.length;
  n += cell.notes.length;
  return Math.max(1, n);
}

/** A cell selected for display, plus whether it must render as a compact summary. */
interface VisibleCell {
  cell: TranscriptCell;
  compact: boolean;
}

interface FitResult {
  visible: VisibleCell[];
  /** Older cells above the window the user can still page up to reach. */
  hiddenOlderCount: number;
}

/**
 * Choose the cells to show, anchored `scrollOffset` turns back from the newest.
 *
 * Two guarantees beyond "take the most recent that fit":
 *  - The anchor (newest visible turn) is NEVER sliced. When it alone overflows the
 *    viewport it renders as a fixed-height summary (`compact`) instead — a clipped
 *    round border with no scrollback is exactly the bug this fixes (#97).
 *  - When older cells remain above the window we reserve one row for the
 *    "↑ N older turns" indicator so it never displaces a visible cell.
 */
function fitCells(
  cells: TranscriptCell[],
  height: number,
  width: number,
  scrollOffset = 0,
): FitResult {
  if (cells.length === 0) return { visible: [], hiddenOlderCount: 0 };
  const viewport = Math.max(1, height);
  const clamped = Math.min(
    Math.max(0, Math.floor(scrollOffset)),
    cells.length - 1,
  );
  const anchorIdx = cells.length - 1 - clamped;

  const computeWindow = (budget: number): FitResult => {
    const out: VisibleCell[] = [];
    let used = 0;
    for (let i = anchorIdx; i >= 0; i--) {
      const cell = cells[i];
      // Only the anchor can overflow the whole viewport; collapse it rather than
      // letting overflow:hidden tear its card. Judged against the full viewport
      // (not `budget`, which may be a row smaller to make room for the indicator)
      // so an exactly-fitting turn is never needlessly compacted. `>=` is
      // deliberately conservative — estimateLines can undercount, and a compact
      // summary is a far better failure mode than a clipped card. Older cells that
      // don't fit are dropped (and counted), as before.
      const oversized =
        i === anchorIdx &&
        cell.kind === "turn" &&
        estimateLines(cell, width) >= viewport;
      const cost = oversized ? COMPACT_TURN_LINES : estimateLines(cell, width);
      if (out.length > 0 && used + cost > budget) break;
      out.unshift({ cell, compact: oversized });
      used += cost;
    }
    const topIdx = anchorIdx - out.length + 1;
    return { visible: out, hiddenOlderCount: topIdx };
  };

  // Recompute against a one-row-smaller budget once we know the indicator is needed,
  // so reserving its row can never push the topmost cell into the clip region. Skip
  // it when the window is a single oversized anchor: it's force-included regardless
  // of budget, so reserving achieves nothing — and a compact summary has no border
  // to tear, so its benign trailing margin clipping below the indicator is fine.
  let result = computeWindow(viewport);
  const onlyCompactAnchor =
    result.visible.length === 1 && result.visible[0].compact;
  if (result.hiddenOlderCount > 0 && !onlyCompactAnchor) {
    result = computeWindow(Math.max(1, viewport - 1));
  }
  return result;
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
  scrollOffset = 0,
  emptyText = "Ask Daintree to inspect worktrees, delegate edits, or watch terminals.",
}: {
  cells: TranscriptCell[];
  height: number;
  width?: number;
  now?: number;
  expanded?: boolean;
  /**
   * Turns back from the newest to anchor the view (0 = latest). State lives in the
   * shell (DaintreeInkApp) so this stays a pure presentational component; clamping
   * is internal so an out-of-range value is harmless.
   */
  scrollOffset?: number;
  /** Override the empty-state line (the sidebar uses a shorter one). */
  emptyText?: string;
}) {
  const { visible, hiddenOlderCount } = fitCells(
    cells,
    Math.max(1, height),
    width,
    scrollOffset,
  );
  return (
    <Box flexDirection="column" height={height} overflow="hidden">
      {visible.length === 0 ? (
        <Box flexGrow={1} alignItems="center" justifyContent="center">
          <Text dimColor>{emptyText}</Text>
        </Box>
      ) : (
        <>
          {hiddenOlderCount > 0 ? (
            <Text dimColor wrap="truncate">
              ↑ {hiddenOlderCount} older{" "}
              {hiddenOlderCount === 1 ? "turn" : "turns"} — PgUp
            </Text>
          ) : null}
          {visible.map(({ cell, compact }) =>
            cell.kind === "turn" ? (
              <TurnCellView
                key={cell.id}
                turn={cell}
                width={width}
                now={now}
                expanded={expanded}
                compact={compact}
              />
            ) : cell.kind === "note" ? (
              <NoteView key={cell.id} cell={cell} />
            ) : (
              <CommandView key={cell.id} cell={cell} />
            ),
          )}
        </>
      )}
    </Box>
  );
}
