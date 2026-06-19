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

  it("names the most urgent attention item but suppresses inert action labels", () => {
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
    expect(frame).toContain("3 failures");
    // The bracketed action labels looked interactive but had no key handler —
    // they must not render until they are actually wired (issue #93).
    expect(frame).not.toContain("[F focus terminal]");
    expect(frame).not.toContain("[R rerun]");
  });

  it("hides empty sections (no audit/timers shown when there are none)", () => {
    const frame =
      render(<OperationsView dashboard={dash()} width={72} now={0} />).lastFrame() ?? "";
    expect(frame).not.toContain("RECENT");
    expect(frame).not.toContain("SCHEDULED");
    expect(frame).toContain("Standing by");
  });

  it("focuses a single section when a panel command set activePanel", () => {
    const frame =
      render(
        <OperationsView
          dashboard={dash({
            watchers: [watcher({})],
            timers: [{ id: "t1", title: "nudge", fireAt: 0, createdAt: 0 } as any],
            audit: [
              { id: "a1", toolName: "git.push", outcome: "ok", durationMs: 5 } as any,
            ],
          })}
          width={72}
          now={0}
          activePanel="timers"
        />,
      ).lastFrame() ?? "";
    // Only the timers panel renders; the other sections are filtered out.
    expect(frame).toContain("SCHEDULED");
    expect(frame).toContain("nudge");
    expect(frame).not.toContain("NOW");
    expect(frame).not.toContain("AGENTS");
    expect(frame).not.toContain("RECENT");
  });

  it("renders the full deck when activePanel is null (unchanged behavior)", () => {
    const frame =
      render(
        <OperationsView
          dashboard={dash({ watchers: [watcher({})] })}
          width={72}
          now={0}
          activePanel={null}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("NOW");
    expect(frame).toContain("AGENTS");
  });

  it("shows an honest placeholder when a focused panel is empty", () => {
    const frame =
      render(
        <OperationsView dashboard={dash()} width={72} now={0} activePanel="timers" />,
      ).lastFrame() ?? "";
    expect(frame).not.toContain("SCHEDULED");
    expect(frame).toContain("Nothing here yet.");
  });
});
