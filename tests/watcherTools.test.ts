import { describe, it, expect } from "vitest";
import { watcherTools } from "../src/tools/watcherTools.js";
import { Db } from "../src/storage/db.js";
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
  it("warns that supervision pauses on close when the scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => true));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
    expect(res.summary).not.toContain("no scheduler is running");
  });

  it("warns it will not check when no scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => false));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not check");
  });

  it("assumes the scheduler is active (pauses-on-close wording) when daemonActive is absent", async () => {
    const res = await create.handler(args, ctxWith(undefined));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
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
