import { render } from "ink-testing-library";
import { StatusLine } from "../../src/ui/components/StatusLine.js";
import type { DashboardState } from "../../src/ui/types.js";
import type { WatcherRecord } from "../../src/schemas.js";

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

function watcher(over: Partial<WatcherRecord> = {}): WatcherRecord {
  return {
    id: "wch_1",
    kind: "terminal",
    title: "watch tests",
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

const event = (severity: string) => ({ id: severity, severity }) as any;

describe("StatusLine", () => {
  it("shows the scrollback affordance, tier, and MCP health", () => {
    const frame = render(<StatusLine dashboard={dash()} tier="operator" />).lastFrame() ?? "";
    // Scrollback is the terminal's now, so the footer just points at it.
    expect(frame).toContain("scroll for history");
    expect(frame).toContain("OPERATOR");
    expect(frame).toContain("MCP");
  });

  it("keeps work detail out of the footer and shows only inventory counts", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).not.toContain("WORKING"); // work detail belongs in the ledger
    expect(frame).not.toContain("term_8");
    expect(frame).toContain("agents 1"); // compact rollup on the right
  });

  it("shows an attention chip when the inbox is non-empty", () => {
    const frame =
      render(<StatusLine dashboard={dash({ inbox: [event("error"), event("attention")] })} />)
        .lastFrame() ?? "";
    expect(frame).toContain("!2");
  });

  it("flags a degraded MCP connection", () => {
    const frame =
      render(<StatusLine dashboard={dash({ mcp: { connected: false } as any })} />).lastFrame() ??
      "";
    expect(frame).toContain("DEGRADED");
  });
});
