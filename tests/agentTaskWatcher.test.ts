import { describe, it, expect } from "vitest";
import { agentTaskTools } from "../src/tools/agentTaskTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const spawn = agentTaskTools.find((t) => t.name === "agentTask.spawnForEdits")!;

function ctxWith(daemonActive: () => boolean): ToolContext {
  const db = new Db(":memory:");
  return {
    db,
    actor: "main",
    daemonActive,
    confirm: async () => true,
    log: () => {},
    mcp: {
      isConnected: () => true,
      callTool: async () => ({
        text: "",
        content: [],
        structuredContent: { terminalId: "term_x", taskId: "task_x" },
        isError: false,
      }),
    },
  } as unknown as ToolContext;
}

const args = {
  title: "fix the thing",
  taskPrompt: "make the change",
  watcher: { create: true },
};

describe("agentTask.spawnForEdits watcher lifecycle notice", () => {
  it("warns supervision pauses on close when a watcher is created and the scheduler runs", async () => {
    const res = await spawn.handler(args, ctxWith(() => true));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("watcher");
    expect(res.summary).toContain("pauses when you close the assistant");
  });

  it("warns the watcher will not check when no scheduler is running", async () => {
    const res = await spawn.handler(args, ctxWith(() => false));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not check");
  });

  it("omits the lifecycle note when no watcher is created", async () => {
    const res = await spawn.handler(
      { title: "fix", taskPrompt: "go" },
      ctxWith(() => true),
    );
    expect(res.ok).toBe(true);
    expect(res.summary).not.toContain("pauses when you close the assistant");
  });

  it("attaches a fast supervisor watcher (3s cadence, isSupervisor true)", async () => {
    const ctx = ctxWith(() => true);
    const res = await spawn.handler(args, ctx);
    expect(res.ok).toBe(true);
    const watchers = ctx.db.listWatchers();
    expect(watchers).toHaveLength(1);
    expect(watchers[0].cadenceMs).toBe(3000);
    expect(watchers[0].isSupervisor).toBe(true);
  });

  it("honours an explicit cadence override at or above the tick", async () => {
    const ctx = ctxWith(() => true);
    const res = await spawn.handler(
      { ...args, watcher: { create: true, cadenceMs: 30_000 } },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect(ctx.db.listWatchers()[0].cadenceMs).toBe(30_000);
  });

  it("records spawnMode=edit in optionsJson by default (no mode arg)", async () => {
    const ctx = ctxWith(() => true);
    const res = await spawn.handler(args, ctx);
    expect(res.ok).toBe(true);
    const opts = JSON.parse(ctx.db.listWatchers()[0].optionsJson!);
    expect(opts.spawnMode).toBe("edit");
  });

  it("records spawnMode=explore in optionsJson for an explore spawn", async () => {
    const ctx = ctxWith(() => true);
    const res = await spawn.handler({ ...args, mode: "explore" }, ctx);
    expect(res.ok).toBe(true);
    // The faked MCP returns no worktreeId, so optionsJson carries only spawnMode —
    // verificationScope is absent, confirming the mode is recorded unconditionally.
    const opts = JSON.parse(ctx.db.listWatchers()[0].optionsJson!);
    expect(opts.spawnMode).toBe("explore");
    expect(opts.verificationScope).toBeUndefined();
  });
});
