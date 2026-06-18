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

    // agent.launch was called with the constraints block + an idempotency key,
    // plus a "<Agent>: <task>" name derived from the title (default agent → "Claude").
    const launch = c._calls.find((x) => x.name === "agent.launch");
    expect(launch).toBeDefined();
    expect(String(launch?.args.prompt)).toContain("only in this worktree");
    expect(typeof launch?.args.requestKey).toBe("string");
    expect(launch?.args.name).toBe("Claude: Fix OAuth callback");
    db.close();
  });

  it("uses a read-only constraints block in explore mode (no edit language)", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);
    const c = ctx(db);

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      {
        mode: "explore",
        title: "Explore canopy-app",
        taskPrompt: "Explore this entire project and report how it fits together.",
        worktreeId: "wt-1",
        watcher: { create: true },
      },
      c,
    );

    expect(res.ok).toBe(true);
    const launch = c._calls.find((x) => x.name === "agent.launch");
    const prompt = String(launch?.args.prompt);
    // Read-only framing, and NONE of the edit-mode "make changes" language.
    expect(prompt).toContain("READ-ONLY exploration");
    expect(prompt).not.toContain("only in this worktree");
    expect(prompt).not.toContain("changed files");
    db.close();
  });

  it("prefixes the launch name with the agentId and stays within the label cap", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);
    const c = ctx(db);

    const title =
      "Refactor the authentication middleware and tighten its error handling paths";
    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title, taskPrompt: "Refactor it.", agentId: "codex" },
      c,
    );
    expect(res.ok).toBe(true);

    const launch = c._calls.find((x) => x.name === "agent.launch");
    expect(launch).toBeDefined();
    const name = String(launch?.args.name);
    expect(name.startsWith("Codex: ")).toBe(true);
    expect(name.length).toBeLessThanOrEqual(60);
    // The idempotency key is still present alongside the new name field.
    expect(typeof launch?.args.requestKey).toBe("string");
    db.close();
  });

  it("normalizes whitespace and falls back to a non-empty name", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);

    const collapse = ctx(db);
    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "  Fix\n\nOAuth\t callback  ", taskPrompt: "go" },
      collapse,
    );
    const collapsed = collapse._calls.find((x) => x.name === "agent.launch");
    expect(collapsed?.args.name).toBe("Claude: Fix OAuth callback");

    const blank = ctx(db);
    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "   ", taskPrompt: "go" },
      blank,
    );
    const blanked = blank._calls.find((x) => x.name === "agent.launch");
    expect(blanked?.args.name).toBe("Claude: task");

    db.close();
  });

  it("hard-caps the launch name at 60 chars even for a long agentId", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(agentTaskTools);
    const c = ctx(db);

    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Refactor", taskPrompt: "go", agentId: "x".repeat(100) },
      c,
    );
    const launch = c._calls.find((x) => x.name === "agent.launch");
    expect(String(launch?.args.name).length).toBeLessThanOrEqual(60);
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
