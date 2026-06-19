import { act } from "react";
import { render } from "ink-testing-library";
import {
  ThinkingDot,
  BRAILLE_FRAMES,
  ASCII_FRAMES,
} from "../../src/ui/components/ThinkingDot.js";

// Only the spinner's own interval is faked; Ink's internal scheduling stays real
// so frames still flush to `lastFrame()`. State updates from the fired timer are
// wrapped in `act()` (React 19) so the render lands before we assert.
const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

describe("ThinkingDot", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders the first braille frame before any tick", () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    expect(lastFrame()).toBe(BRAILLE_FRAMES[0]);
    unmount();
  });

  it("advances one braille frame per interval", () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    advance(80);
    expect(lastFrame()).toBe(BRAILLE_FRAMES[1]);
    advance(80);
    expect(lastFrame()).toBe(BRAILLE_FRAMES[2]);
    unmount();
  });

  it("wraps back to the first frame after a full cycle", () => {
    const { lastFrame, unmount } = render(<ThinkingDot />);
    advance(80 * BRAILLE_FRAMES.length);
    expect(lastFrame()).toBe(BRAILLE_FRAMES[0]);
    unmount();
  });

  it("uses the single-column ASCII frame set in ascii mode", () => {
    const { lastFrame, unmount } = render(<ThinkingDot ascii />);
    expect(lastFrame()).toBe(ASCII_FRAMES[0]);
    advance(80);
    expect(lastFrame()).toBe(ASCII_FRAMES[1]);
    unmount();
  });

  it("every frame is exactly one column wide (layout must not reflow)", () => {
    for (const f of [...BRAILLE_FRAMES, ...ASCII_FRAMES]) {
      // String length 1 — a single code point, single display cell in a UTF font.
      expect(f).toHaveLength(1);
    }
  });

  it("clears its interval on unmount (no leaked timer)", () => {
    const { unmount } = render(<ThinkingDot />);
    unmount();
    // With the only interval cleared, no timers remain pending.
    expect(vi.getTimerCount()).toBe(0);
  });
});
