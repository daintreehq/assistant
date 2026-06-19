import { describe, it, expect } from "vitest";
import { Scheduler } from "../src/daemon/scheduler.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import { ToolRegistry } from "../src/tools/registry.js";
import { ok, type ToolContext, type ToolDef } from "../src/tools/types.js";
import type { ModelRouter } from "../src/models/router.js";
import type { QueueEvent, WatcherVerdict } from "../src/schemas.js";

/** A mutating tool that a non-interactive actor can never run unattended. */
const projectTool: ToolDef = {
  name: "test.project",
  description: "A mutating project test tool.",
  risk: "project",
  parameters: { type: "object", properties: {}, additionalProperties: false },
  async handler() {
    return ok("project ran");
  },
};

/** Minimal fake MCP client implementing the LowLevelMcpClient-ish surface used by ctx. */
function fakeMcp(opts: {
  connected?: boolean;
  getStatus?: Record<string, unknown>;
} = {}) {
  const connected = opts.connected ?? false;
  return {
    isConnected: () => connected,
    status: () => ({ connected, transport: "injected" as const }),
    listTools: async () => [],
    callTool: async (name: string, args?: Record<string, unknown>) => {
      if (name === "terminal.getStatus") {
        // Real shape: { terminalIds } -> { terminals: [{ terminalId, ...status }] }.
        const ids = Array.isArray(args?.terminalIds)
          ? (args!.terminalIds as unknown[]).map(String)
          : [];
        const terminals = ids.map((terminalId) => ({
          terminalId,
          ...(opts.getStatus ?? {}),
        }));
        return {
          text: "",
          content: [],
          structuredContent: { terminals },
          isError: false,
        };
      }
      // terminal.getOutput and anything else
      return { text: "", content: [], isError: false };
    },
  };
}

/** Fake router: chat() returns a no-change summary; json() returns a fixed verdict. */
function fakeRouter(): ModelRouter {
  const verdict: WatcherVerdict = {
    classification: "still_working",
    confidence: 0.7,
    summary: "still working",
    evidence: [],
    recommendedAction: "none",
  };
  return {
    chat: async () => ({ content: "(no change) nothing" }),
    json: async () => verdict,
  } as unknown as ModelRouter;
}

function makeDeps() {
  const db = new Db(":memory:");
  const queue = new Queue(db);
  const router = fakeRouter();
  const registry = new ToolRegistry();
  const mcp = fakeMcp({ connected: false });

  const ctxFor = (actor: ToolContext["actor"]): ToolContext =>
    ({
      config: {} as ToolContext["config"],
      mcp: mcp as unknown as ToolContext["mcp"],
      db,
      queue,
      router,
      projectPath: "/tmp/project",
      actor,
      confirm: async () => true,
      log: () => {},
      daemonActive: () => true,
    }) as ToolContext;

  return { db, queue, router, registry, ctxFor };
}

describe("Scheduler.tick", () => {
  it("fires a one-shot enqueue timer once, publishes a digest event, and marks it fired", async () => {
    const deps = makeDeps();
    const scheduler = new Scheduler(deps);
    const now = 1_000_000;

    const timer = deps.db.insertTimer({
      title: "remind me",
      fireAt: now - 5000, // due in the past
      payloadType: "enqueue",
      payloadJson: JSON.stringify({ type: "enqueue", message: "ping" }),
    });

    await scheduler.tick(now);

    const digest = deps.queue.digest();
    expect(digest).toHaveLength(1);
    expect(digest[0].source).toBe("timer");
    expect(digest[0].summary).toBe("ping");

    const after = deps.db.getTimer(timer.id)!;
    expect(after.status).toBe("fired");
    expect(after.runCount).toBe(1);
    expect(after.lastFiredAt).toBe(now);

    // Idempotent: re-ticking does not re-fire (no longer 'scheduled').
    await scheduler.tick(now + 1);
    expect(deps.queue.digest()).toHaveLength(1);
  });

  it("reschedules a repeating timer: increments runCount, status scheduled, future fireAt", async () => {
    const deps = makeDeps();
    const scheduler = new Scheduler(deps);
    const now = 2_000_000;
    const repeatEveryMs = 60_000;

    const timer = deps.db.insertTimer({
      title: "heartbeat",
      fireAt: now - 1000,
      repeatEveryMs,
      payloadType: "enqueue",
      payloadJson: JSON.stringify({ type: "enqueue", message: "beat" }),
    });

    await scheduler.tick(now);

    const after = deps.db.getTimer(timer.id)!;
    expect(after.runCount).toBe(1);
    expect(after.status).toBe("scheduled");
    expect(after.fireAt).toBe(now + repeatEveryMs);
    expect(after.fireAt).toBeGreaterThan(now);
  });

  // #60 regression: a repeating timer's dedupeKey is stable across firings
  // (`timer:<id>`, NOT keyed by runCount), so successive fires update ONE live
  // inbox item in place instead of leaving a stale row behind on every tick.
  it("repeating timer updates one live inbox item across fires (#60)", async () => {
    const deps = makeDeps();
    const scheduler = new Scheduler(deps);
    const now = 3_000_000;
    const repeatEveryMs = 60_000;

    const timer = deps.db.insertTimer({
      title: "heartbeat",
      fireAt: now - 1000,
      repeatEveryMs,
      payloadType: "enqueue",
      payloadJson: JSON.stringify({ type: "enqueue", message: "beat" }),
    });

    // First fire (runCount → 1), then the rescheduled fire (runCount → 2).
    await scheduler.tick(now);
    expect(deps.db.getTimer(timer.id)!.runCount).toBe(1);
    await scheduler.tick(now + repeatEveryMs);
    expect(deps.db.getTimer(timer.id)!.runCount).toBe(2);

    // Exactly ONE open inbox item — the second fire bumped the first, it didn't
    // spawn a stale duplicate. The dedupeKey carries no run-count segment.
    const digest = deps.queue.digest();
    expect(digest).toHaveLength(1);
    expect(digest[0].source).toBe("timer");
    expect(digest[0].count).toBe(2);
    expect(digest[0].dedupeKey).toBe(`timer:${timer.id}`);
  });

  it("runs a due terminal watcher through fake MCP and sets lastClassification", async () => {
    const deps = makeDeps();
    // Watcher path needs a connected MCP reporting agentState=waiting.
    const mcp = fakeMcp({ connected: true, getStatus: { agentState: "waiting" } });
    const ctxFor = (actor: ToolContext["actor"]): ToolContext =>
      ({
        config: {} as ToolContext["config"],
        mcp: mcp as unknown as ToolContext["mcp"],
        db: deps.db,
        queue: deps.queue,
        router: deps.router,
        projectPath: "/tmp/project",
        actor,
        confirm: async () => true,
        log: () => {},
        daemonActive: () => true,
      }) as ToolContext;
    const scheduler = new Scheduler({ ...deps, ctxFor });
    const now = 3_000_000;

    const watcher = deps.db.insertWatcher({
      kind: "terminal",
      title: "watch terminal",
      goal: "wait for completion",
      targetsJson: JSON.stringify(["term-1"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: now - 1000, // due
    });

    await scheduler.tick(now);

    const after = deps.db.getWatcher(watcher.id)!;
    expect(after.lastClassification).toBe("waiting_for_input");
    // runTerminalWatcherCheck stamps real wall-clock time, then schedules the
    // next check one cadence later.
    expect(typeof after.lastCheckedAt).toBe("number");
    expect(after.nextCheckAt).toBe(after.lastCheckedAt! + watcher.cadenceMs);

    // A waiting_for_input classification is meaningful -> published once.
    const digest = deps.queue.digest({ severityAtLeast: "attention" });
    expect(digest.some((e) => e.source === "terminal_watcher")).toBe(true);
  });

  it("surfaces a call_safe_tool denial as a low-severity event without a duplicate timer error", async () => {
    const deps = makeDeps();
    deps.registry.register(projectTool);
    // The timer actor hits a confirm-required tool: the registry denies it and
    // publishes the surfacing event; the scheduler must not also raise an error.
    const ctxFor = (actor: ToolContext["actor"]): ToolContext =>
      ({
        config: { tier: "operator" } as ToolContext["config"],
        mcp: {} as ToolContext["mcp"],
        db: deps.db,
        queue: deps.queue,
        router: deps.router,
        projectPath: "/tmp/project",
        actor,
        confirm: async () => true,
        log: () => {},
      }) as ToolContext;
    const scheduler = new Scheduler({ ...deps, ctxFor });
    const now = 4_000_000;

    deps.db.insertTimer({
      title: "auto project action",
      fireAt: now - 5000,
      payloadType: "call_safe_tool",
      payloadJson: JSON.stringify({
        type: "call_safe_tool",
        toolCall: { toolName: "test.project", args: { name: "x" } },
      }),
    });

    await scheduler.tick(now);

    const events = deps.queue.digest();
    // No duplicate timer error for the structural denial.
    expect(events.some((e) => e.source === "timer" && e.severity === "error")).toBe(
      false,
    );
    // The registry's low-severity surfacing event is present.
    const denial = events.find((e) => e.source === "system");
    expect(denial).toBeDefined();
    expect(denial!.severity).toBe("info");
    expect(denial!.dedupeKey).toBe("denied:timer:test.project");
  });

  it("still raises a timer error when a call_safe_tool fails for a non-denial reason", async () => {
    const deps = makeDeps();
    const ctxFor = (actor: ToolContext["actor"]): ToolContext =>
      ({
        config: { tier: "operator" } as ToolContext["config"],
        mcp: {} as ToolContext["mcp"],
        db: deps.db,
        queue: deps.queue,
        router: deps.router,
        projectPath: "/tmp/project",
        actor,
        confirm: async () => true,
        log: () => {},
      }) as ToolContext;
    const scheduler = new Scheduler({ ...deps, ctxFor });
    const now = 5_000_000;

    deps.db.insertTimer({
      title: "broken tool call",
      fireAt: now - 5000,
      payloadType: "call_safe_tool",
      payloadJson: JSON.stringify({
        type: "call_safe_tool",
        toolCall: { toolName: "does.not.exist", args: {} },
      }),
    });

    await scheduler.tick(now);

    const events = deps.queue.digest();
    expect(
      events.some((e) => e.source === "timer" && e.severity === "error"),
    ).toBe(true);
  });

  it("isolates a throwing timer so later timers still fire and notify() still delivers", async () => {
    const deps = makeDeps();
    const delivered: QueueEvent[] = [];
    const scheduler = new Scheduler({
      ...deps,
      onAttention: (events) => {
        delivered.push(...events);
      },
    });
    const now = 7_000_000;

    // `bad` is due earlier, so dueTimers (ORDER BY fireAt) fires it FIRST — proving
    // the loop continues to `good` after `bad` throws, rather than aborting.
    const bad = deps.db.insertTimer({
      title: "boom",
      fireAt: now - 5000,
      payloadType: "enqueue",
      payloadJson: JSON.stringify({ type: "enqueue", message: "boom" }),
    });
    const good = deps.db.insertTimer({
      title: "survivor",
      fireAt: now - 4000,
      payloadType: "enqueue",
      payloadJson: JSON.stringify({ type: "enqueue", message: "survived" }),
    });

    // Force reschedule() to throw for the bad timer only. reschedule() calls
    // updateTimer OUTSIDE fireTimer's inner try/catch, so the throw escapes
    // fireTimer entirely — exactly the path the tick-loop guard must contain.
    const realUpdate = deps.db.updateTimer.bind(deps.db);
    deps.db.updateTimer = ((id: string, patch: Partial<Parameters<typeof realUpdate>[1]>) => {
      if (id === bad.id) throw new Error("simulated sqlite failure");
      return realUpdate(id, patch);
    }) as typeof deps.db.updateTimer;

    // The whole tick must resolve, not reject.
    await expect(scheduler.tick(now)).resolves.toBeUndefined();

    // The later timer still fired and finished despite the earlier one throwing.
    const survivor = deps.db.getTimer(good.id)!;
    expect(survivor.status).toBe("fired");
    expect(survivor.runCount).toBe(1);

    // notify() still ran and delivered the surviving timer's attention event.
    expect(delivered.some((e) => e.summary === "survived")).toBe(true);
  });

  it("fires a legacy run_check timer as a deprecated reminder without calling the model", async () => {
    const deps = makeDeps();
    // Count EVERY model path, not just chat — the grounded-reminder fallback must
    // consult none of them. A regression that reached for router.json would
    // otherwise slip past a chat-only spy.
    let modelCalls = 0;
    const countModel =
      (real: (...a: unknown[]) => unknown) =>
      (...args: unknown[]) => {
        modelCalls++;
        return real(...args);
      };
    (deps.router as unknown as Record<string, unknown>).chat = countModel(
      deps.router.chat as unknown as (...a: unknown[]) => unknown,
    );
    (deps.router as unknown as Record<string, unknown>).json = countModel(
      deps.router.json as unknown as (...a: unknown[]) => unknown,
    );

    const delivered: QueueEvent[] = [];
    const scheduler = new Scheduler({
      ...deps,
      onAttention: (events) => {
        delivered.push(...events);
      },
    });
    const now = 8_000_000;

    // A legacy row: the tool can no longer create run_check, but old DB rows must
    // still fire gracefully rather than crash or silently vanish.
    const timer = deps.db.insertTimer({
      title: "legacy check",
      fireAt: now - 1000,
      payloadType: "run_check",
      payloadJson: JSON.stringify({ type: "run_check", checkPrompt: "is the build done?" }),
    });

    await scheduler.tick(now);

    // No ungrounded model call on any router method.
    expect(modelCalls).toBe(0);

    // The prompt is surfaced as a single reminder published at exactly "attention".
    const digest = deps.queue.digest({ severityAtLeast: "attention" });
    expect(digest).toHaveLength(1);
    expect(digest[0].source).toBe("timer");
    expect(digest[0].severity).toBe("attention");
    expect(digest[0].summary).toContain("is the build done?");
    expect(digest[0].summary.toLowerCase()).toContain("deprecated");

    // notify() must still deliver it — the whole point of the isolation fix.
    expect(delivered.some((e) => e.summary.includes("is the build done?"))).toBe(true);

    // And the timer advances normally.
    const after = deps.db.getTimer(timer.id)!;
    expect(after.status).toBe("fired");
    expect(after.runCount).toBe(1);
  });

  it("fires a legacy run_check row whose JSON omits `type`, dispatching on the DB column", async () => {
    const deps = makeDeps();
    const scheduler = new Scheduler(deps);
    const now = 8_500_000;

    // Pathological row: payloadType column says run_check but the JSON blob has no
    // `type`. Dispatching on the column (not payload.type) keeps it from firing as
    // a silent no-op.
    const timer = deps.db.insertTimer({
      title: "typeless legacy check",
      fireAt: now - 1000,
      payloadType: "run_check",
      payloadJson: JSON.stringify({ checkPrompt: "did it deploy?" }),
    });

    await scheduler.tick(now);

    const digest = deps.queue.digest({ severityAtLeast: "attention" });
    expect(digest).toHaveLength(1);
    expect(digest[0].summary).toContain("did it deploy?");
    expect(deps.db.getTimer(timer.id)!.status).toBe("fired");
  });

  it("threads the timer id as actorId so a scoped grant authorizes its call_safe_tool", async () => {
    const deps = makeDeps();
    deps.registry.register(projectTool);
    // ctxFor now forwards the actor id the scheduler passes in.
    const ctxFor = (actor: ToolContext["actor"], actorId?: string): ToolContext =>
      ({
        config: { tier: "operator" } as ToolContext["config"],
        mcp: {} as ToolContext["mcp"],
        db: deps.db,
        queue: deps.queue,
        router: deps.router,
        projectPath: "/tmp/project",
        actor,
        actorId,
        confirm: async () => true,
        log: () => {},
      }) as ToolContext;
    const scheduler = new Scheduler({ ...deps, ctxFor });
    const now = 6_000_000;

    const timer = deps.db.insertTimer({
      title: "granted project action",
      fireAt: now - 5000,
      payloadType: "call_safe_tool",
      payloadJson: JSON.stringify({
        type: "call_safe_tool",
        toolCall: { toolName: "test.project", args: { name: "x" } },
      }),
    });
    // The registry consumes grants against real Date.now() (dispatch's clock),
    // not the scheduler's injected `now`, so the TTL must be real wall-clock.
    deps.db.insertGrant({
      actorId: timer.id,
      actorType: "timer",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    await scheduler.tick(now);

    // The grant authorized the otherwise-denied call: audited grant_ok, no denial.
    const audit = deps.db.listAudit();
    expect(audit.some((a) => a.toolName === "test.project" && a.outcome === "grant_ok")).toBe(
      true,
    );
    const events = deps.queue.digest();
    expect(events.some((e) => e.source === "system")).toBe(false);
  });
});
