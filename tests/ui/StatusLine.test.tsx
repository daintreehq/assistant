import { render } from "ink-testing-library";
import { StatusLine } from "../../src/ui/components/StatusLine.js";
import type { DashboardState, SessionUsage } from "../../src/ui/types.js";
import type { WatcherRecord } from "../../src/schemas.js";

function usage(over: Partial<SessionUsage> = {}): SessionUsage {
  return {
    promptTokens: 1000,
    completionTokens: 200,
    totalTokens: 1200,
    costUsd: 0.012,
    contextTokens: 25_200, // 42% of 60k
    contextThreshold: 60_000,
    lastTier: "large",
    lastModel: "minimax-m3",
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
  it("stands by when nothing is active, surfacing the tier", () => {
    const frame = render(<StatusLine dashboard={dash()} tier="operator" />).lastFrame() ?? "";
    expect(frame).toContain("Standing by");
    expect(frame).toContain("OPERATOR");
    expect(frame).toContain("MCP");
  });

  it("prefers the current active agent over inventory counts", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).toContain("WORKING"); // active agent badge
    expect(frame).toContain("term_8"); // the supervised terminal
    expect(frame).toContain("agents 1"); // compact rollup on the right
  });

  it("keeps the tier visible during an active run", () => {
    const frame =
      render(
        <StatusLine dashboard={dash({ watchers: [watcher()] })} tier="system" now={0} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("WORKING"); // active agent occupies the left side
    expect(frame).toContain("sys"); // tier badge persists on the right
    expect(frame).toContain("MCP"); // right-side rollup still intact
  });

  it("uses the short tier label for operator and supervisor during a run", () => {
    const op =
      render(
        <StatusLine dashboard={dash({ watchers: [watcher()] })} tier="operator" now={0} />,
      ).lastFrame() ?? "";
    expect(op).toContain("op");
    expect(op).not.toContain("OPERATOR"); // short form on the right, not the idle label
    const sup =
      render(
        <StatusLine dashboard={dash({ watchers: [watcher()] })} tier="supervisor" now={0} />,
      ).lastFrame() ?? "";
    expect(sup).toContain("sup");
  });

  it("keeps the tier visible even when the model id is suppressed on a narrow line", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash({ watchers: [watcher()] })}
          sessionUsage={usage()}
          tier="operator"
          width={50}
          now={0}
        />,
      ).lastFrame() ?? "";
    expect(frame).not.toContain("minimax-m3"); // model dropped on a narrow line
    expect(frame).toContain("op"); // tier survives the squeeze
    expect(frame).toContain("MCP");
  });

  it("leaves no orphan separator when no tier is provided during a run", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).toContain("MCP");
    expect(frame).not.toMatch(/·\s+·/); // no dangling " ·  · " from an empty tier badge
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

  it("surfaces context pressure, session cost, and active model", () => {
    const frame =
      render(
        <StatusLine dashboard={dash()} sessionUsage={usage()} width={80} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("CTX 42%");
    expect(frame).toContain("$0.012");
    expect(frame).toContain("minimax-m3");
  });

  it("rounds context pressure toward the auto-compact threshold", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash()}
          sessionUsage={usage({ contextTokens: 48_000 })}
          width={80}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("CTX 80%");
  });

  it("omits the context gauge until a usage reading arrives", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash()}
          sessionUsage={usage({ contextThreshold: 0, contextTokens: 0 })}
          width={80}
        />,
      ).lastFrame() ?? "";
    expect(frame).not.toContain("CTX");
  });

  it("hides the model id on a narrow status line", () => {
    const frame =
      render(
        <StatusLine dashboard={dash()} sessionUsage={usage()} width={50} />,
      ).lastFrame() ?? "";
    // Pressure still shows, but the long model id is dropped to protect the left side.
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("minimax-m3");
  });

  it("renders nothing extra when no session usage is provided", () => {
    const frame = render(<StatusLine dashboard={dash()} />).lastFrame() ?? "";
    expect(frame).not.toContain("CTX");
    expect(frame).not.toContain("$");
  });

  it("shows context pressure but hides cost when cost is unknown", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash()}
          sessionUsage={usage({ costUsd: undefined })}
          width={80}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("$");
  });
});
