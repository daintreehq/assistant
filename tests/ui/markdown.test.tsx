import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { renderMarkdown } from "../../src/ui/markdown.js";
import { TurnCellView } from "../../src/ui/components/TurnCellView.js";
import type { TurnCell } from "../../src/ui/types.js";

// renderMarkdown returns an ANSI string (it bakes truecolor for the cockpit), so
// these assertions strip SGR to test the visible text, not the styling escapes
// (color is environment-dependent; the markers being *gone* is what matters).
// captureCharFrame() for the TurnCellView block is already plain text, so it
// needs no stripping.
const stripAnsi = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

// The native <markdown> renderable parses asynchronously (tree-sitter) on a path
// that runs OFF the OpenTUI render loop, so a bare flush()/waitForVisualIdle() can
// return before the parsed text has painted (they only settle the render loop). A
// single fixed wall-clock wait is racy on slow CI runners (the parse can land after
// it), so we POLL: flush + settle + check the frame for the expected text, retrying
// until it lands or we hit a generous ceiling. Returns the committed frame.
async function waitForMarkdown(
  t: {
    flush: () => Promise<void>;
    waitForVisualIdle: () => Promise<void>;
    captureCharFrame: () => string;
  },
  expected: string,
): Promise<string> {
  let frame = "";
  // ~3s ceiling (60 × 50ms) — far past the parse cost, but only ever waits as long
  // as the parse actually takes since we return the moment the text appears.
  for (let i = 0; i < 60; i++) {
    await t.flush();
    await t.waitForVisualIdle();
    frame = t.captureCharFrame();
    if (frame.includes(expected)) return frame;
    await new Promise((r) => setTimeout(r, 50));
  }
  return frame;
}

function turn(over: Partial<TurnCell>): TurnCell {
  return {
    kind: "turn",
    id: "t1",
    userText: "",
    assistantText: "",
    streaming: false,
    state: "complete",
    phase: "complete",
    phaseStartedAt: 1_700_000_000_000,
    ts: 1_700_000_000_000,
    notes: [],
    activities: [],
    ...over,
  };
}

describe("renderMarkdown", () => {
  test("returns a string synchronously", () => {
    expect(typeof renderMarkdown("hello")).toBe("string");
  });

  test("converts inline markers to styled spans and drops the raw syntax", () => {
    const out = stripAnsi(renderMarkdown("Here's **bold** and `code` and *it*."));
    expect(out).toContain("Here's bold and code and it.");
    expect(out).not.toContain("**");
    expect(out).not.toContain("`");
  });

  test("renders a heading without its leading '#'", () => {
    const out = stripAnsi(renderMarkdown("# Title"));
    expect(out).toContain("Title");
    expect(out).not.toContain("#");
  });

  test("unescapes entities so quotes/apostrophes render literally", () => {
    expect(stripAnsi(renderMarkdown("can't \"do\" it"))).toContain('can\'t "do" it');
    expect(renderMarkdown("can't")).not.toContain("&#39;");
  });

  test("renders list items without the literal markdown bullet syntax", () => {
    const out = stripAnsi(renderMarkdown("- one\n- two"));
    expect(out).toContain("one");
    expect(out).toContain("two");
    expect(out).not.toContain("- one"); // the raw dash bullet is consumed
    expect(out).not.toContain("- two");
  });

  test("styles inline markers INSIDE list items", () => {
    // The regression that leaked raw markdown: marked v15 nests a list item's
    // inline children (bold/`code`) under a `text` token's `.tokens`, and
    // marked-terminal's text renderer emitted the raw `.text` instead of recursing,
    // so `* **Project**: \`code\`` printed with the literal `**`/backticks. A
    // paragraph parses fine (its inline renderers run directly) — only the
    // list-item path hit the bug, which is why whole bulleted snapshots went raw.
    const out = stripAnsi(renderMarkdown("* **Project**: `code` here\n* plain"));
    expect(out).toContain("Project");
    expect(out).toContain("code");
    expect(out).toContain("plain");
    expect(out).not.toContain("**");
    expect(out).not.toContain("`");
  });

  test("does not leave trailing blank lines", () => {
    expect(renderMarkdown("hello")).not.toMatch(/\n\s*$/);
  });

  test("strips ANSI escapes injected in the input (no passthrough)", () => {
    const out = renderMarkdown("\x1b[31mred\x1b[0m **bold**");
    expect(out).not.toContain("\x1b[31m"); // injected color gone
    expect(stripAnsi(out)).toContain("red bold");
  });

  test("returns empty for empty input", () => {
    expect(renderMarkdown("")).toBe("");
  });
});

describe("TurnCellView markdown", () => {
  test("styles finalized prose (no raw markers, no caret)", async () => {
    const t = await testRender(
      <TurnCellView turn={turn({ assistantText: "do **it** now" })} width={72} />,
      { width: 72, height: 8 },
    );
    // Finalized prose renders via OpenTUI's NATIVE <markdown> renderable, which
    // parses asynchronously (tree-sitter) OFF the render loop — flush()/visualIdle
    // settle the render loop but can return before the parse lands. A short
    // wall-clock wait lets the parse complete, then flush() commits the repaint.
    const frame = await waitForMarkdown(t, "do it now");
    expect(frame).toContain("do it now");
    expect(frame).not.toContain("**");
    expect(frame).not.toContain("▌");
  });

  test("shows raw text and a caret while streaming", async () => {
    const t = await testRender(
      <TurnCellView
        turn={turn({
          assistantText: "do **it**",
          streaming: true,
          state: "active",
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("do **it**"); // raw markers, not yet parsed
    expect(frame).toContain("▌"); // streaming caret
  });

  test("keeps prose raw and caret-less during an active tool phase", async () => {
    // A tool call mid-turn stops the caret (streaming=false) but the turn stays
    // active — prose must remain raw, NOT markdown-rendered, until it finalizes.
    const t = await testRender(
      <TurnCellView
        turn={turn({
          assistantText: "do **it**",
          streaming: false,
          state: "active",
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("do **it**"); // still raw
    expect(frame).not.toContain("▌"); // caret gone with the stream
  });

  test("acknowledges the turn instantly with '· received' before the model answers", async () => {
    const t = await testRender(
      <TurnCellView
        turn={turn({
          state: "active",
          phase: "received",
          assistantText: "",
          streaming: false,
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("DAINTREE");
    expect(frame).toContain("received"); // instant ack, never the vague "Thinking"
    expect(frame).not.toContain("Thinking");
  });

  test("names the silent 'Analyzing request' gap while active and output-less", async () => {
    // The real pre-token state after `assistant:start`: active, streaming=true,
    // empty assistantText, phase "analyzing". The precise label must show here (not a
    // vague Thinking, and not the stream caret) so the gap is never silent.
    const t = await testRender(
      <TurnCellView
        turn={turn({
          state: "active",
          phase: "analyzing",
          assistantText: "",
          streaming: true,
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Analyzing request");
    expect(frame).not.toContain("Thinking");
    expect(frame).not.toContain("▌"); // the status line replaces the stream caret
  });

  test("shows 'Generating' under DAINTREE while prose streams", async () => {
    // The run reads "DAINTREE → Generating → [the answer]": the status sits under the
    // marker, above the streaming response — not as a notification under the input.
    const t = await testRender(
      <TurnCellView
        turn={turn({
          state: "active",
          phase: "generating",
          assistantText: "hi",
          streaming: true,
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("hi"); // the streaming response
    expect(frame).toContain("Generating"); // ...with the live status above it
    expect(frame).not.toContain("Thinking");
  });

  test("drops the status line once a tool activity begins", async () => {
    // "as the tech [tools] starts loading, it can stop saying it's thinking":
    // the activity tree takes over once work is visible.
    const t = await testRender(
      <TurnCellView
        turn={turn({
          state: "active",
          phase: "tool_running",
          assistantText: "",
          activities: [
            {
              id: "a1",
              name: "fs.read",
              label: "Read",
              state: "active",
              startedAt: 1_700_000_000_000,
            },
          ],
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Read"); // the activity row is what takes over
    expect(frame).not.toContain("Thinking");
    expect(frame).not.toContain("Analyzing");
  });

  test("renders markdown for a cancelled turn (finalized, no caret)", async () => {
    const t = await testRender(
      <TurnCellView
        turn={turn({
          assistantText: "do **it**",
          streaming: false,
          state: "cancelled",
        })}
        width={72}
      />,
      { width: 72, height: 8 },
    );
    // Cancelled is finalized too, so the prose also routes through the async
    // native <markdown> renderable — settle past the parse before asserting.
    const frame = await waitForMarkdown(t, "do it");
    expect(frame).toContain("do it");
    expect(frame).not.toContain("**");
    expect(frame).not.toContain("▌");
  });
});
