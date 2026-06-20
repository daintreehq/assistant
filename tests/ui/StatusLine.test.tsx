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
  it("renders nothing when idle with no signal to report", () => {
    // Silence already means idle: no "Standing by", no steady-state MCP badge, no
    // tier (that lives in the masthead now). A clean idle line is simply empty.
    const frame = render(<StatusLine dashboard={dash()} />).lastFrame() ?? "";
    expect(frame.trim()).toBe("");
  });

  it("never shows an 'MCP' token while the link is healthy", () => {
    // The startup banner already confirms the connection; the status line only ever
    // speaks about it by exception (DEGRADED). A connected line must stay quiet.
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).toContain("WORKING");
    expect(frame).not.toContain("MCP");
  });

  it("carries no tier token — the tier lives in the masthead", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).not.toMatch(/\bsys\b/i);
    expect(frame).not.toMatch(/\bop\b/i);
    expect(frame).not.toContain("SYSTEM");
  });

  it("prefers the current active agent over inventory counts", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).toContain("WORKING"); // active agent badge
    expect(frame).toContain("term_8"); // the supervised terminal
    expect(frame).toContain("agents 1"); // compact rollup on the right
  });

  it("drops the model id on a narrow active line but keeps context pressure", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash({ watchers: [watcher()] })}
          sessionUsage={usage()}
          width={50}
          now={0}
        />,
      ).lastFrame() ?? "";
    expect(frame).not.toContain("minimax-m3"); // model dropped on a narrow line
    expect(frame).toContain("CTX 42%"); // pressure survives the squeeze
  });

  it("keeps context pressure visible during an active run", () => {
    const frame =
      render(
        <StatusLine
          dashboard={dash({ watchers: [watcher()] })}
          sessionUsage={usage()}
          width={80}
          now={0}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("$0.012");
    expect(frame).not.toContain("minimax-m3");
  });

  it("leaves no orphan separator during a run", () => {
    const frame =
      render(<StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />).lastFrame() ?? "";
    expect(frame).toContain("WORKING");
    expect(frame).not.toMatch(/·\s+·/); // no dangling " ·  · " between segments
  });

  it("shows an attention chip when the inbox is non-empty", () => {
    // The inbox is controller-filtered to actionable severities (>= attention), so
    // its length IS the actionable count — non-actionable debug/info/done never reach
    // the chip.
    const frame =
      render(<StatusLine dashboard={dash({ inbox: [event("error"), event("attention")] })} />)
        .lastFrame() ?? "";
    expect(frame).toContain("!2");
  });

  // Truecolor SGRs for the relevant tones (ink-testing-library forces color).
  const DANGER = "38;2;251;113;133"; // error → danger (#FB7185)
  const WARNING = "38;2;246;200;95"; // attention → warning (#F6C85F)
  const BLOCKED = "38;2;196;181;253"; // blocked/urgent → blocked (#C4B5FD)

  it("colors the chip by the MOST urgent item, not the inbox head (#154)", () => {
    // Pass the worst event LAST to prove the color comes from topSeverity(), not an
    // implicit inbox[0] ordering.
    const frame =
      render(<StatusLine dashboard={dash({ inbox: [event("attention"), event("error")] })} />)
        .lastFrame() ?? "";
    expect(frame).toContain("!2");
    expect(frame).toContain(DANGER); // worst item (error) drives the color
    expect(frame).not.toContain(WARNING); // not the head item's (attention) tone
  });

  it("ranks error above blocked for the chip color, matching DB severity order (#154)", () => {
    // SEVERITY_RANK mirrors the DB's canonical order, so a mixed inbox colors the
    // chip by `error` (danger/red), not `blocked` (purple) — regardless of position.
    const frame =
      render(<StatusLine dashboard={dash({ inbox: [event("blocked"), event("error")] })} />)
        .lastFrame() ?? "";
    expect(frame).toContain("!2");
    expect(frame).toContain(DANGER); // error wins
    expect(frame).not.toContain(BLOCKED); // not the blocked tone
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
