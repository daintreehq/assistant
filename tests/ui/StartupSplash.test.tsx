import { test, expect, describe, mock } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { StartupSplash } from "../../src/ui/components/StartupSplash.js";
import {
  SPLASH_FRAMES,
  SPLASH_HEIGHT,
  SPLASH_WIDTH,
} from "../../src/ui/splash/frames.js";

describe("splash frames", () => {
  test("are a non-empty, rectangular sequence", () => {
    expect(SPLASH_FRAMES.length).toBeGreaterThan(1);
    for (const frame of SPLASH_FRAMES) {
      const lines = frame.split("\n");
      expect(lines).toHaveLength(SPLASH_HEIGHT);
      for (const line of lines) expect(line).toHaveLength(SPLASH_WIDTH);
    }
  });

  test("draws in: the last frame has more ink than the first", () => {
    const ink = (f: string) => f.replace(/[ \n]/g, "").length;
    expect(ink(SPLASH_FRAMES.at(-1)!)).toBeGreaterThan(ink(SPLASH_FRAMES[0]));
  });
});

describe("StartupSplash", () => {
  test("renders the mark at natural size, a couple of blank lines down, not screen-filling", async () => {
    const t = await testRender(<StartupSplash columns={60} rows={24} />, {
      width: 60,
      height: 24,
    });
    await t.flush();
    const lines = t.captureCharFrame().split("\n");
    // Breathing room above (no vertical centering): the first line is blank.
    expect(lines[0].trim()).toBe("");
    // The full mark is drawn...
    expect(lines.length).toBeGreaterThanOrEqual(SPLASH_HEIGHT);
    // ...but it is NOT inflated to fill the 24-row viewport — natural height + a
    // small top margin, well under the screen. (The captured frame spans the whole
    // viewport, so we look at where the drawn content ends rather than line count:
    // the last non-blank row of the mark sits well above the bottom of the screen.)
    const lastInkRow = lines.reduce(
      (acc, l, i) => (l.trim() !== "" ? i : acc),
      0,
    );
    expect(lastInkRow).toBeLessThan(24);
  });

  // Smallest left-pad across the drawn rows. The mark is horizontally centered, so
  // this pad is `floor((track - SPLASH_WIDTH) / 2)` plus a constant intrinsic offset
  // — left-alignment would make it constant regardless of width. Reading the minimum
  // is robust to color stripping and to sparse early frames (it just tracks the
  // least-indented rendered row, which shifts by exactly the centering pad).
  const minLeftPad = async (columns: number): Promise<number> => {
    const t = await testRender(<StartupSplash columns={columns} rows={40} />, {
      width: columns,
      height: 40,
    });
    await t.flush();
    const lines = t
      .captureCharFrame()
      .split("\n")
      .filter((l) => l.trim() !== "");
    return Math.min(...lines.map((l) => l.length - l.trimStart().length));
  };

  test("centers the mark horizontally — the inset grows with the terminal width", async () => {
    // Left-alignment would hold the inset constant; centering widens it as the
    // terminal does. Proves the mark tracks the middle, not the left edge.
    expect(await minLeftPad(80)).toBeGreaterThan(await minLeftPad(60));
  });

  test("skips the mark on a terminal too narrow to hold it, but still unblocks boot", async () => {
    // A clipped logo looks broken, so the mark is dropped — yet onComplete MUST
    // still fire, because that callback is the controller's draw-done gate; not
    // firing it would hang boot forever on a narrow pane.
    const onComplete = mock();
    const t = await testRender(
      <StartupSplash columns={SPLASH_WIDTH} rows={24} onComplete={onComplete} />,
      { width: SPLASH_WIDTH, height: 24 },
    );
    await t.flush();
    expect(t.captureCharFrame().trim()).toBe("");
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  test("fires onComplete exactly once after the draw finishes and the linger elapses", async () => {
    // Drive the real frame-step timers (setTimeout(1000/fps) per frame + a final
    // `lingerMs` hold) with a fast fps and short linger so the whole draw completes
    // quickly, then assert the draw-done gate fired precisely once — never twice,
    // even as the held final frame sits on screen. (Real timers, like the rest of the
    // animation suite: the splash self-advances off setTimeout, not the render loop,
    // so we wait wall-clock rather than pumping frames.)
    const onComplete = mock();
    const fps = 120; // ~8.3ms/frame
    const lingerMs = 20;
    await testRender(
      <StartupSplash
        columns={60}
        rows={24}
        fps={fps}
        lingerMs={lingerMs}
        onComplete={onComplete}
      />,
      { width: 60, height: 24 },
    );
    // Past the whole draw (frames * 1000/fps) + linger, with generous slack so timer
    // jitter can't make it flaky.
    const drawMs = (SPLASH_FRAMES.length * 1000) / fps + lingerMs;
    await new Promise((r) => setTimeout(r, drawMs + 200));
    expect(onComplete).toHaveBeenCalledTimes(1);
    // Let more time pass past the linger to prove it is never re-fired.
    await new Promise((r) => setTimeout(r, 100));
    expect(onComplete).toHaveBeenCalledTimes(1);
  });
});
