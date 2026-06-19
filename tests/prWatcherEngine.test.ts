import { describe, it, expect } from "vitest";
import { runPrWatcherCheck } from "../src/daemon/prWatcherEngine.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";
import type { WatcherRecord } from "../src/schemas.js";
import type { PrWatcherOptions } from "../src/daemon/prWatcherEngine.js";

const PR_WATCHER_CADENCE_MS = 60_000;

/**
 * Fake MCP whose forge.getPR returns a configurable result. `pr` is placed in
 * whichever envelope the test asks for (structured / text / wrapped) so the
 * defensive parser is exercised against every shape Daintree might emit.
 */
function fakeMcp(opts: {
  connected?: boolean;
  pr?: Record<string, unknown>;
  envelope?: "structured" | "text" | "wrapped";
  isError?: boolean;
  throws?: boolean;
}) {
  const connected = opts.connected ?? true;
  return {
    isConnected: () => connected,
    callTool: async (name: string) => {
      if (name !== "forge.getPR") throw new Error(`unexpected tool ${name}`);
      if (opts.throws) throw new Error("transport boom");
      const pr = opts.pr ?? {};
      const envelope = opts.envelope ?? "structured";
      if (envelope === "text") {
        return { text: JSON.stringify(pr), content: [], isError: Boolean(opts.isError) };
      }
      if (envelope === "wrapped") {
        return {
          text: "",
          content: [],
          structuredContent: { pr },
          isError: Boolean(opts.isError),
        };
      }
      return {
        text: "",
        content: [],
        structuredContent: pr,
        isError: Boolean(opts.isError),
      };
    },
  };
}

function makeCtx(mcp: ReturnType<typeof fakeMcp>): { ctx: ToolContext; db: Db; queue: Queue } {
  const db = new Db(":memory:");
  const queue = new Queue(db);
  const ctx = {
    config: {} as ToolContext["config"],
    mcp: mcp as unknown as ToolContext["mcp"],
    db,
    queue,
    projectPath: "/tmp/project",
    actor: "watcher",
    confirm: async () => true,
    log: () => {},
  } as unknown as ToolContext;
  return { ctx, db, queue };
}

function insertPrWatcher(
  db: Db,
  options: PrWatcherOptions,
  extra: Partial<WatcherRecord> = {},
): WatcherRecord {
  return db.insertWatcher({
    kind: "pr_state",
    title: `PR #${options.prNumber}`,
    goal: "watch pr",
    targetsJson: JSON.stringify([`PR #${options.prNumber}`]),
    cadenceMs: PR_WATCHER_CADENCE_MS,
    modelTier: "small",
    optionsJson: JSON.stringify(options),
    nextCheckAt: Date.now(),
    ...extra,
  });
}

describe("runPrWatcherCheck transitions", () => {
  it("records a baseline on first observation of an open PR without publishing", async () => {
    const { ctx, db, queue } = makeCtx(
      fakeMcp({ pr: { state: "open", draft: false, updated_at: "2026-01-01T00:00:00Z" } }),
    );
    const w = insertPrWatcher(db, { prNumber: 5 });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
    expect(queue.digest({ severityAtLeast: "debug" })).toHaveLength(0);
    const after = db.getWatcher(w.id)!;
    const opts = JSON.parse(after.optionsJson!) as PrWatcherOptions;
    expect(opts.lastState).toBe("open");
    expect(opts.lastIsDraft).toBe(false);
    expect(opts.lastUpdatedAt).toBe("2026-01-01T00:00:00Z");
    expect(after.nextCheckAt).toBeGreaterThan(Date.now() - 1000);
  });

  it("publishes an attention event and stops the watcher when the PR merges", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ pr: { state: "open", merged: true } }));
    const w = insertPrWatcher(db, { prNumber: 9, lastState: "open" });
    db.insertGrant({
      actorId: w.id,
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 3,
    });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("state_change");
    expect(res.status).toBe("condition_met");
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events).toHaveLength(1);
    expect(events[0].source).toBe("pr_watcher");
    expect(events[0].title).toContain("merged");
    expect(db.getWatcher(w.id)!.status).toBe("condition_met");
    // A stopped watcher must not retain scoped grants.
    expect(db.listGrants(w.id)).toHaveLength(0);
  });

  it("publishes a state_change and stops when the PR closes", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ pr: { state: "closed" } }));
    const w = insertPrWatcher(db, { prNumber: 3, lastState: "open" });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("state_change");
    expect(res.status).toBe("condition_met");
    expect(queue.digest({ severityAtLeast: "attention" })[0].title).toContain("closed");
  });

  it("publishes draft_ready when a draft PR becomes ready for review", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ pr: { state: "open", draft: false } }));
    const w = insertPrWatcher(db, { prNumber: 11, lastState: "open", lastIsDraft: true });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("draft_ready");
    expect(res.status).toBe("active");
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events).toHaveLength(1);
    expect(events[0].title).toContain("ready for review");
  });

  it("publishes activity at info severity when updatedAt advances with no state change", async () => {
    const { ctx, db, queue } = makeCtx(
      fakeMcp({ pr: { state: "open", updated_at: "2026-02-02T00:00:00Z" } }),
    );
    const w = insertPrWatcher(db, {
      prNumber: 1,
      lastState: "open",
      lastUpdatedAt: "2026-01-01T00:00:00Z",
    });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("activity");
    // Below the attention threshold — quiet inbox item, not an interrupt.
    expect(queue.digest({ severityAtLeast: "attention" })).toHaveLength(0);
    const all = queue.digest({ severityAtLeast: "info" });
    expect(all).toHaveLength(1);
    expect(all[0].severity).toBe("info");
    expect(all[0].title).toContain("updated");
  });

  it("does not double-publish activity when the state also changes", async () => {
    const { ctx, db, queue } = makeCtx(
      fakeMcp({ pr: { state: "merged", updated_at: "2026-02-02T00:00:00Z" } }),
    );
    const w = insertPrWatcher(db, {
      prNumber: 1,
      lastState: "open",
      lastUpdatedAt: "2026-01-01T00:00:00Z",
    });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("state_change");
    // Exactly one event — the merge — not a merge + an activity ping.
    expect(queue.digest({ severityAtLeast: "info" })).toHaveLength(1);
  });

  it("stays silent and active when nothing changed", async () => {
    const { ctx, db, queue } = makeCtx(
      fakeMcp({ pr: { state: "open", draft: false, updated_at: "2026-01-01T00:00:00Z" } }),
    );
    const w = insertPrWatcher(db, {
      prNumber: 1,
      lastState: "open",
      lastIsDraft: false,
      lastUpdatedAt: "2026-01-01T00:00:00Z",
    });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
    expect(queue.digest({ severityAtLeast: "debug" })).toHaveLength(0);
  });

  it("stops and announces when first observed already merged", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ pr: { state: "merged" } }));
    const w = insertPrWatcher(db, { prNumber: 8 });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.transition).toBe("state_change");
    expect(res.status).toBe("condition_met");
    expect(queue.digest({ severityAtLeast: "attention" })).toHaveLength(1);
  });
});

describe("runPrWatcherCheck payload shapes", () => {
  it("parses a PR delivered as a JSON text body", async () => {
    const { ctx, db } = makeCtx(
      fakeMcp({ pr: { state: "merged" }, envelope: "text" }),
    );
    const w = insertPrWatcher(db, { prNumber: 2, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.transition).toBe("state_change");
    expect(res.state).toBe("merged");
  });

  it("parses a PR nested under a `pr` wrapper key", async () => {
    const { ctx, db } = makeCtx(
      fakeMcp({ pr: { state: "closed" }, envelope: "wrapped" }),
    );
    const w = insertPrWatcher(db, { prNumber: 4, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.transition).toBe("state_change");
    expect(res.state).toBe("closed");
  });

  it("treats GitLab-style state/work_in_progress fields correctly", async () => {
    const { ctx, db } = makeCtx(
      fakeMcp({ pr: { state: "opened", work_in_progress: false } }),
    );
    const w = insertPrWatcher(db, { prNumber: 6, lastState: "open", lastIsDraft: true });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.transition).toBe("draft_ready");
  });
});

describe("runPrWatcherCheck transient + error handling", () => {
  it("reschedules without publishing when MCP is disconnected", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ connected: false }));
    const w = insertPrWatcher(db, { prNumber: 1, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
    expect(queue.digest({ severityAtLeast: "debug" })).toHaveLength(0);
    expect(db.getWatcher(w.id)!.nextCheckAt).toBeGreaterThan(Date.now() - 1000);
  });

  it("reschedules without publishing when forge.getPR throws", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ throws: true }));
    const w = insertPrWatcher(db, { prNumber: 1, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
    expect(queue.digest({ severityAtLeast: "debug" })).toHaveLength(0);
  });

  it("reschedules without publishing when forge.getPR returns isError", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ isError: true, pr: { state: "merged" } }));
    const w = insertPrWatcher(db, { prNumber: 1, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
    // Even though the payload says "merged", an isError result must not act on it.
    expect(db.getWatcher(w.id)!.status).toBe("active");
  });

  it("reschedules without publishing when the payload is unrecognizable", async () => {
    const { ctx, db } = makeCtx(fakeMcp({ pr: { totallyUnrelated: true } }));
    const w = insertPrWatcher(db, { prNumber: 1, lastState: "open" });
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.published).toBe(false);
    expect(res.status).toBe("active");
  });

  it("disables the watcher and publishes an error on corrupt options", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ pr: { state: "open" } }));
    // optionsJson without a numeric prNumber is corrupt for a PR watcher.
    const w = insertPrWatcher(db, { prNumber: 1 }, { optionsJson: JSON.stringify({}) });
    db.insertGrant({
      actorId: w.id,
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 3,
    });

    const res = await runPrWatcherCheck(w, ctx);

    expect(res.status).toBe("error");
    expect(db.getWatcher(w.id)!.status).toBe("error");
    const events = queue.digest({ severityAtLeast: "error" });
    expect(events).toHaveLength(1);
    expect(events[0].source).toBe("pr_watcher");
    expect(db.listGrants(w.id)).toHaveLength(0);
  });

  it("times out via stopAfterMs even before reading the forge", async () => {
    const { ctx, db, queue } = makeCtx(fakeMcp({ connected: false }));
    const w = insertPrWatcher(
      db,
      { prNumber: 1, lastState: "open" },
      { stopAfterMs: 1, createdAt: Date.now() - 60_000 },
    );
    const res = await runPrWatcherCheck(w, ctx);
    expect(res.status).toBe("timeout");
    expect(db.getWatcher(w.id)!.status).toBe("timeout");
    const events = queue.digest({ severityAtLeast: "info" });
    expect(events).toHaveLength(1);
    expect(events[0].title).toContain("watch ended");
  });
});

describe("runPrWatcherCheck two-step integration", () => {
  it("records baseline on the first poll, then fires on the merge it observes next", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let pr: Record<string, unknown> = { state: "open" };
    const mcp = {
      isConnected: () => true,
      callTool: async () => ({
        text: "",
        content: [],
        structuredContent: pr,
        isError: false,
      }),
    };
    const ctx = {
      config: {} as ToolContext["config"],
      mcp: mcp as unknown as ToolContext["mcp"],
      db,
      queue,
      projectPath: "/tmp/project",
      actor: "watcher",
      confirm: async () => true,
      log: () => {},
    } as unknown as ToolContext;

    const w = insertPrWatcher(db, { prNumber: 21 });

    // First poll: open → baseline only.
    const first = await runPrWatcherCheck(w, ctx);
    expect(first.published).toBe(false);

    // The watcher row now carries the baseline; re-fetch and poll again as merged.
    pr = { state: "open", merged: true };
    const refreshed = db.getWatcher(w.id)!;
    const second = await runPrWatcherCheck(refreshed, ctx);
    expect(second.transition).toBe("state_change");
    expect(queue.digest({ severityAtLeast: "attention" })).toHaveLength(1);

    db.close();
  });
});
