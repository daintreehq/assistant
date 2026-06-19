import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { mcpTools } from "../src/tools/mcpTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

/**
 * Issue #126: workflow.startWorkOnIssue must atomically attach a supervising
 * watcher to the terminal it launches, instead of relying on a separate, racy
 * watcher.terminal.create follow-up. These tests drive the tool through the real
 * registry (so tier-gating + confirmation run) with a fake MCP whose
 * structuredContent we control to exercise each terminalId path.
 */
type Calls = Array<{ name: string; args: Record<string, unknown> }>;

function ctx(
  structuredContent: unknown,
  opts: { daemonActive?: () => boolean } = {},
): ToolContext & { _calls: Calls; db: Db } {
  const calls: Calls = [];
  const mcp = {
    isConnected: () => true,
    callTool: async (name: string, args: Record<string, unknown>) => {
      calls.push({ name, args });
      return { text: "ok", content: [], structuredContent, isError: false };
    },
  } as unknown as ToolContext["mcp"];
  const c = {
    config: { tier: "operator" } as ToolContext["config"],
    mcp,
    db: new Db(":memory:"),
    queue: {} as ToolContext["queue"],
    router: {} as ToolContext["router"],
    projectPath: "/tmp/p",
    actor: "main",
    daemonActive: opts.daemonActive ?? (() => true),
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
  return Object.assign(c, { _calls: calls }) as ToolContext & { _calls: Calls; db: Db };
}

const RESULT = {
  issueNumber: 126,
  issueTitle: "Attach a supervising watcher atomically",
  worktreeId: "wt_abc",
  terminalId: "term_abc",
};

async function start(
  reg: ToolRegistry,
  c: ToolContext,
  args: Record<string, unknown> = { arguments: { issueId: "126" } },
) {
  return reg.dispatch("workflow.startWorkOnIssue", args, c);
}

function reg() {
  const r = new ToolRegistry();
  r.registerAll(mcpTools);
  return r;
}

describe("workflow.startWorkOnIssue atomic supervisor watcher (#126)", () => {
  it("attaches one supervisor watcher targeting the launched terminal", async () => {
    const c = ctx(RESULT);
    const res = await start(reg(), c);
    expect(res.ok).toBe(true);
    const watchers = c.db.listWatchers();
    expect(watchers).toHaveLength(1);
    const w = watchers[0];
    expect(JSON.parse(w.targetsJson)).toEqual(["term_abc"]);
    expect(w.isSupervisor).toBe(true);
    expect(w.cadenceMs).toBe(3000);
    expect(w.modelTier).toBe("small");
    if (res.ok) expect((res.result as { watcherId?: string }).watcherId).toBe(w.id);
  });

  it("records spawnMode=edit and scopes verification to the worktree", async () => {
    const c = ctx(RESULT);
    await start(reg(), c);
    const opts = JSON.parse(c.db.listWatchers()[0].optionsJson!);
    expect(opts.spawnMode).toBe("edit");
    expect(opts.verificationScope).toEqual({ worktreeId: "wt_abc" });
  });

  it("omits verificationScope when no worktreeId is returned", async () => {
    const c = ctx({ ...RESULT, worktreeId: undefined });
    await start(reg(), c);
    const opts = JSON.parse(c.db.listWatchers()[0].optionsJson!);
    expect(opts.spawnMode).toBe("edit");
    expect(opts.verificationScope).toBeUndefined();
  });

  it("derives the watcher title/goal from the issue number and title", async () => {
    const c = ctx(RESULT);
    await start(reg(), c);
    const w = c.db.listWatchers()[0];
    expect(w.title).toContain("Attach a supervising watcher atomically");
    expect(w.goal).toContain("issue #126");
  });

  it("emits the foreground-only lifecycle note in the summary", async () => {
    const c = ctx(RESULT);
    const res = await start(reg(), c);
    expect(res.summary).toContain("term_abc");
    expect(res.summary).toContain("discarded when you close the assistant");
  });

  it("notes the watcher will not check when no scheduler is running", async () => {
    const c = ctx(RESULT, { daemonActive: () => false });
    const res = await start(reg(), c);
    expect(res.summary).toContain("no scheduler is running");
  });

  it("does NOT attach a watcher when terminalId is null (best-effort skip)", async () => {
    const c = ctx({ ...RESULT, terminalId: null });
    const res = await start(reg(), c);
    expect(res.ok).toBe(true);
    expect(c.db.listWatchers()).toHaveLength(0);
  });

  it("does NOT attach a watcher when terminalId is absent", async () => {
    const c = ctx({ issueNumber: 126, worktreeId: "wt_abc" });
    const res = await start(reg(), c);
    expect(res.ok).toBe(true);
    expect(c.db.listWatchers()).toHaveLength(0);
  });

  it("skips attachment when attachWatcher:false", async () => {
    const c = ctx(RESULT);
    const res = await start(reg(), c, {
      arguments: { issueId: "126" },
      attachWatcher: false,
    });
    expect(res.ok).toBe(true);
    expect(c.db.listWatchers()).toHaveLength(0);
  });

  it("stays ok with a warning when watcher insertion fails (best-effort)", async () => {
    const c = ctx(RESULT);
    c.db.insertWatcher = () => {
      throw new Error("disk full");
    };
    const res = await start(reg(), c);
    expect(res.ok).toBe(true);
    if (res.ok) {
      expect((res.result as { watcherId?: string }).watcherId).toBeUndefined();
      expect((res.result as { watcherWarning?: string }).watcherWarning).toContain("disk full");
    }
    expect(res.summary).toContain("could not be attached");
  });

  it("does not create a duplicate when an active supervisor already targets the terminal", async () => {
    const c = ctx(RESULT);
    // First launch attaches the supervisor.
    await start(reg(), c);
    expect(c.db.listWatchers()).toHaveLength(1);
    const firstId = c.db.listWatchers()[0].id;
    // A retry of the same launch must reuse it, not stack a second watcher.
    const res = await start(reg(), c);
    expect(res.ok).toBe(true);
    expect(c.db.listWatchers()).toHaveLength(1);
    if (res.ok) expect((res.result as { watcherId?: string }).watcherId).toBe(firstId);
  });

  it("never forwards attachWatcher to the Daintree MCP call", async () => {
    const c = ctx(RESULT);
    await start(reg(), c, {
      arguments: { issueId: "126" },
      requestKey: "rk-9",
      attachWatcher: true,
    });
    const call = c._calls.find((x) => x.name === "workflow.startWorkOnIssue");
    expect(call?.args).not.toHaveProperty("attachWatcher");
    expect(call?.args.issueId).toBe("126");
    expect(call?.args.requestKey).toBe("rk-9");
  });
});
