import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { agentTaskTools } from "../src/tools/agentTaskTools.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";

/**
 * Contract-shaped coverage for agentTask.spawnForEdits: Daintree's agent.launch
 * returns ONLY { terminalId, location } (no worktreeId/taskId), so the spawn tool
 * must read terminalId from that shape, attach a watcher, and carry a requestKey.
 */
function ctx(db: Db): ToolContext & {
  _calls: Array<{ name: string; args: Record<string, unknown> }>;
} {
  const calls: Array<{ name: string; args: Record<string, unknown> }> = [];
  const mcp = {
    isConnected: () => true,
    callTool: async (name: string, args: Record<string, unknown>) => {
      calls.push({ name, args });
      // Real agent.launch result shape.
      return {
        text: "",
        content: [],
        structuredContent: { terminalId: "term_9", location: "grid" },
        isError: false,
      };
    },
  } as unknown as ToolContext["mcp"];
  const c = {
    config: { tier: "operator" } as ToolContext["config"],
    mcp,
    db,
    queue: new Queue(db),
    router: {} as ToolContext["router"],
    projectPath: "/tmp/p",
    actor: "main",
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
  return Object.assign(c, { _calls: calls }) as ToolContext & {
    _calls: typeof calls;
  };
}

describe("agentTask.spawnForEdits", () => {
  it("reads terminalId from { terminalId, location } and attaches a watcher", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);
    const c = ctx(db);

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      {
        title: "Fix OAuth callback",
        taskPrompt: "Repair the OAuth callback handler.",
        worktreeId: "wt-1",
        watcher: { create: true },
      },
      c,
    );

    expect(res.ok).toBe(true);
    const payload = res.result as
      | { terminalId?: string; watcherId?: string }
      | undefined;
    expect(payload?.terminalId).toBe("term_9");
    expect(payload?.watcherId).toBeTruthy();

    // A watcher targeting the launched terminal was persisted.
    const watchers = db.dueWatchers(Date.now() + 1_000_000);
    expect(watchers.some((w) => w.targetsJson.includes("term_9"))).toBe(true);

    // agent.launch was called with the constraints block + an idempotency key.
    const launch = c._calls.find((x) => x.name === "agent.launch");
    expect(launch).toBeDefined();
    expect(String(launch?.args.prompt)).toContain("only in this worktree");
    expect(typeof launch?.args.requestKey).toBe("string");
    db.close();
  });

  it("fails cleanly when Daintree MCP is not connected", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);
    const c = ctx(db);
    (c as { mcp: ToolContext["mcp"] }).mcp = {
      isConnected: () => false,
    } as unknown as ToolContext["mcp"];

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "x", taskPrompt: "y" },
      c,
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("MCP_UNAVAILABLE");
    db.close();
  });
});
