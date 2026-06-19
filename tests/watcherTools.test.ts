import { describe, it, expect } from "vitest";
import { watcherTools } from "../src/tools/watcherTools.js";
import { Db } from "../src/storage/db.js";
import { WatchCondition } from "../src/schemas.js";
import type { ToolContext } from "../src/tools/types.js";

const create = watcherTools.find((t) => t.name === "watcher.terminal.create")!;
const cancel = watcherTools.find((t) => t.name === "watcher.cancel")!;

function ctxWith(daemonActive?: () => boolean): ToolContext {
  const db = new Db(":memory:");
  return {
    db,
    actor: "main",
    daemonActive,
  } as unknown as ToolContext;
}

describe("watcher.terminal.create defaults", () => {
  it("creates a slow monitor watcher (120s, isSupervisor false) by default", async () => {
    const ctx = ctxWith(() => true);
    const res = await create.handler(args, ctx);
    expect(res.ok).toBe(true);
    const w = ctx.db.getWatcher((res as { result: { id: string } }).result.id);
    expect(w?.cadenceMs).toBe(120_000);
    expect(w?.isSupervisor).toBe(false);
  });
});

const args = {
  terminalIds: ["term_1"],
  title: "build",
  goal: "wait for green",
};

describe("watcher.terminal.create lifecycle notice", () => {
  it("warns the watcher is discarded on close when the scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => true));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("discarded when you close the assistant");
    expect(res.summary).toContain("does not resume on the next launch");
    expect(res.summary).not.toContain("no scheduler is running");
  });

  it("warns it will not check when no scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => false));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not check");
  });

  it("assumes the scheduler is active (discarded-on-close wording) when daemonActive is absent", async () => {
    const res = await create.handler(args, ctxWith(undefined));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("discarded when you close the assistant");
  });
});

describe("WatchCondition rejects degenerate conditions", () => {
  it("rejects an empty contains string", () => {
    expect(WatchCondition.safeParse({ contains: "" }).success).toBe(false);
  });

  it("accepts a non-empty contains string", () => {
    expect(WatchCondition.safeParse({ contains: "done" }).success).toBe(true);
  });

  it("rejects an invalid regex pattern", () => {
    expect(WatchCondition.safeParse({ regex: "[" }).success).toBe(false);
  });

  it("rejects an empty regex pattern (matches everything)", () => {
    expect(WatchCondition.safeParse({ regex: "" }).success).toBe(false);
  });

  it("accepts a valid regex pattern", () => {
    expect(WatchCondition.safeParse({ regex: "done|failed" }).success).toBe(true);
  });

  it("rejects a zero or negative noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: 0 }).success).toBe(false);
    expect(WatchCondition.safeParse({ noOutputForMs: -1 }).success).toBe(false);
  });

  it("rejects a non-finite or non-integer noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: Infinity }).success).toBe(false);
    expect(WatchCondition.safeParse({ noOutputForMs: 0.5 }).success).toBe(false);
  });

  it("accepts a positive noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: 1 }).success).toBe(true);
  });

  it("rejects an empty or whitespace-only modelJudge string", () => {
    expect(WatchCondition.safeParse({ modelJudge: "" }).success).toBe(false);
    expect(WatchCondition.safeParse({ modelJudge: "  " }).success).toBe(false);
  });

  it("rejects a whitespace-only contains string", () => {
    expect(WatchCondition.safeParse({ contains: "   " }).success).toBe(false);
  });

  it("rejects an invalid leaf wrapped in not", () => {
    expect(WatchCondition.safeParse({ not: { contains: "" } }).success).toBe(false);
  });

  it("rejects empty all/any groups", () => {
    expect(WatchCondition.safeParse({ all: [] }).success).toBe(false);
    expect(WatchCondition.safeParse({ any: [] }).success).toBe(false);
  });

  it("rejects a nested invalid condition inside all", () => {
    expect(WatchCondition.safeParse({ all: [{ any: [] }] }).success).toBe(false);
  });

  it("accepts a well-formed nested group", () => {
    expect(
      WatchCondition.safeParse({ all: [{ contains: "done" }, { runtimeStatusIs: "exited" }] }).success,
    ).toBe(true);
  });
});

describe("watcher.cancel revokes scoped grants", () => {
  it("cancels the watcher and revokes its live automation grants", async () => {
    const db = new Db(":memory:");
    const ctx = { db, actor: "main" } as unknown as ToolContext;
    const w = db.insertWatcher({
      kind: "terminal",
      title: "w",
      goal: "g",
      targetsJson: JSON.stringify(["term_1"]),
      cadenceMs: 1000,
      modelTier: "small",
      nextCheckAt: Date.now(),
    });
    db.insertGrant({
      actorId: w.id,
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 3,
    });
    expect(db.listGrants(w.id)).toHaveLength(1);

    const res = await cancel.handler({ id: w.id }, ctx);
    expect(res.ok).toBe(true);
    expect(db.listGrants(w.id)).toHaveLength(0);
    db.close();
  });
});
