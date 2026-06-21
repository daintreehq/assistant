import { test, expect, describe, mock } from "bun:test";
import { act, createRef } from "react";
import { testRender } from "@opentui/react/test-utils";
import {
  Composer,
  type ComposerHandle,
} from "../../src/ui/components/Composer.js";
import { LIVE_CHROME_MAX_WIDTH } from "../../src/ui/liveChrome.js";

// OpenTUI port of the Ink composer suite. `captureCharFrame()` already returns
// plain text (no ANSI), so the old strip-ansi step is gone. Keys are driven with
// `t.mockInput`, each press wrapped in `act()` + a `flush()` so the state update
// (and any cascading effect in MultilineInput) commits and repaints before the
// next keystroke reads the buffer — the editor closes over `value`, so a key must
// see the prior key's committed value.
//
// We render input-driven cases with `kittyKeyboard: true`: under the kitty
// keyboard protocol a lone Escape is unambiguous (raw mode would treat the bare
// ESC byte as the Alt prefix of the *next* key and swallow the cancel gesture),
// and plain Return arrives without a spurious meta flag.
const COMPOSER_SIZE = { width: 80, height: 14, kittyKeyboard: true } as const;

/** Type a string one printable key at a time, flushing between each. */
async function type(t: Awaited<ReturnType<typeof testRender>>, text: string) {
  for (const ch of text) {
    await act(async () => {
      t.mockInput.pressKey(ch);
    });
    await t.flush();
  }
}

/** Press a single named/control key (RETURN, TAB, ARROW_UP, …) and flush. */
async function press(
  t: Awaited<ReturnType<typeof testRender>>,
  key: Parameters<typeof t.mockInput.pressKey>[0],
  modifiers?: Parameters<typeof t.mockInput.pressKey>[1],
) {
  await act(async () => {
    t.mockInput.pressKey(key, modifiers);
  });
  await t.flush();
}

/** Escape via the dedicated helper (clean single ESC under kitty mode). */
async function escape(t: Awaited<ReturnType<typeof testRender>>) {
  await act(async () => {
    t.mockInput.pressEscape();
  });
  await t.flush();
}

describe("Composer", () => {
  test("renders the single prompt glyph and the context hints", async () => {
    const t = await testRender(
      <Composer busy={false} contextHint="2 agents active · MCP" onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("›"); // the one prompt glyph (no repeated branding)
    expect(frame).not.toContain("daintree ❯");
    expect(frame).toContain("commands"); // / commands hint
    expect(frame).toContain("inspect ops"); // ^O opens operations as inspect mode
    expect(frame).toContain("2 agents active");
  });

  test("keeps the input rules full-width on a wide terminal", async () => {
    const t = await testRender(
      <Composer busy={false} contextHint="agents 0 · tmr 0" onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const ruleWidths = t
      .captureCharFrame()
      .split("\n")
      .filter((line) => /^[─-]+$/.test(line.trim()))
      .map((line) => line.trim().length);
    expect(Math.max(...ruleWidths)).toBeGreaterThan(LIVE_CHROME_MAX_WIDTH);
  });

  test("shows NO thinking/stage/spinner at the input while busy", async () => {
    // The active turn shows the live "Thinking" line in the transcript above; the
    // input must not repeat it (no stage text, no spinner glyph).
    const t = await testRender(
      <Composer busy stage="Delegating" onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("Delegating");
    expect(frame).not.toContain("⠋"); // no braille spinner
  });

  test("renders no spinner glyph or queued suffix when idle (#115)", async () => {
    const t = await testRender(
      <Composer busy={false} stage="Thinking" queueDepth={2} onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("⠋");
    expect(frame).not.toContain("queued");
  });

  test("surfaces silently-queued follow-ups while busy (#95)", async () => {
    const t = await testRender(
      <Composer busy stage="Watching" queueDepth={2} onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("2 queued");
    expect(frame).not.toContain("Watching"); // the stage is no longer shown here
  });

  test("omits the queued count when nothing is waiting (#95)", async () => {
    const t = await testRender(
      <Composer busy stage="Watching" queueDepth={0} onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("queued");
    expect(frame).not.toContain("Watching");
  });

  test("opens a filtered slash palette as you type a command", async () => {
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    // Nothing typed yet → no palette.
    expect(t.captureCharFrame()).not.toContain("supervised agents");
  });

  test("surfaces /models in the palette as you type (issue #50)", async () => {
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "/mod");
    expect(t.captureCharFrame()).toContain("/models");
  });

  test("submits typed input on Enter when focused", async () => {
    let submitted: string | undefined;
    const t = await testRender(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "hello");
    await press(t, "RETURN");
    expect(submitted).toBe("hello");
  });

  test("ignores keystrokes when not focused (busy / view open)", async () => {
    let submitted: string | undefined;
    const t = await testRender(
      <Composer busy focus={false} onSubmit={(v) => (submitted = v)} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "nope");
    await press(t, "RETURN");
    expect(submitted).toBeUndefined();
  });

  test("places the cursor at the end after a Tab completion", async () => {
    let submitted: string | undefined;
    const t = await testRender(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "/stat");
    await press(t, "TAB"); // completes to "/status " (cursor must follow to the end)
    await type(t, "now");
    await press(t, "RETURN");
    expect(submitted).toBe("/status now"); // not "/statnow us"
  });

  test("records accepted prompts and recalls them with ↑", async () => {
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "alpha");
    await press(t, "RETURN"); // accepted (onSubmit returns void) → recorded, cleared
    await type(t, "beta");
    await press(t, "RETURN");
    await press(t, "ARROW_UP"); // newest
    expect(t.captureCharFrame()).toContain("beta");
    await press(t, "ARROW_UP"); // older
    expect(t.captureCharFrame()).toContain("alpha");
  });

  test("stays focused while busy so a follow-up can be typed and queued (#45)", async () => {
    let submitted: string | undefined;
    const t = await testRender(
      <Composer busy focus onSubmit={(v) => (submitted = v)} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "next task");
    await press(t, "RETURN");
    // Busy no longer blocks the composer — the keystrokes land and submit.
    expect(submitted).toBe("next task");
  });

  test("shows the Esc-cancel hint only while busy (#45)", async () => {
    const idle = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await idle.flush();
    expect(idle.captureCharFrame()).not.toContain("cancel");
    const busy = await testRender(
      <Composer busy focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await busy.flush();
    expect(busy.captureCharFrame()).toContain("cancel");
  });

  // The hint row is a single truncating line; read it back to assert the ORDER of
  // the promoted hint, and that ^O never appears twice.
  const hintLine = (frame: string) =>
    frame.split("\n").find((l) => l.includes("commands")) ?? "";

  test("leads the hint row with ^O when actionable attention is pending (#154)", async () => {
    const t = await testRender(
      <Composer busy={false} focus attentionPending onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const line = hintLine(t.captureCharFrame());
    expect(line).toContain("inspect ops");
    // ^O is promoted to the front, ahead of the commands hint.
    expect(line.indexOf("inspect ops")).toBeLessThan(line.indexOf("commands"));
    // …and it is emitted exactly once (not duplicated into the trailing slot).
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  test("keeps ^O in its trailing slot when no attention is pending (#154)", async () => {
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const line = hintLine(t.captureCharFrame());
    // Default order: commands first, ^O last.
    expect(line.indexOf("commands")).toBeLessThan(line.indexOf("inspect ops"));
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  test("leads with Esc cancel over ^O when a cancellable turn is in flight (#154)", async () => {
    const t = await testRender(
      <Composer busy cancellable focus attentionPending onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const line = hintLine(t.captureCharFrame());
    // Cancel takes precedence: Esc leads even though attention is pending.
    expect(line).toContain("cancel");
    expect(line.indexOf("cancel")).toBeLessThan(line.indexOf("commands"));
    expect(line.indexOf("cancel")).toBeLessThan(line.indexOf("inspect ops"));
    // ^O is not promoted here, so it stays a single trailing hint.
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  test("does not promote ^O for a non-cancellable busy turn with attention (#154)", async () => {
    // cancellable={false} suppresses the Esc hint, and attention then leads with ^O.
    const t = await testRender(
      <Composer busy cancellable={false} focus attentionPending onSubmit={() => {}} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    const line = hintLine(t.captureCharFrame());
    expect(line).not.toContain("cancel");
    expect(line.indexOf("inspect ops")).toBeLessThan(line.indexOf("commands"));
    expect(line.match(/inspect ops/g)?.length).toBe(1);
  });

  test("Escape on an empty composer while busy aborts the turn (#45)", async () => {
    let cancelled = 0;
    const t = await testRender(
      <Composer busy focus onSubmit={() => {}} onCancel={() => cancelled++} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await escape(t);
    expect(cancelled).toBe(1);
  });

  test("Escape with text in the buffer clears it instead of cancelling (#45)", async () => {
    let cancelled = 0;
    const t = await testRender(
      <Composer busy focus onSubmit={() => {}} onCancel={() => cancelled++} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "half typed");
    await escape(t);
    // The buffer is cleared (cancel-edit gesture), and the turn is NOT aborted.
    expect(cancelled).toBe(0);
    expect(t.captureCharFrame()).not.toContain("half typed");
  });

  test("restore() pushes a pulled-back message back into the buffer (#61)", async () => {
    const ref = createRef<ComposerHandle>();
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} ref={ref} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    // Buffer starts empty (the placeholder shows, not real text).
    expect(t.captureCharFrame()).toContain("supervise");

    await act(async () => {
      ref.current!.restore("pulled back message");
    });
    await t.flush();
    expect(t.captureCharFrame()).toContain("pulled back message");
  });

  test("restored text can be edited and submitted (#61)", async () => {
    let submitted: string | undefined;
    const ref = createRef<ComposerHandle>();
    const t = await testRender(
      <Composer busy={false} focus onSubmit={(v) => (submitted = v)} ref={ref} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await act(async () => {
      ref.current!.restore("edit me");
    });
    await t.flush();
    // The cursor parks at the end after an external replacement, so typing appends.
    await type(t, "!");
    await press(t, "RETURN");
    expect(submitted).toBe("edit me!");
  });

  test("Escape when idle does not invoke onCancel (#45)", async () => {
    let cancelled = 0;
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => {}} onCancel={() => cancelled++} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await escape(t);
    expect(cancelled).toBe(0);
  });

  test("does not record a rejected submit in history", async () => {
    const t = await testRender(
      <Composer busy={false} focus onSubmit={() => false} />,
      COMPOSER_SIZE,
    );
    await t.flush();
    await type(t, "nope");
    await press(t, "RETURN"); // rejected → text kept, NOT recorded
    expect(t.captureCharFrame()).toContain("nope");
    await press(t, "u", { ctrl: true }); // ^U: clear the kept draft
    await press(t, "ARROW_UP"); // nothing to recall — history is empty
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("nope");
    expect(frame).toContain("supervise"); // the empty-draft placeholder
  });
});
