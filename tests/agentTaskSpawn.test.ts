import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { agentTaskTools } from "../src/tools/agentTaskTools.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";

/**
 * Contract-shaped coverage for agentTask.spawnForEdits. Daintree's agent.launch
 * returns ONLY { terminalId, location } (no worktreeId/taskId), so the spawn tool
 * must read terminalId from that shape, attach a watcher, and carry a *deterministic*
 * requestKey. This file also covers issue #79: the launch is now an idempotent saga
 * — a retry reconciles instead of duplicating, and a missing terminalId is treated
 * as ambiguous (a recoverable failure), not a silent success.
 */

type McpResult = {
  text: string;
  content: unknown[];
  structuredContent: Record<string, unknown>;
  isError: boolean;
};

interface CtxOpts {
  connected?: boolean;
  /** agent.launch result. Default: a real { terminalId, location } success. */
  launchResult?: McpResult;
  /** If set, agent.launch throws (transport error). */
  launchThrows?: boolean;
  /** terminal.list result used by reconciliation. Default: empty inventory. */
  terminalListResult?: McpResult;
}

const launchOk = (terminalId = "term_9"): McpResult => ({
  text: "",
  content: [],
  structuredContent: { terminalId, location: "grid" },
  isError: false,
});

const launchNoTerminal = (): McpResult => ({
  text: "",
  content: [],
  structuredContent: { location: "grid" },
  isError: false,
});

const terminalList = (
  terminals: Array<Record<string, unknown>>,
): McpResult => ({
  text: "",
  content: [],
  structuredContent: { terminals },
  isError: false,
});

function ctx(
  db: Db,
  opts: CtxOpts = {},
): ToolContext & { _calls: Array<{ name: string; args: Record<string, unknown> }> } {
  const calls: Array<{ name: string; args: Record<string, unknown> }> = [];
  const mcp = {
    isConnected: () => opts.connected ?? true,
    callTool: async (name: string, args: Record<string, unknown>) => {
      calls.push({ name, args });
      if (name === "agent.launch") {
        if (opts.launchThrows) throw new Error("connection reset");
        return opts.launchResult ?? launchOk();
      }
      if (name === "terminal.list") {
        return opts.terminalListResult ?? terminalList([]);
      }
      return { text: "", content: [], structuredContent: {}, isError: false };
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

function registry(): ToolRegistry {
  const reg = new ToolRegistry();
  reg.registerAll(agentTaskTools);
  return reg;
}

describe("agentTask.spawnForEdits", () => {
  it("reads terminalId from { terminalId, location } and attaches a watcher", async () => {
    const db = new Db(":memory:");
    const reg = registry();
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
      | { terminalId?: string; watcherId?: string; launchId?: string }
      | undefined;
    expect(payload?.terminalId).toBe("term_9");
    expect(payload?.watcherId).toBeTruthy();
    expect(payload?.launchId).toMatch(/^agt_/);

    // A watcher targeting the launched terminal was persisted.
    const watchers = db.dueWatchers(Date.now() + 1_000_000);
    expect(watchers.some((w) => w.targetsJson.includes("term_9"))).toBe(true);

    // The saga record reached the terminal `confirmed` stage.
    const launch = db.getAgentLaunch(payload!.launchId!)!;
    expect(launch.stage).toBe("confirmed");
    expect(launch.terminalId).toBe("term_9");
    expect(launch.watcherId).toBe(payload?.watcherId);

    // agent.launch was called with the constraints block + a deterministic key,
    // plus a "<Agent>: <task>" name derived from the title (default agent → "Claude").
    const call = c._calls.find((x) => x.name === "agent.launch");
    expect(call).toBeDefined();
    expect(String(call?.args.prompt)).toContain("only in this worktree");
    expect(typeof call?.args.requestKey).toBe("string");
    expect(call?.args.name).toBe("Claude: Fix OAuth callback");
    db.close();
  });

  it("derives a DETERMINISTIC requestKey from the task identity (not a random UUID)", async () => {
    // Same {taskPrompt, worktreeId, agentId, mode} on two independent stores must
    // produce the same requestKey — that is what lets a retry reconcile.
    const reg = registry();
    const args = {
      title: "anything",
      taskPrompt: "Repair the OAuth callback handler.",
      worktreeId: "wt-1",
    };
    const a = ctx(new Db(":memory:"));
    const b = ctx(new Db(":memory:"));
    await reg.dispatch("agentTask.spawnForEdits", args, a);
    await reg.dispatch("agentTask.spawnForEdits", { ...args, title: "different title" }, b);

    const keyA = a._calls.find((x) => x.name === "agent.launch")?.args.requestKey;
    const keyB = b._calls.find((x) => x.name === "agent.launch")?.args.requestKey;
    expect(keyA).toBe(keyB);
    expect(String(keyA)).toMatch(/^[0-9a-f]{16}$/);
    a.db.close();
    b.db.close();
  });

  it("changing the task identity changes the requestKey", async () => {
    const reg = registry();
    const base = { title: "t", taskPrompt: "go", worktreeId: "wt-1" };
    const a = ctx(new Db(":memory:"));
    const b = ctx(new Db(":memory:"));
    await reg.dispatch("agentTask.spawnForEdits", base, a);
    await reg.dispatch("agentTask.spawnForEdits", { ...base, worktreeId: "wt-2" }, b);
    const keyA = a._calls.find((x) => x.name === "agent.launch")?.args.requestKey;
    const keyB = b._calls.find((x) => x.name === "agent.launch")?.args.requestKey;
    expect(keyA).not.toBe(keyB);
    a.db.close();
    b.db.close();
  });

  it("classifies a missing terminalId as AMBIGUOUS (recoverable), not a success", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    // Launch returns no terminalId AND the inventory has no matching terminal.
    const c = ctx(db, { launchResult: launchNoTerminal() });

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go", worktreeId: "wt-1", watcher: { create: true } },
      c,
    );

    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");
    expect(res.error?.recoverable).toBe(true);
    const launchId = (res.error?.details as { launchId?: string })?.launchId;
    expect(launchId).toMatch(/^agt_/);
    // The saga record is parked at `ambiguous`, still in-flight for reconciliation.
    expect(db.getAgentLaunch(launchId!)!.stage).toBe("ambiguous");
    // A reconciliation read was attempted.
    expect(c._calls.some((x) => x.name === "terminal.list")).toBe(true);
    db.close();
  });

  it("reconciles an ambiguous launch by matching the deterministic name in terminal.list", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    // No terminalId in the launch response, but the inventory shows our terminal.
    const c = ctx(db, {
      launchResult: launchNoTerminal(),
      terminalListResult: terminalList([
        { id: "term_42", name: "Claude: Fix OAuth", agentId: "claude", worktreeId: "wt-1" },
        { id: "term_other", name: "Codex: something else" },
      ]),
    });

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go", worktreeId: "wt-1", watcher: { create: true } },
      c,
    );

    expect(res.ok).toBe(true);
    const payload = res.result as { terminalId?: string; watcherId?: string; launchId?: string };
    expect(payload.terminalId).toBe("term_42");
    expect(payload.watcherId).toBeTruthy();
    expect(db.getAgentLaunch(payload.launchId!)!.stage).toBe("confirmed");
    db.close();
  });

  it("is idempotent: a retry of an unresolved launch reconciles instead of launching a 2nd agent", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const args = {
      title: "Fix OAuth",
      taskPrompt: "go",
      worktreeId: "wt-1",
      watcher: { create: true },
    };

    // First attempt: no terminalId, empty inventory → ambiguous, record parked.
    const first = ctx(db, { launchResult: launchNoTerminal() });
    const r1 = await reg.dispatch("agentTask.spawnForEdits", args, first);
    expect(r1.ok).toBe(false);
    expect(r1.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");

    // Second attempt (same DB, same args): the agent has since appeared in the
    // inventory. The retry must find the in-flight record and bind it WITHOUT
    // calling agent.launch again.
    const second = ctx(db, {
      launchResult: launchOk("should_not_be_used"),
      terminalListResult: terminalList([
        { id: "term_77", name: "Claude: Fix OAuth", agentId: "claude" },
      ]),
    });
    const r2 = await reg.dispatch("agentTask.spawnForEdits", args, second);

    expect(r2.ok).toBe(true);
    const payload = r2.result as { terminalId?: string };
    expect(payload.terminalId).toBe("term_77");
    // The crucial assertion: the second dispatch did NOT launch another agent.
    expect(second._calls.filter((x) => x.name === "agent.launch")).toHaveLength(0);
    db.close();
  });

  it("does not block a fresh run of the same task after a prior launch confirmed", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const args = { title: "Fix OAuth", taskPrompt: "go", worktreeId: "wt-1" };

    const a = ctx(db, { launchResult: launchOk("term_a") });
    const r1 = await reg.dispatch("agentTask.spawnForEdits", args, a);
    expect(r1.ok).toBe(true);

    // A completed (confirmed) launch is terminal, so the same logical task can be
    // launched again later — agent.launch IS called the second time.
    const b = ctx(db, { launchResult: launchOk("term_b") });
    const r2 = await reg.dispatch("agentTask.spawnForEdits", args, b);
    expect(r2.ok).toBe(true);
    expect((r2.result as { terminalId?: string }).terminalId).toBe("term_b");
    expect(b._calls.filter((x) => x.name === "agent.launch")).toHaveLength(1);
    db.close();
  });

  it("escapes the ambiguous deadlock: a retry with a still-empty inventory launches fresh", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const args = { title: "Fix OAuth", taskPrompt: "go", worktreeId: "wt-1" };

    // First attempt: no terminalId, empty inventory → ambiguous, record parked.
    const first = ctx(db, { launchResult: launchNoTerminal() });
    const r1 = await reg.dispatch("agentTask.spawnForEdits", args, first);
    expect(r1.ok).toBe(false);
    expect(r1.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");

    // Retry, inventory STILL empty (no agent ever started). Reconciliation finds
    // nothing, so the dead-end record is retired and a fresh launch proceeds —
    // rather than returning AMBIGUOUS forever until the session restarts.
    const second = ctx(db, { launchResult: launchOk("term_fresh") });
    const r2 = await reg.dispatch("agentTask.spawnForEdits", args, second);
    expect(r2.ok).toBe(true);
    expect((r2.result as { terminalId?: string }).terminalId).toBe("term_fresh");
    expect(second._calls.filter((x) => x.name === "agent.launch")).toHaveLength(1);
    db.close();
  });

  it("refuses to bind when two terminals share the deterministic launch name", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db, {
      launchResult: launchNoTerminal(),
      terminalListResult: terminalList([
        { id: "term_a", name: "Claude: Fix OAuth" },
        { id: "term_b", name: "Claude: Fix OAuth" },
      ]),
    });
    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go" },
      c,
    );
    // A multi-match is itself ambiguous — no false-positive bind.
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");
    db.close();
  });

  it("reconciles a transport throw when the terminal appears in the inventory", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db, {
      launchThrows: true,
      // Entry keyed by `terminalId` (not `id`) exercises the parser's fallback.
      terminalListResult: terminalList([
        { terminalId: "term_88", name: "Claude: Fix OAuth", agentId: "claude" },
      ]),
    });
    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go" },
      c,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { terminalId?: string }).terminalId).toBe("term_88");
    db.close();
  });

  it("does not block a fresh run of the same task after a prior launch FAILED", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const args = { title: "Fix OAuth", taskPrompt: "go" };

    // First attempt fails with an explicit error response (terminal `failed`).
    const a = ctx(db, {
      launchResult: { text: "no worktree", content: [], structuredContent: {}, isError: true },
    });
    expect((await reg.dispatch("agentTask.spawnForEdits", args, a)).ok).toBe(false);

    // The same task can be launched again — agent.launch IS called.
    const b = ctx(db, { launchResult: launchOk("term_retry") });
    const r2 = await reg.dispatch("agentTask.spawnForEdits", args, b);
    expect(r2.ok).toBe(true);
    expect(b._calls.filter((x) => x.name === "agent.launch")).toHaveLength(1);
    db.close();
  });

  it("treats a transport throw as AMBIGUOUS (the request may have reached Daintree)", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db, { launchThrows: true });

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go" },
      c,
    );

    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");
    const launchId = (res.error?.details as { launchId?: string })?.launchId;
    expect(db.getAgentLaunch(launchId!)!.stage).toBe("ambiguous");
    db.close();
  });

  it("treats an explicit error response as a clean FAILURE (terminal stage)", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db, {
      launchResult: {
        text: "no worktree available",
        content: [],
        structuredContent: {},
        isError: true,
      },
    });

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go" },
      c,
    );

    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("AGENT_LAUNCH_FAILED");
    // A failed launch must not block a later retry (terminal stage).
    db.close();
  });

  it("keeps a successful launch ok() even when watcher attachment throws", async () => {
    const db = new Db(":memory:");
    // Make watcher insertion fail; the agent is still running, so the launch
    // should succeed with a warning, not fail.
    (db as unknown as { insertWatcher: () => never }).insertWatcher = () => {
      throw new Error("disk full");
    };
    const reg = registry();
    const c = ctx(db);

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Fix OAuth", taskPrompt: "go", worktreeId: "wt-1", watcher: { create: true } },
      c,
    );

    expect(res.ok).toBe(true);
    const payload = res.result as { watcherId?: string; watcherWarning?: string; launchId?: string };
    expect(payload.watcherId).toBeUndefined();
    expect(payload.watcherWarning).toContain("could not be attached");
    // The record stays at terminal_bound (recoverable) so a retry can re-attach.
    expect(db.getAgentLaunch(payload.launchId!)!.stage).toBe("terminal_bound");
    db.close();
  });

  it("uses a read-only constraints block in explore mode (no edit language)", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db);

    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      {
        mode: "explore",
        title: "Explore project",
        taskPrompt: "Explore this entire project and report how it fits together.",
        worktreeId: "wt-1",
        watcher: { create: true },
      },
      c,
    );

    expect(res.ok).toBe(true);
    const call = c._calls.find((x) => x.name === "agent.launch");
    const prompt = String(call?.args.prompt);
    expect(prompt).toContain("READ-ONLY exploration");
    expect(prompt).not.toContain("only in this worktree");
    expect(prompt).not.toContain("changed files");
    db.close();
  });

  it("prefixes the launch name with the agentId and stays within the label cap", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db);

    const title =
      "Refactor the authentication middleware and tighten its error handling paths";
    const res = await reg.dispatch(
      "agentTask.spawnForEdits",
      { title, taskPrompt: "Refactor it.", agentId: "codex" },
      c,
    );
    expect(res.ok).toBe(true);

    const call = c._calls.find((x) => x.name === "agent.launch");
    expect(call).toBeDefined();
    const name = String(call?.args.name);
    expect(name.startsWith("Codex: ")).toBe(true);
    expect(name.length).toBeLessThanOrEqual(60);
    expect(typeof call?.args.requestKey).toBe("string");
    db.close();
  });

  it("normalizes whitespace and falls back to a non-empty name", async () => {
    const reg = registry();

    // Distinct stores/prompts so the two launches don't dedupe by idempotency key.
    const collapse = ctx(new Db(":memory:"));
    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "  Fix\n\nOAuth\t callback  ", taskPrompt: "collapse-case" },
      collapse,
    );
    const collapsed = collapse._calls.find((x) => x.name === "agent.launch");
    expect(collapsed?.args.name).toBe("Claude: Fix OAuth callback");

    const blank = ctx(new Db(":memory:"));
    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "   ", taskPrompt: "blank-case" },
      blank,
    );
    const blanked = blank._calls.find((x) => x.name === "agent.launch");
    expect(blanked?.args.name).toBe("Claude: task");

    collapse.db.close();
    blank.db.close();
  });

  it("hard-caps the launch name at 60 chars even for a long agentId", async () => {
    const db = new Db(":memory:");
    const reg = registry();
    const c = ctx(db);

    await reg.dispatch(
      "agentTask.spawnForEdits",
      { title: "Refactor", taskPrompt: "go", agentId: "x".repeat(100) },
      c,
    );
    const call = c._calls.find((x) => x.name === "agent.launch");
    expect(String(call?.args.name).length).toBeLessThanOrEqual(60);
    db.close();
  });

  it("fails cleanly when Daintree MCP is not connected", async () => {
    const db = new Db(":memory:");
    const reg = registry();
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
