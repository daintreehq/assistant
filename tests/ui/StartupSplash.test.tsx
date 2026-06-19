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
  it("renders centered within the given viewport without throwing", () => {
    const { lastFrame } = render(
      <StartupSplash columns={60} rows={24} />,
    );
    const frame = lastFrame() ?? "";
    // The mark is centered in a 24-row viewport, so it sits below the top edge.
    expect(frame.split("\n").length).toBeGreaterThanOrEqual(SPLASH_HEIGHT);
  });
});
