/**
 * One turn rendered as a run cell: the human's message under a dim YOU label +
 * left accent bar, Daintree's prose under a
 * DAINTREE marker (never the word "assistant"), then the activity branch tree and
 * any in-turn notes. Body prose uses the terminal's own foreground.
 *
 * While the turn is in flight (`state === "active"`) we print the raw text — plus
 * a caret while tokens are actually streaming — so the human sees them land in
 * place. Once the turn finalizes (complete/failed/cancelled) we render the prose
 * through OpenTUI's NATIVE `<markdown>` renderable so bold/`code`/headings/lists
 * show styled rather than as raw markers. We gate on `state`, NOT `streaming`: a
 * tool call mid-turn stops the caret (`streaming` → false) while the turn stays
 * active, so keying on `streaming` would briefly markdown-render then flip back to
 * raw. Parsing only finalized text means the markdown is built once with no
 * partial-token churn.
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
import { glyphs, toneColor, ui, unicodeOk, terminalThemeMode } from "../theme.js";
import { ActivityTree } from "./ActivityTree.js";
import { ThinkingDot } from "./ThinkingDot.js";
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
  // so it never doubles up with prose or the activity tree.
  const ascii = !unicodeOk();
  const thinking =
    turn.state === "active" &&
    turn.assistantText.length === 0 &&
    turn.activities.length === 0;
  const finalized = turn.state !== "active";
  // Each transcript cell owns the single blank line ABOVE it (marginTop), never
  // below. A leading blank is deterministic — the native renderer reflows the whole
  // tree, so owning the gap as a leading margin keeps exactly one blank line before
  // every turn and never doubles with a neighbour's margin.
  return (
    <box flexDirection="column" marginTop={1}>
      {turn.userText ? (
        <UserMessageCard text={turn.userText} width={width} />
      ) : null}

      {turn.assistantText || turn.streaming || thinking ? (
        <box flexDirection="column">
          <text
            fg={ui.color.accent}
            attributes={TextAttributes.BOLD}
          >
            {set.brand} DAINTREE
          </text>
          {thinking ? (
            // ThinkingDot is its own `<text>` (an animated spinner), and a native
            // `<text>` may NOT nest another `<text>` — so the dot and the dim
            // "Thinking" label sit side by side in a row box rather than as inline
            // runs of one `<Text>` (the Ink shape). Both still read as one line.
            <box flexDirection="row">
              <ThinkingDot ascii={ascii} />
              <text attributes={TextAttributes.DIM}> Thinking</text>
            </box>
          ) : finalized && turn.assistantText ? (
            // Finalized prose: native markdown styling (built once). A bare
            // `<markdown content="">` renders nothing, so the empty case is handled
            // by the branch above (it only enters here when assistantText is set).
            <markdown content={turn.assistantText} syntaxStyle={MARKDOWN_STYLE} />
          ) : (
            // Active turn (or finalized-but-empty): raw text + a streaming caret so
            // the human watches tokens land in place. A native `<text>` may not nest
            // a `<text>`, so the caret is a `<span>` child of the same `<text>`.
            <text>
              {turn.assistantText}
              {turn.streaming ? (
                <span attributes={TextAttributes.DIM}>▌</span>
              ) : null}
            </text>
          )}
        </box>
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
