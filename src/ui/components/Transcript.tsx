/**
 * The run-oriented transcript renderer. In the inline cockpit the transcript is
 * NOT a fixed-height viewport — completed turns scroll into the terminal's native
 * scrollback and only the in-flight turn lives in the repainting region. So this
 * file's job is two things: a single-cell renderer ({@link CellView}) shared by
 * both regions, and a thin list wrapper kept for the gallery/tests. Committed
 * turns read as stable cells; only the last turn mutates.
 *
 * OpenTUI port: Ink `<Box>`/`<Text>` → `<box>`/`<text>`; `color=` → `fg=`,
 * `dimColor`/`bold` → the DIM/BOLD attributes, and an Ink `<Text>` that nested
 * other `<Text>` runs becomes one `<text>` whose children are `<span>` (a native
 * `<text>` may not contain another `<text>`).
 */
import { TextAttributes } from "@opentui/core";
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
    <box marginTop={1}>
      <text>
        <span fg={toneColor(tone)}>
          {set.continuation}
          {sym}{" "}
        </span>
        {cell.text}
      </text>
    </box>
  );
}

function CommandView({
  cell,
}: {
  cell: Extract<TranscriptCell, { kind: "command" }>;
}) {
  return (
    <box flexDirection="column" marginTop={1} paddingLeft={1}>
      {cell.title ? (
        <text fg={ui.color.info} attributes={TextAttributes.BOLD}>
          {cell.title}
        </text>
      ) : null}
      {cell.text ? (
        <text attributes={TextAttributes.DIM}>{cell.text}</text>
      ) : null}
    </box>
  );
}

/**
 * Render a single transcript cell. Used directly by ControlRoom for both the
 * committed history and the live tail, so a cell looks identical whether it has
 * scrolled into terminal scrollback or is still repainting.
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
    <box flexDirection="column">
      {cells.length === 0 ? (
        <text attributes={TextAttributes.DIM}>{emptyText}</text>
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
    </box>
  );
}
