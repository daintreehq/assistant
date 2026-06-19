import { render } from "ink-testing-library";
import { renderMarkdown } from "../../src/ui/markdown.js";
import { TurnCellView } from "../../src/ui/components/TurnCellView.js";
import type { TurnCell } from "../../src/ui/types.js";

// Strip ANSI so assertions are about the visible text, not the styling escapes
// (color is environment-dependent; the markers being *gone* is what matters).
const stripAnsi = (s: string) => s.replace(/\x1b\[[0-9;]*m/g, "");

function turn(over: Partial<TurnCell>): TurnCell {
  return {
    kind: "turn",
    id: "t1",
    userText: "",
    assistantText: "",
    streaming: false,
    state: "complete",
    ts: 1_700_000_000_000,
    notes: [],
    activities: [],
    ...over,
  };
}

describe("renderMarkdown", () => {
  it("returns a string synchronously", () => {
    expect(typeof renderMarkdown("hello")).toBe("string");
  });

  it("converts inline markers to styled spans and drops the raw syntax", () => {
    const out = stripAnsi(renderMarkdown("Here's **bold** and `code` and *it*."));
    expect(out).toContain("Here's bold and code and it.");
    expect(out).not.toContain("**");
    expect(out).not.toContain("`");
  });

  it("renders a heading without its leading '#'", () => {
    const out = stripAnsi(renderMarkdown("# Title"));
    expect(out).toContain("Title");
    expect(out).not.toContain("#");
  });

  it("unescapes entities so quotes/apostrophes render literally", () => {
    expect(stripAnsi(renderMarkdown("can't \"do\" it"))).toContain('can\'t "do" it');
    expect(renderMarkdown("can't")).not.toContain("&#39;");
  });

  it("renders list items without the literal markdown bullet syntax", () => {
    const out = stripAnsi(renderMarkdown("- one\n- two"));
    expect(out).toContain("one");
    expect(out).toContain("two");
    expect(out).not.toContain("- one"); // the raw dash bullet is consumed
    expect(out).not.toContain("- two");
  });

  it("does not leave trailing blank lines", () => {
    expect(renderMarkdown("hello")).not.toMatch(/\n\s*$/);
  });

  it("strips ANSI escapes injected in the input (no passthrough)", () => {
    const out = renderMarkdown("\x1b[31mred\x1b[0m **bold**");
    expect(out).not.toContain("\x1b[31m"); // injected color gone
    expect(stripAnsi(out)).toContain("red bold");
  });

  it("returns empty for empty input", () => {
    expect(renderMarkdown("")).toBe("");
  });
});

describe("TurnCellView markdown", () => {
  it("styles finalized prose (no raw markers, no caret)", () => {
    const frame = stripAnsi(
      render(
        <TurnCellView turn={turn({ assistantText: "do **it** now" })} width={72} />,
      ).lastFrame() ?? "",
    );
    expect(frame).toContain("do it now");
    expect(frame).not.toContain("**");
    expect(frame).not.toContain("▌");
  });

  it("shows raw text and a caret while streaming", () => {
    const frame = stripAnsi(
      render(
        <TurnCellView
          turn={turn({
            assistantText: "do **it**",
            streaming: true,
            state: "active",
          })}
          width={72}
        />,
      ).lastFrame() ?? "",
    );
    expect(frame).toContain("do **it**"); // raw markers, not yet parsed
    expect(frame).toContain("▌"); // streaming caret
  });

  it("keeps prose raw and caret-less during an active tool phase", () => {
    // A tool call mid-turn stops the caret (streaming=false) but the turn stays
    // active — prose must remain raw, NOT markdown-rendered, until it finalizes.
    const frame = stripAnsi(
      render(
        <TurnCellView
          turn={turn({
            assistantText: "do **it**",
            streaming: false,
            state: "active",
          })}
          width={72}
        />,
      ).lastFrame() ?? "",
    );
    expect(frame).toContain("do **it**"); // still raw
    expect(frame).not.toContain("▌"); // caret gone with the stream
  });

  it("renders markdown for a cancelled turn (finalized, no caret)", () => {
    const frame = stripAnsi(
      render(
        <TurnCellView
          turn={turn({
            assistantText: "do **it**",
            streaming: false,
            state: "cancelled",
          })}
          width={72}
        />,
      ).lastFrame() ?? "",
    );
    expect(frame).toContain("do it");
    expect(frame).not.toContain("**");
    expect(frame).not.toContain("▌");
  });
});
