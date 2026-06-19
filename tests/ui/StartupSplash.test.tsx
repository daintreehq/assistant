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

  it("skips the mark on a terminal too narrow to hold it (no clipped logo)", () => {
    const { lastFrame } = render(
      <StartupSplash columns={SPLASH_WIDTH} rows={24} />,
    );
    expect((lastFrame() ?? "").trim()).toBe("");
  });
});
