import { render } from "ink-testing-library";
import { WatcherPanel } from "../../src/ui/components/WatcherPanel.js";
import type { WatcherRecord } from "../../src/schemas.js";

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_123",
    kind: "terminal",
    title: "watch tests",
    goal: "wait for tests",
    targetsJson: JSON.stringify(["term_1"]),
    cadenceMs: 120000,
    modelTier: "small",
    status: "active",
    lastClassification: "still_working",
    nextCheckAt: 0,
    createdAt: 0,
    ...over,
  };
}

describe("WatcherPanel", () => {
  it("renders watcher id and a status badge", () => {
    const { lastFrame } = render(
      <WatcherPanel height={6} watchers={[watcher({})]} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("wch_123");
    expect(frame).toContain("working"); // badge for still_working
  });

  it("maps a failure classification to a 'failed' badge", () => {
    const { lastFrame } = render(
      <WatcherPanel
        height={6}
        watchers={[watcher({ lastClassification: "tests_failed" })]}
      />,
    );
    expect(lastFrame() ?? "").toContain("failed");
  });

  it("shows 'none' when there are no watchers", () => {
    const { lastFrame } = render(<WatcherPanel height={6} watchers={[]} />);
    expect(lastFrame() ?? "").toContain("none");
  });
});
