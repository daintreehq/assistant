import { render } from "ink-testing-library";
import {
  ThinkingDot,
  BRAILLE_FRAMES,
  ASCII_FRAMES,
} from "../../src/ui/components/ThinkingDot.js";

// Real timers + a wait past one 80ms interval, matching this suite's convention
// (no fake timers / `act()` anywhere — those leak React act-environment state into
// the stdin-driven Composer tests that share the run). We assert the spinner moved
// to a *valid, different* frame rather than pinning an exact index, so ordinary
// timer jitter can't make it flaky.
const PAST_ONE_INTERVAL = () => new Promise((r) => setTimeout(r, 120));

describe("ThinkingDot", () => {
  it("renders the first braille frame before any tick", () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    expect(lastFrame()).toBe(BRAILLE_FRAMES[0]);
    unmount();
  });

  it("advances to another braille frame as time passes", async () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    await PAST_ONE_INTERVAL();
    const frame = lastFrame() ?? "";
    expect(frame).not.toBe(BRAILLE_FRAMES[0]); // it moved
    expect(BRAILLE_FRAMES).toContain(frame as (typeof BRAILLE_FRAMES)[number]);
    unmount();
  });

  it("uses the single-column ASCII frame set in ascii mode", async () => {
    const { lastFrame, unmount } = render(<ThinkingDot ascii />);
    expect(lastFrame()).toBe(ASCII_FRAMES[0]);
    await PAST_ONE_INTERVAL();
    const frame = lastFrame() ?? "";
    expect(frame).not.toBe(ASCII_FRAMES[0]); // it moved
    expect(ASCII_FRAMES).toContain(frame as (typeof ASCII_FRAMES)[number]);
    unmount();
  });

  it("keeps every frame exactly one column wide (layout must not reflow)", () => {
    for (const f of [...BRAILLE_FRAMES, ...ASCII_FRAMES]) {
      // A single code point — one display cell in a UTF font — so the
      // `{glyph} {stage}` line never reflows between Unicode and ASCII modes.
      expect(f).toHaveLength(1);
    }
  });

  it("stops cleanly on unmount (no further frames after teardown)", async () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    unmount();
    const after = lastFrame();
    // The interval is cleared on unmount; waiting past an interval must not advance
    // (or throw via a setState on an unmounted component).
    await PAST_ONE_INTERVAL();
    expect(lastFrame()).toBe(after);
  });
});
