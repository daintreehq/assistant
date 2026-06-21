import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import {
  ThinkingDot,
  BRAILLE_FRAMES,
  ASCII_FRAMES,
} from "../../src/ui/components/ThinkingDot.js";

// Real timers, matching the Ink-era convention (no fake timers anywhere — the
// spinner is a plain 80ms `setInterval`). We sleep past one interval to let the
// out-of-band `setInterval` fire its React state update, then `flush()` to commit
// + repaint before reading the frame. We assert the spinner moved to a *valid,
// different* frame rather than pinning an exact index, so ordinary timer jitter
// can't make it flaky.
const PAST_ONE_INTERVAL = () => new Promise((r) => setTimeout(r, 120));

describe("ThinkingDot", () => {
  test("renders the first braille frame before any tick", async () => {
    const t = await testRender(<ThinkingDot />, { width: 8, height: 1 });
    await t.flush();
    expect(t.captureCharFrame().trim()).toBe(BRAILLE_FRAMES[0]);
  });

  test("advances to another braille frame as time passes", async () => {
    const t = await testRender(<ThinkingDot />, { width: 8, height: 1 });
    await t.flush();
    await PAST_ONE_INTERVAL();
    await t.flush();
    const frame = t.captureCharFrame().trim();
    expect(frame).not.toBe(BRAILLE_FRAMES[0]); // it moved
    expect(BRAILLE_FRAMES).toContain(frame as (typeof BRAILLE_FRAMES)[number]);
  });

  test("uses the single-column ASCII frame set in ascii mode", async () => {
    const t = await testRender(<ThinkingDot ascii />, { width: 8, height: 1 });
    await t.flush();
    expect(t.captureCharFrame().trim()).toBe(ASCII_FRAMES[0]);
    await PAST_ONE_INTERVAL();
    await t.flush();
    const frame = t.captureCharFrame().trim();
    expect(frame).not.toBe(ASCII_FRAMES[0]); // it moved
    expect(ASCII_FRAMES).toContain(frame as (typeof ASCII_FRAMES)[number]);
  });

  test("keeps every frame exactly one column wide (layout must not reflow)", () => {
    for (const f of [...BRAILLE_FRAMES, ...ASCII_FRAMES]) {
      // A single code point — one display cell in a UTF font — so the
      // `{glyph} {stage}` line never reflows between Unicode and ASCII modes.
      expect(f).toHaveLength(1);
    }
  });
});
