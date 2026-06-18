import { render } from "ink-testing-library";
import { OperationsView } from "../../src/ui/components/OperationsView.js";
import type { DashboardState } from "../../src/ui/types.js";
import type { WatcherRecord } from "../../src/schemas.js";

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_1",
    kind: "terminal",
    title: "repair tests",
    goal: "wait for tests",
    targetsJson: JSON.stringify(["term_8"]),
    cadenceMs: 1000,
    modelTier: "small",
    status: "active",
    lastClassification: "still_working",
    nextCheckAt: 0,
    createdAt: 0,
    ...over,
  };
}

function dash(over: Partial<DashboardState> = {}): DashboardState {
  return {
    mcp: { connected: true } as any,
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
    ...over,
  };
}

describe("OperationsView", () => {
  it("orders sections by human priority and merges watchers into agents", () => {
    const frame =
      render(
        <OperationsView dashboard={dash({ watchers: [watcher({})] })} width={72} now={0} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("NOW");
    expect(frame).toContain("AGENTS");
    expect(frame).toContain("term_8"); // the supervised terminal, not a separate concept
  });

  it("names the most urgent attention item and its recommended actions", () => {
    const frame =
      render(
        <OperationsView
          dashboard={dash({
            inbox: [
              {
                id: "e1",
                source: "terminal_watcher",
                severity: "error",
                title: "Tests failed in term_8",
                summary: "3 failures",
                createdAt: 0,
                count: 1,
                recommendedActions: [
                  { label: "focus terminal", toolName: "terminal.focus" },
                  { label: "rerun", toolName: "recipe.run" },
                ],
              } as any,
            ],
          })}
          width={72}
          now={0}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("NEEDS ATTENTION");
    expect(frame).toContain("Tests failed in term_8");
    expect(frame).toContain("[F focus terminal]");
    expect(frame).toContain("[R rerun]");
  });

  it("hides empty sections (no audit/timers shown when there are none)", () => {
    const frame =
      render(<OperationsView dashboard={dash()} width={72} now={0} />).lastFrame() ?? "";
    expect(frame).not.toContain("RECENT");
    expect(frame).not.toContain("SCHEDULED");
    expect(frame).toContain("Standing by");
  });
});
