import { createRef } from "react";
import { cleanup, render } from "ink-testing-library";
import {
  Composer,
  type ComposerHandle,
} from "../../src/ui/components/Composer.js";
import { LIVE_CHROME_MAX_WIDTH } from "../../src/ui/liveChrome.js";

const tick = () => new Promise((r) => setTimeout(r, 20));
const ANSI = /\x1b\[[0-9;]*m/g;

// Unmount every rendered Composer after each test. A *busy* Composer mounts the
// animated ThinkingDot, which holds a live setInterval; without teardown those
// intervals keep firing setState across later tests and race the stdin-driven
// input assertions (a leaked spinner reorders keystrokes / repaints mid-tick).
afterEach(cleanup);

const ESC = "\x1b";
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

  it("keeps the input rules full-width on a wide terminal", () => {
    const { lastFrame } = render(
      <Composer busy={false} contextHint="agents 0 · tmr 0" onSubmit={() => {}} />,
    );
    const ruleWidths = (lastFrame() ?? "")
      .split("\n")
      .map((line) => line.replace(ANSI, ""))
      .filter((line) => /^[─-]+$/.test(line.trim()))
      .map((line) => line.length);
    expect(Math.max(...ruleWidths)).toBeGreaterThan(LIVE_CHROME_MAX_WIDTH);
  });

  it("shows NO thinking/stage/spinner at the input while busy", () => {
    // The active turn shows the live "Thinking" line in the transcript above; the
    // input must not repeat it (no stage text, no spinner glyph).
    const { lastFrame } = render(
      <Composer busy stage="Delegating" onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).not.toContain("Delegating");
    expect(frame).not.toContain("⠋"); // no braille spinner
  });

  it("renders no spinner glyph or queued suffix when idle (#115)", () => {
    const { lastFrame } = render(
      <Composer busy={false} stage="Thinking" queueDepth={2} onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).not.toContain("⠋");
    expect(frame).not.toContain("queued");
  });

  it("surfaces silently-queued follow-ups while busy (#95)", () => {
    const { lastFrame } = render(
      <Composer busy stage="Watching" queueDepth={2} onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("2 queued");
    expect(frame).not.toContain("Watching"); // the stage is no longer shown here
  });

  it("omits the queued count when nothing is waiting (#95)", () => {
    const { lastFrame } = render(
      <Composer busy stage="Watching" queueDepth={0} onSubmit={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).not.toContain("queued");
    expect(frame).not.toContain("Watching");
  });

  it("opens a filtered slash palette as you type a command", () => {
    const { lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} />,
    );
    // Nothing typed yet → no palette.
    expect(lastFrame() ?? "").not.toContain("supervised agents");
  });

  it("surfaces /models in the palette as you type (issue #50)", async () => {
    const { stdin, lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} />,
    );
    stdin.write("/mod");
    await tick();
    expect(lastFrame() ?? "").toContain("/models");
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

  it("stays focused while busy so a follow-up can be typed and queued (#45)", async () => {
    let submitted: string | undefined;
    const { stdin } = render(
      <Composer busy focus onSubmit={(v) => (submitted = v)} />,
    );
    stdin.write("next task");
    await tick();
    stdin.write(ENTER);
    await tick();
    // Busy no longer blocks the composer — the keystrokes land and submit.
    expect(submitted).toBe("next task");
  });

  it("shows the Esc-cancel hint only while busy (#45)", () => {
    const idle = render(<Composer busy={false} focus onSubmit={() => {}} />);
    expect(idle.lastFrame() ?? "").not.toContain("cancel");
    const busy = render(<Composer busy focus onSubmit={() => {}} />);
    expect(busy.lastFrame() ?? "").toContain("cancel");
  });

  // The hint row is a single truncating line; strip ANSI and read it back to assert
  // the ORDER of the promoted hint, and that ^O never appears twice.
  const hintLine = (frame: string) =>
    (frame.replace(ANSI, "").split("\n").find((l) => l.includes("commands")) ?? "");

  it("leads the hint row with ^O when actionable attention is pending (#154)", () => {
    const { lastFrame } = render(
      <Composer busy={false} focus attentionPending onSubmit={() => {}} />,
    );
    const line = hintLine(lastFrame() ?? "");
    expect(line).toContain("inspect ops");
    // ^O is promoted to the front, ahead of the commands hint.
    expect(line.indexOf("inspect ops")).toBeLessThan(line.indexOf("commands"));
    // …and it is emitted exactly once (not duplicated into the trailing slot).
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  it("keeps ^O in its trailing slot when no attention is pending (#154)", () => {
    const { lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} />,
    );
    const line = hintLine(lastFrame() ?? "");
    // Default order: commands first, ^O last.
    expect(line.indexOf("commands")).toBeLessThan(line.indexOf("inspect ops"));
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  it("leads with Esc cancel over ^O when a cancellable turn is in flight (#154)", () => {
    const { lastFrame } = render(
      <Composer busy cancellable focus attentionPending onSubmit={() => {}} />,
    );
    const line = hintLine(lastFrame() ?? "");
    // Cancel takes precedence: Esc leads even though attention is pending.
    expect(line).toContain("cancel");
    expect(line.indexOf("cancel")).toBeLessThan(line.indexOf("commands"));
    expect(line.indexOf("cancel")).toBeLessThan(line.indexOf("inspect ops"));
    // ^O is not promoted here, so it stays a single trailing hint.
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  it("does not promote ^O for a non-cancellable busy turn with attention (#154)", () => {
    // cancellable={false} suppresses the Esc hint, and attention then leads with ^O.
    const { lastFrame } = render(
      <Composer busy cancellable={false} focus attentionPending onSubmit={() => {}} />,
    );
    const line = hintLine(lastFrame() ?? "");
    expect(line).not.toContain("cancel");
    expect(line.indexOf("inspect ops")).toBeLessThan(line.indexOf("commands"));
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  it("Escape on an empty composer while busy aborts the turn (#45)", async () => {
    let cancelled = 0;
    const { stdin } = render(
      <Composer busy focus onSubmit={() => {}} onCancel={() => cancelled++} />,
    );
    stdin.write(ESC);
    await tick();
    expect(cancelled).toBe(1);
  });

  it("Escape with text in the buffer clears it instead of cancelling (#45)", async () => {
    let cancelled = 0;
    const { stdin, lastFrame } = render(
      <Composer busy focus onSubmit={() => {}} onCancel={() => cancelled++} />,
    );
    stdin.write("half typed");
    await tick();
    stdin.write(ESC);
    await tick();
    // The buffer is cleared (cancel-edit gesture), and the turn is NOT aborted.
    expect(cancelled).toBe(0);
    expect(lastFrame() ?? "").not.toContain("half typed");
  });

  it("restore() pushes a pulled-back message back into the buffer (#61)", async () => {
    const ref = createRef<ComposerHandle>();
    const { lastFrame } = render(
      <Composer busy={false} focus onSubmit={() => {}} ref={ref} />,
    );
    await tick();
    // Buffer starts empty (the placeholder shows, not real text).
    expect(lastFrame() ?? "").toContain("supervise");

    ref.current!.restore("pulled back message");
    await tick();
    expect(lastFrame() ?? "").toContain("pulled back message");
  });

  it("restored text can be edited and submitted (#61)", async () => {
    let submitted: string | undefined;
    const ref = createRef<ComposerHandle>();
    const { stdin } = render(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} ref={ref} />,
    );
    await tick();
    ref.current!.restore("edit me");
    await tick();
    // The cursor parks at the end after an external replacement, so typing appends.
    stdin.write("!");
    await tick();
    stdin.write(ENTER);
    await tick();
    expect(submitted).toBe("edit me!");
  });

  it("Escape when idle does not invoke onCancel (#45)", async () => {
    let cancelled = 0;
    const { stdin } = render(
      <Composer busy={false} focus onSubmit={() => {}} onCancel={() => cancelled++} />,
    );
    stdin.write(ESC);
    await tick();
    expect(cancelled).toBe(0);
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
