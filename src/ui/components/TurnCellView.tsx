/**
 * One turn rendered as a run cell: the human's message under a dim YOU label +
 * left accent bar, Daintree's prose under a
 * DAINTREE marker (never the word "assistant"), then the activity branch tree and
 * any in-turn notes. Body prose uses the terminal's own foreground.
 *
 * While the turn is in flight (`state === "active"`) we stream the COMPLETED lines
 * through OpenTUI's NATIVE `<markdown>` renderable and show the in-progress line as
 * raw text + a caret (see {@link StreamingProse}) — so bold/`code`/headings/lists
 * style AS they're produced, the way other agent CLIs do it, instead of only at the
 * end. Once the turn finalizes (complete/failed/cancelled) the whole text renders as
 * markdown. Splitting at the last newline means the stable block is byte-identical
 * between tokens within a line, so the markdown re-parses once per completed line,
 * not per token.
 *
 * OpenTUI port: the Ink path pre-converted markdown to an ANSI string (via
 * `renderMarkdown` / marked-terminal) and showed it in a `<Text>`. We now hand the
 * finalized prose straight to `<markdown content={…}>` (MarkdownRenderable), which
 * parses + styles natively against {@link MARKDOWN_STYLE} (the semantic palette).
 * The renderable needs a `syntaxStyle`, so we build one once at module load. Empty
 * finalized prose falls back to a plain `<text>` (a bare `<markdown content="">`
 * renders nothing, and we never want an empty hole where prose was expected).
 */
import { SyntaxStyle, RGBA, TextAttributes } from "@opentui/core";
import type { TurnCell } from "../types.js";
import { glyphs, toneColor, ui, terminalThemeMode } from "../theme.js";
import { ActivityTree } from "./ActivityTree.js";
import { LiveRunStatus } from "./LiveRunStatus.js";
import { UserMessageCard } from "./UserMessageCard.js";

// The native `<markdown>` renderable styles inline runs (bold/italic) itself via
// text attributes, but it still requires a `syntaxStyle` for its `default` text
// and any fenced-code highlighting — without one its tree-sitter pass throws and
// the markers leak through unconcealed. We build it once and mirror the semantic
// palette from the old `marked-terminal` mapping in markdown.ts: inline/fenced
// `code` → info (cyan), headings → accent (green), with body prose left on the
// `default` style so it keeps a neutral foreground (the "never force white" rule).
// In `none` theme mode we drop color entirely and lean on the concealed markers.
const colorize = terminalThemeMode() !== "none" && !process.env.NO_COLOR;
const MARKDOWN_STYLE = colorize
  ? SyntaxStyle.fromStyles({
      default: {},
      "markup.heading": { fg: RGBA.fromHex(ui.color.accent), bold: true },
      "markup.raw": { fg: RGBA.fromHex(ui.color.info) },
      "markup.raw.inline": { fg: RGBA.fromHex(ui.color.info) },
      "markup.raw.block": { fg: RGBA.fromHex(ui.color.info) },
      "markup.link": { fg: RGBA.fromHex(ui.color.info), underline: true },
      "markup.link.url": { fg: RGBA.fromHex(ui.color.info), underline: true },
      "markup.link.label": { fg: RGBA.fromHex(ui.color.info), underline: true },
    })
  : SyntaxStyle.fromStyles({ default: {} });

/**
 * In-flight prose, rendered as PLAIN TEXT + a trailing caret — deliberately NOT live
 * markdown. The native `<markdown>` renderable parses asynchronously (tree-sitter), and
 * its measured height DIPS while a line re-parses, then jumps back. In the split-footer
 * cockpit the live footer is sized to its content every frame, so each of those height
 * dips made the whole footer shrink-then-grow and forced a full repaint — i.e. the
 * response visibly FLASHED on every line as it streamed.
 *
 * Plain text only ever GROWS as tokens arrive (no reflow, no async parse), so the footer
 * height is monotonic and never force-repaints mid-stream. The full markdown styling —
 * headings, code, links — lands the moment the turn seals into native scrollback (see
 * the `finalized` branch in TurnCellView), which is the polished result the user sees in
 * history. Stream raw, style on commit (the Claude Code model).
 */
function StreamingProse({
  text,
  streaming,
}: {
  text: string;
  streaming: boolean;
}) {
  return (
    <text>
      {text}
      {streaming ? <span attributes={TextAttributes.DIM}>▌</span> : null}
    </text>
  );
}

/**
 * The live (active-turn) body in a FIXED-height pane: streaming prose on top, the
 * activity tree pinned at the bottom, the whole thing reserving exactly `rows` rows
 * no matter how much has streamed. That invariance is the streaming-flash fix — the
 * footer measures this tree every frame, and a height change forces a full
 * split-footer repaint (the flash). The prose is tail-sliced to the last lines that
 * can fit so the most recent output stays visible; the activity tree (few rows) keeps
 * its natural height and the prose flexes into whatever space is left.
 */
function LiveBody({
  turn,
  width,
  now,
  expanded,
  rows,
}: {
  turn: TurnCell;
  width: number;
  now?: number;
  expanded?: boolean;
  rows: number;
}) {
  // Reserve rows for the activity tree (≈one row each), leave the rest for prose.
  const actRows = Math.min(turn.activities.length, Math.max(0, rows - 1));
  const proseRows = Math.max(1, rows - actRows);
  const lines = turn.assistantText ? turn.assistantText.split("\n") : [];
  const proseTail = lines.slice(-proseRows).join("\n");
  return (
    <box flexDirection="column" height={rows} overflow="hidden" flexShrink={0}>
      <box flexDirection="column" flexGrow={1} flexShrink={1} overflow="hidden">
        {turn.assistantText ? (
          <StreamingProse text={proseTail} streaming={turn.streaming} />
        ) : null}
      </box>
      <ActivityTree
        activities={turn.activities}
        width={width}
        now={now}
        expanded={expanded}
        live
      />
    </box>
  );
}

export function TurnCellView({
  turn,
  width,
  now,
  expanded = false,
  liveMaxRows,
}: {
  turn: TurnCell;
  width: number;
  now?: number;
  expanded?: boolean;
  /**
   * Row budget for an ACTIVE turn's growing body (prose + activity tree). When set,
   * the live body renders inside a FIXED-height pane of this many rows so the footer
   * tree height stays invariant as tokens stream — the structural fix for the
   * streaming flash (a footer-height change forces a full split-footer repaint, see
   * useFooterHeight). Completed content shows as the tail; the full styled turn lands
   * in native scrollback the moment it seals. Unset (gallery/tests/history) = unbounded.
   */
  liveMaxRows?: number;
}) {
  const set = glyphs();
  const finalized = turn.state !== "active";
  // Bound the live body only while the turn is active AND a budget was supplied.
  const bounded = !finalized && liveMaxRows != null && liveMaxRows > 0;
  // The DAINTREE block shows the instant the turn is live (so the cockpit reacts on
  // submit) or once it has said anything. The precise live state — Analyzing /
  // Integrating / Waiting for approval / Cancelling — is named by LiveRunStatus below
  // the marker (it renders nothing while prose streams or tools run, since those are
  // self-evident). The old hardcoded "Thinking" + the `no-text && no-activities` gate
  // (which made the spinner vanish once any activity existed) are gone.
  const showDaintree = turn.state === "active" || !!turn.assistantText;
  // Each transcript cell owns the single blank line ABOVE it (marginTop), never
  // below. A leading blank is deterministic — the native renderer reflows the whole
  // tree, so owning the gap as a leading margin keeps exactly one blank line before
  // every turn and never doubles with a neighbour's margin.
  return (
    <box flexDirection="column" marginTop={1}>
      {turn.userText ? (
        <UserMessageCard text={turn.userText} width={width} />
      ) : null}

      {showDaintree ? (
        <box flexDirection="column">
          {/* Marker + an instant "· received" ack that disappears the moment the model
              starts (analyzing/a token/a tool). Distinct `<span>` runs so the dim ack
              doesn't inherit the bold-accent marker styling. */}
          <text>
            <span fg={ui.color.accent} attributes={TextAttributes.BOLD}>
              {set.brand} DAINTREE
            </span>
            {turn.phase === "received" ? (
              <span attributes={TextAttributes.DIM}> · received</span>
            ) : null}
          </text>
          {/* The live run status sits DIRECTLY under the marker, above the response, so
              a turn reads "DAINTREE → Generating → [the answer]". Animated spinner +
              precise label + live elapsed; null once the turn finalizes or while the
              activity tree is the indicator (tool_running). */}
          <LiveRunStatus turn={turn} now={now} />
          {/* Finalized prose only here (full native-markdown styling over the whole
              text). While ACTIVE the prose lives in the bounded pane below so the
              footer can't grow per token. */}
          {finalized && turn.assistantText ? (
            <markdown content={turn.assistantText} syntaxStyle={MARKDOWN_STYLE} />
          ) : null}
        </box>
      ) : null}

      {bounded ? (
        // The live body — streaming prose + the activity tree — in a FIXED-height pane
        // so the footer never resizes mid-stream. The pane reserves `liveMaxRows`
        // rows; the prose shows its TAIL (last lines), the activity tree sits below it
        // and always shows in full (tools are few). On seal the unbounded branch above
        // + ActivityTree below render the whole turn into scrollback at full fidelity.
        <LiveBody
          turn={turn}
          width={width}
          now={now}
          expanded={expanded}
          rows={liveMaxRows!}
        />
      ) : (
        // Unbounded path (gallery / direct tests / non-live callers): no fixed pane, so
        // the active prose streams here as raw text + caret (finalized prose is the
        // markdown branch above). The activity tree follows it.
        <>
          {!finalized && turn.assistantText ? (
            <StreamingProse text={turn.assistantText} streaming={turn.streaming} />
          ) : null}
          <ActivityTree
            activities={turn.activities}
            width={width}
            now={now}
            expanded={expanded}
            // Animate active rows only while the turn is live; a committed/scrollback
            // render passes false so the spinner timer can't freeze/smear (ThinkingDot).
            live={turn.state === "active"}
          />
        </>
      )}

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
          <text key={n.id}>
            <span fg={toneColor(tone)}>
              {set.continuation}
              {sym}{" "}
            </span>
            {n.text}
          </text>
        );
      })}
    </box>
  );
}
