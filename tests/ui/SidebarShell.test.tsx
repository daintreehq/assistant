import { render } from "ink-testing-library";
import { SidebarShell } from "../../src/ui/sidebar/SidebarShell.js";
import { layoutMode } from "../../src/ui/DaintreeInkApp.js";
import type { DaintreeController } from "../../src/ui/hooks/useDaintreeController.js";
import type { DashboardState } from "../../src/ui/types.js";
import type { QueueEvent, WatcherRecord } from "../../src/schemas.js";

const stripAnsi = (s: string) => s.replace(/\[[0-9;]*m/g, "");
const widest = (frame: string) =>
  Math.max(0, ...stripAnsi(frame).split("\n").map((l) => [...l].length));

const app = {
  config: { projectPath: "/Users/x/Projects/assistant", tier: "operator" },
  mcp: { isConnected: () => false, callTool: async () => ({ text: "", structuredContent: {} }) },
} as any;

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_7f2",
    kind: "terminal",
    title: "watch tests",
    goal: "wait for tests",
    targetsJson: JSON.stringify(["term_3a"]),
    cadenceMs: 120000,
    modelTier: "small",
    status: "active",
    lastClassification: "tests_failed",
    nextCheckAt: 0,
    createdAt: 0,
    ...over,
  };
}

function event(over: Partial<QueueEvent>): QueueEvent {
  return {
    id: "evt_1",
    source: "terminal_watcher",
    severity: "attention",
    title: "tests failed",
    summary: "parser.spec.ts failed after agent run",
    createdAt: 1,
    count: 1,
    ...over,
  };
}

function controller(over: Partial<DaintreeController>): DaintreeController {
  const dashboard: DashboardState = {
    mcp: { connected: true } as any,
    watchers: [watcher({})],
    timers: [],
    inbox: [event({})],
    audit: [],
    ...(over.dashboard ?? {}),
  };
  return {
    bridge: {} as any,
    timeline: [],
    busy: false,
    pendingConfirm: null,
    activePanel: null,
    setActivePanel: () => {},
    sendUserMessage: async () => {},
    resolveConfirm: () => {},
    ...over,
    dashboard,
  };
}

describe("layoutMode", () => {
  it("never hides ops below 110 cols — narrow is sidebar, mid is balanced", () => {
    expect(layoutMode(55)).toBe("sidebar");
    expect(layoutMode(36)).toBe("sidebar");
    expect(layoutMode(80)).toBe("balanced");
    expect(layoutMode(120)).toBe("wide");
  });
});

describe("SidebarShell", () => {
  it("renders the cockpit as the primary surface at 55x30", () => {
    const { lastFrame, unmount } = render(
      <SidebarShell app={app} controller={controller({})} columns={55} rows={30} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree");
    expect(frame).toContain("Now");
    expect(frame).toContain("Needs attention");
    expect(frame).toContain("Watchers");
    expect(frame).toContain("tests failed");
    expect(widest(frame)).toBeLessThanOrEqual(55);
    unmount();
  });

  it("prioritizes attention over audit when vertical space is tight (55x18)", () => {
    const dashboard: DashboardState = {
      mcp: { connected: true } as any,
      watchers: [watcher({})],
      timers: [],
      inbox: [event({})],
      audit: [
        { id: "a", ts: 0, actor: "main", toolName: "context.snapshot", argsJson: "{}", outcome: "ok", durationMs: 24, summary: "" },
      ],
    };
    const { lastFrame, unmount } = render(
      <SidebarShell app={app} controller={controller({ dashboard })} columns={55} rows={18} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Needs attention");
    // Compact density drops the audit strip entirely.
    expect(frame).not.toContain("context.snapshot");
    expect(widest(frame)).toBeLessThanOrEqual(55);
    unmount();
  });

  it("stays within the budget and switches to dense rows at 36 cols", () => {
    const { lastFrame, unmount } = render(
      <SidebarShell app={app} controller={controller({})} columns={36} rows={30} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Watchers");
    expect(widest(frame)).toBeLessThanOrEqual(36);
    unmount();
  });

  it("shows the inline confirm card (not a floating modal) when a confirm is pending", () => {
    const pending = {
      id: "c1",
      request: { toolName: "git.commit", risk: "git", summary: "commit staged changes", args: { message: "wip" } },
      resolve: () => {},
    } as any;
    const { lastFrame, unmount } = render(
      <SidebarShell app={app} controller={controller({ pendingConfirm: pending })} columns={55} rows={30} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Confirm action");
    expect(frame).toContain("git.commit");
    expect(frame).toContain("Y approve");
    expect(widest(frame)).toBeLessThanOrEqual(55);
    unmount();
  });

  it("keeps a pending confirm visible even when a focus panel is active", () => {
    const pending = {
      id: "c1",
      request: { toolName: "git.commit", risk: "git", summary: "commit staged changes", args: {} },
      resolve: () => {},
    } as any;
    const { lastFrame, unmount } = render(
      <SidebarShell
        app={app}
        controller={controller({ pendingConfirm: pending, activePanel: "watchers" })}
        columns={55}
        rows={30}
      />,
    );
    const frame = lastFrame() ?? "";
    // Confirm wins the surface; the focus page (with its "Esc home" hint) is hidden.
    expect(frame).toContain("Confirm action");
    expect(frame).not.toContain("Esc home");
    unmount();
  });

  it("renders a focus page with an Esc-home hint when a panel is active", () => {
    const { lastFrame, unmount } = render(
      <SidebarShell app={app} controller={controller({ activePanel: "watchers" })} columns={55} rows={30} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Watchers");
    expect(frame).toContain("Esc home");
    unmount();
  });
});
