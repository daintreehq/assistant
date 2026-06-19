import { describe, it, expect } from "vitest";
import { runTerminalWatcherCheck } from "../src/daemon/watcherEngine.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";
import type { ModelRouter } from "../src/models/router.js";

/** A router that fails loudly if consulted — proves the rate-limit classification
 *  is reached deterministically, without a model call. */
function throwingRouter(): ModelRouter {
  return {
    chat: async () => {
      throw new Error("router.chat must not be called");
    },
    json: async () => {
      throw new Error("router.json must not be called");
    },
  } as unknown as ModelRouter;
}

function fakeMcp(recentOutput: string, textOnly = false) {
  return {
    isConnected: () => true,
    status: () => ({ connected: true, transport: "injected" as const }),
    listTools: async () => [],
    callTool: async (name: string, args?: Record<string, unknown>) => {
      if (name === "terminal.getStatus") {
        const ids = Array.isArray(args?.terminalIds)
          ? (args!.terminalIds as unknown[]).map(String)
          : [];
        const terminals = ids.map((tid) => ({
          terminalId: tid,
          agentState: "working",
          recentOutput,
        }));
        // Daintree's real shape delivers the array in the text body, not
        // structuredContent (#108) — exercise that path too.
        return textOnly
          ? { text: JSON.stringify({ terminals }), content: [], structuredContent: undefined, isError: false }
          : { text: "", content: [], structuredContent: { terminals }, isError: false };
      }
      return { text: "", content: [], isError: false };
    },
  };
}

function ctxWith(db: Db, queue: Queue, mcp: unknown): ToolContext {
  return {
    config: {} as ToolContext["config"],
    mcp: mcp as ToolContext["mcp"],
    db,
    queue,
    router: throwingRouter(),
    projectPath: "/tmp/p",
    actor: "watcher",
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
}

describe("runTerminalWatcherCheck rate-limit classification (#123)", () => {
  it("classifies a throttled terminal as rate_limited, publishes once, and backs off", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp("…working…\nAPI Error: Rate limit reached. Please retry-after 30s."),
    );
    const w = db.insertWatcher({
      kind: "terminal",
      title: "supervisor",
      goal: "build the feature",
      targetsJson: JSON.stringify(["term-a"]),
      cadenceMs: 10_000, // shorter than the 60s cooldown — proves the back-off
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      isSupervisor: true,
    });

    const before = Date.now();
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    // Deterministically classified as rate_limited at attention severity.
    expect(outcome.classification).toBe("rate_limited");
    expect(outcome.severity).toBe("attention");

    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.length).toBe(1);
    expect(events[0].title.toLowerCase()).toContain("rate limited");

    // The watcher backs off to at least the cooldown (not the 10s cadence).
    const rec = db.getWatcher(w.id)!;
    expect(rec.nextCheckAt).toBeGreaterThanOrEqual(before + 60_000);
    expect(rec.lastClassification).toBe("rate_limited");
    expect(rec.status).toBe("active"); // rate_limited does NOT stop the watcher

    db.close();
  });

  it("detects rate_limited via Daintree's text-body JSON shape too", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp("Error: 429 Too Many Requests — quota exceeded", true),
    );
    const w = db.insertWatcher({
      kind: "terminal",
      title: "supervisor",
      goal: "g",
      targetsJson: JSON.stringify(["term-a"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      isSupervisor: true,
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("rate_limited");
    db.close();
  });

  it("does not flag a terminal whose output is unrelated", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Unrelated output → the deterministic check must not fire (router would throw
    // if consulted, so use output that the engine resolves without a model: empty).
    const ctx = ctxWith(db, queue, fakeMcp(""));
    const w = db.insertWatcher({
      kind: "terminal",
      title: "supervisor",
      goal: "g",
      targetsJson: JSON.stringify(["term-a"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      isSupervisor: true,
    });
    const before = Date.now();
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).not.toBe("rate_limited");
    // Normal cadence, no rate-limit back-off.
    const rec = db.getWatcher(w.id)!;
    expect(rec.nextCheckAt).toBeLessThan(before + 60_000);
    db.close();
  });
});
