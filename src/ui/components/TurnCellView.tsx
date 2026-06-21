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
  const finalized = turn.state !== "active";
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
          {turn.assistantText ? (
            finalized ? (
              // Finalized prose: native markdown styling over the whole text.
              <markdown
                content={turn.assistantText}
                syntaxStyle={MARKDOWN_STYLE}
              />
            ) : (
              // Active: stream completed lines as styled markdown, the in-progress
              // line as raw text + caret (see StreamingProse).
              <StreamingProse
                text={turn.assistantText}
                streaming={turn.streaming}
              />
            )
          ) : null}
        </box>
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
