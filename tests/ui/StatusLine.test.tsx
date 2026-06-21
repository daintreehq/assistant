import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
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

// captureCharFrame() is already plain text (no ANSI), so the old strip-ansi step is
// gone. Render at a generous width/height so the single status row never wraps.
async function frameOf(node: Parameters<typeof testRender>[0]): Promise<string> {
  const t = await testRender(node, { width: 80, height: 6 });
  await t.flush();
  return t.captureCharFrame();
}

describe("StatusLine", () => {
  test("renders nothing when idle with no signal to report", async () => {
    // Silence already means idle: no "Standing by", no steady-state MCP badge, no
    // tier (that lives in the masthead now). A clean idle line is simply empty.
    const frame = await frameOf(<StatusLine dashboard={dash()} />);
    expect(frame.trim()).toBe("");
  });

  test("never shows an 'MCP' token while the link is healthy", async () => {
    // The startup banner already confirms the connection; the status line only ever
    // speaks about it by exception (DEGRADED). A connected line must stay quiet.
    const frame = await frameOf(
      <StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />,
    );
    expect(frame).toContain("WORKING");
    expect(frame).not.toContain("MCP");
  });

  test("carries no tier token — the tier lives in the masthead", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />,
    );
    expect(frame).not.toMatch(/\bsys\b/i);
    expect(frame).not.toMatch(/\bop\b/i);
    expect(frame).not.toContain("SYSTEM");
  });

  test("prefers the current active agent over inventory counts", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />,
    );
    expect(frame).toContain("WORKING"); // active agent badge
    expect(frame).toContain("term_8"); // the supervised terminal
    expect(frame).toContain("agents 1"); // compact rollup on the right
  });

  test("drops the model id on a narrow active line but keeps context pressure", async () => {
    const t = await testRender(
      <StatusLine
        dashboard={dash({ watchers: [watcher()] })}
        sessionUsage={usage()}
        width={50}
        now={0}
      />,
      { width: 80, height: 6 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).not.toContain("minimax-m3"); // model dropped on a narrow line
    expect(frame).toContain("CTX 42%"); // pressure survives the squeeze
  });

  test("keeps context pressure visible during an active run", async () => {
    const frame = await frameOf(
      <StatusLine
        dashboard={dash({ watchers: [watcher()] })}
        sessionUsage={usage()}
        width={80}
        now={0}
      />,
    );
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("$0.012");
    expect(frame).not.toContain("minimax-m3");
  });

  test("leaves no orphan separator during a run", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash({ watchers: [watcher()] })} now={0} />,
    );
    expect(frame).toContain("WORKING");
    expect(frame).not.toMatch(/·\s+·/); // no dangling " ·  · " between segments
  });

  test("shows an attention chip when the inbox is non-empty", async () => {
    // The inbox is controller-filtered to actionable severities (>= attention), so
    // its length IS the actionable count — non-actionable debug/info/done never reach
    // the chip.
    const frame = await frameOf(
      <StatusLine dashboard={dash({ inbox: [event("error"), event("attention")] })} />,
    );
    expect(frame).toContain("!2");
  });

  // The chip COLOR is no longer carried by SGR escapes (captureCharFrame is plain
  // text). OpenTUI exposes per-span color via captureSpans(); find the span carrying
  // the chip text and assert its foreground RGB. This replaces the old chalk-level
  // pin + SGR-substring assertions with a direct color read.
  describe("attention chip color", () => {
    const DANGER: [number, number, number] = [251, 113, 133]; // error → danger (#FB7185)
    const WARNING: [number, number, number] = [246, 200, 95]; // attention → warning (#F6C85F)
    const BLOCKED: [number, number, number] = [196, 181, 253]; // blocked/urgent → blocked (#C4B5FD)

    // Collect the fg RGB triples of every span whose text contains the chip token.
    async function chipColors(
      node: Parameters<typeof testRender>[0],
    ): Promise<Array<[number, number, number]>> {
      const t = await testRender(node, { width: 80, height: 6 });
      await t.flush();
      const out: Array<[number, number, number]> = [];
      for (const line of t.captureSpans().lines) {
        for (const span of line.spans) {
          if (span.text.includes("!2")) {
            const [r, g, b] = span.fg.toInts();
            out.push([r, g, b]);
          }
        }
      }
      return out;
    }

    const has = (
      colors: Array<[number, number, number]>,
      target: [number, number, number],
    ) => colors.some((c) => c[0] === target[0] && c[1] === target[1] && c[2] === target[2]);

    test("colors the chip by the MOST urgent item, not the inbox head (#154)", async () => {
      // Pass the worst event LAST to prove the color comes from topSeverity(), not an
      // implicit inbox[0] ordering.
      const colors = await chipColors(
        <StatusLine dashboard={dash({ inbox: [event("attention"), event("error")] })} />,
      );
      expect(colors.length).toBeGreaterThan(0);
      expect(has(colors, DANGER)).toBe(true); // worst item (error) drives the color
      expect(has(colors, WARNING)).toBe(false); // not the head item's (attention) tone
    });

    test("ranks error above blocked for the chip color, matching DB severity order (#154)", async () => {
      // SEVERITY_RANK mirrors the DB's canonical order, so a mixed inbox colors the
      // chip by `error` (danger/red), not `blocked` (purple) — regardless of position.
      const colors = await chipColors(
        <StatusLine dashboard={dash({ inbox: [event("blocked"), event("error")] })} />,
      );
      expect(colors.length).toBeGreaterThan(0);
      expect(has(colors, DANGER)).toBe(true); // error wins
      expect(has(colors, BLOCKED)).toBe(false); // not the blocked tone
    });
  });

  test("flags a degraded MCP connection", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash({ mcp: { connected: false } as any })} />,
    );
    expect(frame).toContain("DEGRADED");
  });

  test("surfaces context pressure, session cost, and active model", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash()} sessionUsage={usage()} width={80} />,
    );
    expect(frame).toContain("CTX 42%");
    expect(frame).toContain("$0.012");
    expect(frame).toContain("minimax-m3");
  });

  test("rounds context pressure toward the auto-compact threshold", async () => {
    const frame = await frameOf(
      <StatusLine
        dashboard={dash()}
        sessionUsage={usage({ contextTokens: 48_000 })}
        width={80}
      />,
    );
    expect(frame).toContain("CTX 80%");
  });

  test("omits the context gauge until a usage reading arrives", async () => {
    const frame = await frameOf(
      <StatusLine
        dashboard={dash()}
        sessionUsage={usage({ contextThreshold: 0, contextTokens: 0 })}
        width={80}
      />,
    );
    expect(frame).not.toContain("CTX");
  });

  test("hides the model id on a narrow status line", async () => {
    const frame = await frameOf(
      <StatusLine dashboard={dash()} sessionUsage={usage()} width={50} />,
    );
    // Pressure still shows, but the long model id is dropped to protect the left side.
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("minimax-m3");
  });

  test("renders nothing extra when no session usage is provided", async () => {
    const frame = await frameOf(<StatusLine dashboard={dash()} />);
    expect(frame).not.toContain("CTX");
    expect(frame).not.toContain("$");
  });

  test("shows context pressure but hides cost when cost is unknown", async () => {
    const frame = await frameOf(
      <StatusLine
        dashboard={dash()}
        sessionUsage={usage({ costUsd: undefined })}
        width={80}
      />,
    );
    expect(frame).toContain("CTX 42%");
    expect(frame).not.toContain("$");
  });
});
