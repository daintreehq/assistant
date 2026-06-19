/**
 * Markdown → styled terminal text for the Daintree transcript.
 *
 * The model speaks markdown; the cockpit should *show* it (bold, `code`,
 * headings, lists) instead of printing the raw markers. We run finalized turn
 * text through `marked` + `marked-terminal`, which emits an ANSI string that an
 * Ink <Text> renders verbatim — Ink measures/wraps ANSI children via
 * string-width / wrap-ansi, so the styling respects the live cockpit width.
 *
 * WHY only finalized turns (see TurnCellView): while a turn streams we print
 * raw text + a caret and parse exactly once when it completes. `marked` builds
 * a full AST, so a half-typed `**` is rendered as literal text, never as a
 * broken/unclosed escape — but finalize-only also means the cell commits to
 * Ink <Static> and renders just once (no per-token re-parse, no mid-word
 * restyle flicker). The single raw→styled "snap" at end-of-turn is intentional.
 *
 * Styling maps onto the semantic palette in theme.ts. Body prose is left
 * unstyled so it keeps the terminal's own foreground (the "never force white"
 * rule); only inline/section spans get a hue. reflowText:false hands wrapping
 * to Ink rather than hard-wrapping to 80 columns.
 */
import { Marked, type MarkedExtension, type Token, type Tokens } from "marked";
import { markedTerminal } from "marked-terminal";
import { Chalk } from "chalk";
import stripAnsi from "strip-ansi";
import { ui, terminalThemeMode, glyphs } from "./theme.js";

// marked-terminal emits ANSI even at chalk level 0, and Ink passes text-child
// escapes straight through (it never strips them), so color has to be decided
// here: bake truecolor when on, strip every escape when off. The cockpit already
// renders truecolor theme hex elsewhere, so level 3 is the right "on" target.
const colorize = terminalThemeMode() !== "none" && !process.env.NO_COLOR;
const k = new Chalk({ level: colorize ? 3 : 0 });

// An isolated Marked instance so registering the terminal renderer never mutates
// the shared `marked` singleton other code might import.
const md = new Marked();
// `@types/marked-terminal` (v6) still types markedTerminal() as returning a
// TerminalRenderer; v7 returns a MarkedExtension for marked.use(). The options
// object below is still type-checked against TerminalRendererOptions — only the
// return shape is stale — so we validate the call and cast just the result.
md.use(
  markedTerminal({
    reflowText: false, // Ink owns wrapping; don't hard-wrap to 80 cols.
    unescape: true, // render ' and " literally, not &#39; / &quot;.
    tab: 2,
    showSectionPrefix: false, // headings show their text, not a leading '#'.
    // Body paragraphs are deliberately absent here so prose stays on the
    // terminal foreground; only these semantic spans carry a hue.
    strong: k.bold,
    em: k.italic,
    del: k.strikethrough,
    codespan: k.hex(ui.color.info), // `inline code` → cyan
    // Fenced blocks are syntax-highlighted by cli-highlight; `code` is only the
    // fallback styling if highlighting throws. Cyan keeps that fallback on-theme.
    code: k.hex(ui.color.info),
    firstHeading: k.hex(ui.color.accent).bold,
    heading: k.hex(ui.color.accent).bold,
    blockquote: k.dim,
    link: k.hex(ui.color.info).underline,
    href: k.hex(ui.color.info).underline,
  }) as unknown as MarkedExtension,
);

// Tables are the one markdown construct that assumes a wide, fixed canvas:
// marked-terminal renders them as a cli-table3 grid sized to the content, and in
// the narrow inline cockpit that grid is wider than the column, so Ink hard-wraps
// every cell and the borders shred (see the bug report). We override ONLY the
// table renderer (registered after markedTerminal so this wins per-method while
// its inline/block styling is untouched) to emit a width-agnostic record list
// instead: the first column becomes a bulleted heading and the remaining columns
// render as indented `Header: value` lines. We re-render each cell with
// `this.parser.parseInline` so inline styling inside a cell (e.g. `code` → cyan)
// survives the transform. A prompt rule also nudges the model away from tables;
// this is the deterministic backstop for tables it emits anyway or quotes
// verbatim from MCP docs.
function tableToList(
  this: { parser: { parseInline(tokens: Token[]): string } },
  token: Tokens.Table,
): string {
  const bullet = glyphs().bullet;
  const headers = token.header.map((cell) => this.parser.parseInline(cell.tokens));
  const records = token.rows.map((row) => {
    const cells = row.map((cell) => this.parser.parseInline(cell.tokens));
    const lines: string[] = [];
    cells.forEach((value, i) => {
      const text = value.trim();
      if (i === 0) {
        // First column is the record's identity → bulleted heading line.
        lines.push(`${bullet} ${text}`);
      } else if (text) {
        // Skip empty cells so a sparse row doesn't sprout blank `Header:` lines.
        const label = headers[i]?.trim();
        lines.push(`  ${label ? `${k.bold(label)}: ` : ""}${text}`);
      }
    });
    return lines.join("\n");
  });
  return `${records.join("\n")}\n`;
}
md.use({ renderer: { table: tableToList } });

/**
 * Render finalized assistant markdown to a styled ANSI string for an Ink
 * <Text>. Synchronous — no async marked extensions are registered. The trailing
 * blank lines `marked` appends are trimmed so cells don't grow vertical gaps.
 *
 * The input is stripped of any pre-existing escapes first: `marked` does not
 * neutralize raw ANSI in text, and Ink forwards text-child escapes untouched,
 * so untrusted model output could otherwise inject styling or OSC-8 links. When
 * color is off we strip the *output* too (covers SGR plus OSC-8/256-color that a
 * bare `ESC[…m` regex would miss).
 */
export function renderMarkdown(text: string): string {
  const out = (md.parse(stripAnsi(text)) as string).replace(/\n+$/, "");
  return colorize ? out : stripAnsi(out);
}
