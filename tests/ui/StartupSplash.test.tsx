import { render } from "ink-testing-library";
import { StartupSplash } from "../../src/ui/components/StartupSplash.js";
import {
  SPLASH_FRAMES,
  SPLASH_HEIGHT,
  SPLASH_WIDTH,
} from "../../src/ui/splash/frames.js";

describe("splash frames", () => {
  it("are a non-empty, rectangular sequence", () => {
    expect(SPLASH_FRAMES.length).toBeGreaterThan(1);
    for (const frame of SPLASH_FRAMES) {
      const lines = frame.split("\n");
      expect(lines).toHaveLength(SPLASH_HEIGHT);
      for (const line of lines) expect(line).toHaveLength(SPLASH_WIDTH);
    }
  });

  it("draws in: the last frame has more ink than the first", () => {
    const ink = (f: string) => f.replace(/[ \n]/g, "").length;
    expect(ink(SPLASH_FRAMES.at(-1)!)).toBeGreaterThan(ink(SPLASH_FRAMES[0]));
  });
});

describe("StartupSplash", () => {
  it("renders the mark at natural size, a couple of blank lines down, not screen-filling", () => {
    const { lastFrame } = render(
      <StartupSplash columns={60} rows={24} />,
    );
    const lines = (lastFrame() ?? "").split("\n");
    // Breathing room above (no vertical centering): the first line is blank.
    expect(lines[0].trim()).toBe("");
    // The full mark is drawn...
    expect(lines.length).toBeGreaterThanOrEqual(SPLASH_HEIGHT);
    // ...but it is NOT inflated to fill the 24-row viewport — natural height + a
    // small top margin, well under the screen.
    expect(lines.length).toBeLessThan(24);
  });

  // Smallest left-pad across the drawn rows. The mark is horizontally centered, so
  // this pad is `floor((track - SPLASH_WIDTH) / 2)` plus a constant intrinsic offset
  // — left-alignment would make it constant regardless of width. Reading the minimum
  // is robust to color stripping and to sparse early frames (it just tracks the
  // least-indented rendered row, which shifts by exactly the centering pad).
  const minLeftPad = (columns: number): number => {
    const lines = (render(<StartupSplash columns={columns} rows={40} />).lastFrame() ?? "")
      .split("\n")
      .filter((l) => l.trim() !== "");
    return Math.min(...lines.map((l) => l.length - l.trimStart().length));
  };

  it("centers the mark horizontally — the inset grows with the terminal width", () => {
    // Left-alignment would hold the inset constant; centering widens it as the
    // terminal does. Proves the mark tracks the middle, not the left edge.
    expect(minLeftPad(80)).toBeGreaterThan(minLeftPad(60));
  });

  it("skips the mark on a terminal too narrow to hold it, but still unblocks boot", () => {
    // A clipped logo looks broken, so the mark is dropped — yet onComplete MUST
    // still fire, because that callback is the controller's draw-done gate; not
    // firing it would hang boot forever on a narrow pane.
    const onComplete = vi.fn();
    const { lastFrame } = render(
      <StartupSplash columns={SPLASH_WIDTH} rows={24} onComplete={onComplete} />,
    );
    expect((lastFrame() ?? "").trim()).toBe("");
    expect(onComplete).toHaveBeenCalledTimes(1);
  });
});
