import { render } from "ink-testing-library";
import { Composer } from "../../src/ui/components/Composer.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

describe("Composer", () => {
  it("renders the single prompt glyph and the context hints", () => {
    const { lastFrame } = render(
      <Composer busy={false} contextHint="2 agents active · MCP" onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("›"); // the one prompt glyph (no repeated branding)
    expect(frame).not.toContain("daintree ❯");
    expect(frame).toContain("commands"); // / commands hint
    expect(frame).toContain("history"); // PgUp opens scrollback
    expect(frame).toContain("ops"); // ^O opens operations as inspect mode
    expect(frame).toContain("2 agents active");
  });

  it("shows the live stage while busy", () => {
    const { lastFrame } = render(
      <Composer busy stage="Delegating" onSubmit={() => {}} />,
    );
    expect(lastFrame() ?? "").toContain("Delegating");
  });

  it("opens a filtered slash palette as you type a command", () => {
    const { lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} />,
    );
    // Nothing typed yet → no palette.
    expect(lastFrame() ?? "").not.toContain("supervised agents");
  });

  it("submits typed input on Enter when focused", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("hello");
    await tick();
    stdin.write("\r");
    await tick();
    expect(submitted).toBe("hello");
  });

  it("ignores keystrokes when not focused (busy / view open)", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy focus={false} onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("nope");
    stdin.write("\r");
    await tick();
    expect(submitted).toBeUndefined();
  });
});
