import { render } from "ink-testing-library";
import { Composer } from "../../src/ui/components/Composer.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

const ENTER = "\r";
const UP = "[A";
const CTRL_U = ""; // delete the whole line

describe("Composer", () => {
  it("renders the single prompt glyph and the context hints", () => {
    const { lastFrame } = render(
      <Composer busy={false} contextHint="2 agents active · MCP" onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("›"); // the one prompt glyph (no repeated branding)
    expect(frame).not.toContain("daintree ❯");
    expect(frame).toContain("commands"); // / commands hint
    expect(frame).toContain("inspect ops"); // ^O opens operations as inspect mode
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
    stdin.write(ENTER);
    await tick();
    expect(submitted).toBe("hello");
  });

  it("ignores keystrokes when not focused (busy / view open)", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy focus={false} onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("nope");
    stdin.write(ENTER);
    await tick();
    expect(submitted).toBeUndefined();
  });

  it("places the cursor at the end after a Tab completion", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("/stat");
    await tick();
    stdin.write("\t"); // completes to "/status " (cursor must follow to the end)
    await tick();
    stdin.write("now");
    await tick();
    stdin.write(ENTER);
    await tick();
    expect(submitted).toBe("/status now"); // not "/statnow us"
  });

  it("records accepted prompts and recalls them with ↑", async () => {
    const { stdin, lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} />,
    );
    stdin.write("alpha");
    await tick();
    stdin.write(ENTER); // accepted (onSubmit returns void) → recorded, cleared
    await tick();
    stdin.write("beta");
    await tick();
    stdin.write(ENTER);
    await tick();
    stdin.write(UP); // newest
    await tick();
    expect(lastFrame() ?? "").toContain("beta");
    stdin.write(UP); // older
    await tick();
    expect(lastFrame() ?? "").toContain("alpha");
  });

  it("does not record a rejected submit in history", async () => {
    const { stdin, lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => false} />,
    );
    stdin.write("nope");
    await tick();
    stdin.write(ENTER); // rejected → text kept, NOT recorded
    await tick();
    expect(lastFrame() ?? "").toContain("nope");
    stdin.write(CTRL_U); // clear the kept draft
    await tick();
    stdin.write(UP); // nothing to recall — history is empty
    await tick();
    const frame = lastFrame() ?? "";
    expect(frame).not.toContain("nope");
    expect(frame).toContain("supervise"); // the empty-draft placeholder
  });
});
