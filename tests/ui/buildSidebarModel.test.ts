import {
  buildSidebarModel,
  cleanTerminalLine,
  densityFor,
  extractMeaningfulTerminalLine,
  formatAge,
  type BuildSidebarOptions,
} from "../../src/ui/sidebar/model.js";
import type { DashboardState } from "../../src/ui/types.js";
import type { TerminalPreview } from "../../src/ui/hooks/useTerminalPreview.js";
import type {
  AuditRecord,
  QueueEvent,
  TimerRecord,
  WatcherRecord,
} from "../../src/schemas.js";

const NOW = 1_000_000;

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_1",
    kind: "terminal",
    title: "watch tests",
    goal: "wait for tests",
    targetsJson: JSON.stringify(["term_1"]),
    cadenceMs: 120000,
    modelTier: "small",
    status: "active",
    nextCheckAt: 0,
    createdAt: NOW - 60_000,
    ...over,
  };
}

function event(over: Partial<QueueEvent>): QueueEvent {
  return {
    id: "evt_1",
    source: "terminal_watcher",
    severity: "attention",
    title: "something happened",
    summary: "a summary",
    createdAt: NOW,
    count: 1,
    ...over,
  };
}

function dash(over: Partial<DashboardState>): DashboardState {
  return {
    mcp: { connected: true } as any,
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
    ...over,
  };
}

function opts(over: Partial<BuildSidebarOptions> = {}): BuildSidebarOptions {
  return {
    columns: 55,
    rows: 30,
    now: NOW,
    project: "assistant",
    tier: "operator",
    busy: false,
    pendingConfirm: null,
    ...over,
  };
}

describe("formatAge", () => {
  it("formats recent / minutes / hours", () => {
    expect(formatAge(0)).toBe("now");
    expect(formatAge(1_000)).toBe("now");
    expect(formatAge(102_000)).toBe("01:42");
    expect(formatAge(4 * 3600_000)).toBe("4h");
    expect(formatAge(-5)).toBe("—");
  });
});

describe("densityFor", () => {
  it("picks dense below 44 cols, compact when short, else comfortable", () => {
    expect(densityFor(36, 30)).toBe("dense");
    expect(densityFor(55, 18)).toBe("compact");
    expect(densityFor(55, 30)).toBe("comfortable");
  });
});

describe("terminal line extraction", () => {
  it("strips ANSI + spinner residue and keeps the last meaningful line", () => {
    expect(cleanTerminalLine("[32mok[0m ⠋")).toBe("ok");
    const tail = "[2J\n   \nbuilding...\n⠙\nTests: 3 failed\n";
    expect(extractMeaningfulTerminalLine(tail)).toBe("Tests: 3 failed");
  });

  it("preserves bracketed text that is not an ANSI escape sequence", () => {
    // ANSI_RE is ESC-anchored, so plain brackets must survive untouched.
    expect(cleanTerminalLine("[error] build failed")).toBe("[error] build failed");
    expect(cleanTerminalLine("[2/5] compiling")).toBe("[2/5] compiling");
  });
});

describe("buildSidebarModel", () => {
  it("derives a live header capsule with short tier + counts", () => {
    const model = buildSidebarModel(
      dash({ watchers: [watcher({})], inbox: [event({})] }),
      [],
      [],
      opts(),
    );
    expect(model.status).toMatchObject({
      live: true,
      liveLabel: "live",
      tier: "op",
      watcherCount: 1,
      attentionCount: 1,
    });
  });

  it("flags degraded when MCP is down", () => {
    const model = buildSidebarModel(dash({ mcp: { connected: false } as any }), [], [], opts());
    expect(model.status.live).toBe(false);
    expect(model.status.liveLabel).toBe("degraded");
  });

  it("orders attention by severity then recency", () => {
    const inbox = [
      event({ id: "info", severity: "info", createdAt: NOW }),
      event({ id: "blocked", severity: "blocked", createdAt: NOW - 5 }),
      event({ id: "attn", severity: "attention", createdAt: NOW }),
    ];
    const model = buildSidebarModel(dash({ inbox }), [], [], opts());
    expect(model.attention.map((a) => a.id)).toEqual(["blocked", "attn", "info"]);
  });

  it("renders recommended actions as short verbs and a related line", () => {
    const inbox = [
      event({
        recommendedActions: [
          { label: "Open", toolName: "x" },
          { label: "Summarize", toolName: "y" },
        ],
        target: { terminalId: "term_3a" },
        evidence: ["parser.spec.ts failed"],
      }),
    ];
    const model = buildSidebarModel(dash({ inbox }), [], [], opts());
    expect(model.attention[0].actions).toBe("open · summarize");
    expect(model.attention[0].related).toBe("terminal term_3a");
    expect(model.attention[0].evidence).toBe("parser.spec.ts failed");
  });

  it("sorts watchers: needs-input before working before done", () => {
    const watchers = [
      watcher({ id: "done", lastClassification: "completed_success" }),
      watcher({ id: "working", lastClassification: "still_working" }),
      watcher({ id: "needs", lastClassification: "waiting_for_input" }),
    ];
    const model = buildSidebarModel(dash({ watchers }), [], [], opts());
    expect(model.watchers.map((w) => w.id)).toEqual(["needs", "working", "done"]);
    expect(model.watchers[0].symbol).toBe("!");
    expect(model.watchers[1].symbol).toBe("◌");
    expect(model.watchers[2].symbol).toBe("✓");
  });

  it("truncates long titles to the column budget", () => {
    const long = "x".repeat(200);
    const model = buildSidebarModel(
      dash({ watchers: [watcher({ title: long })] }),
      [],
      [],
      opts({ columns: 55 }),
    );
    // budget(55)=51, watcher reserves 21 → title <= 30 chars incl. ellipsis.
    expect(model.watchers[0].title.length).toBeLessThanOrEqual(30);
    expect(model.watchers[0].title.endsWith("…")).toBe(true);
  });

  it("Now reflects a pending confirm, then busy, then top attention, then idle", () => {
    const confirm = {
      id: "c1",
      request: { toolName: "git.commit", risk: "git", summary: "commit", args: {} },
      resolve: () => {},
    } as any;
    expect(buildSidebarModel(dash({}), [], [], opts({ pendingConfirm: confirm })).now.kind).toBe(
      "confirm",
    );

    const running = [
      { id: "t", kind: "tool" as const, name: "context.snapshot", ts: NOW },
    ];
    expect(buildSidebarModel(dash({}), running, [], opts({ busy: true })).now.kind).toBe("running");

    expect(buildSidebarModel(dash({ inbox: [event({})] }), [], [], opts()).now.kind).toBe(
      "attention",
    );
    expect(buildSidebarModel(dash({}), [], [], opts()).now.kind).toBe("idle");
  });

  it("builds terminal rows from previews using the meaningful line", () => {
    const previews: TerminalPreview[] = [
      {
        terminalId: "term_3a",
        tail: "starting\n\nApprove command? y/n",
        agentState: "waiting",
        updatedAt: NOW,
      },
    ];
    const model = buildSidebarModel(dash({}), [], previews, opts());
    expect(model.terminals[0]).toMatchObject({
      id: "term_3a",
      state: "waiting",
      line: "Approve command? y/n",
      isOutput: true,
    });
  });

  it("keeps the Now card in sync with the top attention row after sorting", () => {
    // DB/inbox order puts a low-severity event first; the sort must win for both.
    const inbox = [
      event({ id: "info", severity: "info", title: "fyi", createdAt: NOW }),
      event({ id: "blk", severity: "blocked", title: "merge conflict", createdAt: NOW - 5 }),
    ];
    const model = buildSidebarModel(dash({ inbox }), [], [], opts());
    expect(model.now.kind).toBe("attention");
    expect(model.now.title).toBe(model.attention[0].title);
    expect(model.attention[0].id).toBe("blk");
  });

  it("truncates a long project name so the 2-line header can't wrap", () => {
    const long = "really-long-monorepo-directory-name-that-would-wrap";
    const model = buildSidebarModel(dash({}), [], [], opts({ columns: 55, project: long }));
    expect(model.status.project.length).toBeLessThanOrEqual(55 - 24);
    expect(model.status.project.endsWith("…")).toBe(true);
  });
});
