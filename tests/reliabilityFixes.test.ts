import { describe, it, expect } from "vitest";
import { Db } from "../src/storage/db.js";
import { extractJson } from "../src/models/fireworks.js";
import { ToolRegistry } from "../src/tools/registry.js";
import { timerTools } from "../src/tools/timerTools.js";
import { agentTaskTools } from "../src/tools/agentTaskTools.js";
import type { ToolContext } from "../src/tools/types.js";

describe("event dedupe keeps createdAt stable (#5)", () => {
  it("does not bump createdAt on a deduped re-publish, but advances updatedAt", () => {
    const db = new Db(":memory:");
    const first = db.upsertEvent({
      source: "terminal_watcher",
      severity: "attention",
      title: "t",
      summary: "first",
      dedupeKey: "k1",
      createdAt: 1_000,
    });
    const second = db.upsertEvent({
      source: "terminal_watcher",
      severity: "attention",
      title: "t",
      summary: "second",
      dedupeKey: "k1",
      createdAt: 9_000,
    });
    expect(second.id).toBe(first.id);
    expect(second.count).toBe(2);
    // createdAt is pinned so the scheduler's "is this new?" check fires once.
    expect(second.createdAt).toBe(1_000);
    // updatedAt advances so the inbox can still order it by recency.
    expect(second.updatedAt).toBe(9_000);
    db.close();
  });
});

describe("notification high-water uses notifiedAt (#5 escalation)", () => {
  it("an event surfaces once via notifiedIsNull, then not again after markNotified", () => {
    const db = new Db(":memory:");
    const ev = db.upsertEvent({
      source: "terminal_watcher",
      severity: "attention",
      title: "t",
      summary: "s",
    });
    const before = db.listEvents({ severityAtLeast: "attention", notifiedIsNull: true });
    expect(before.map((e) => e.id)).toContain(ev.id);

    db.markNotified([ev.id]);
    const after = db.listEvents({ severityAtLeast: "attention", notifiedIsNull: true });
    expect(after.map((e) => e.id)).not.toContain(ev.id);
    // Still visible in the normal digest.
    expect(db.listEvents({ severityAtLeast: "attention" }).map((e) => e.id)).toContain(ev.id);
    db.close();
  });

  it("a below-threshold event that escalates is still surfaced once (never notified yet)", () => {
    const db = new Db(":memory:");
    // Published at info (below attention) with a stable dedupeKey, never notified.
    db.upsertEvent({ source: "timer", severity: "info", title: "t", summary: "s", dedupeKey: "esc" });
    expect(db.listEvents({ severityAtLeast: "attention", notifiedIsNull: true })).toHaveLength(0);
    // Escalates to attention on the same key.
    const esc = db.upsertEvent({ source: "timer", severity: "attention", title: "t", summary: "s2", dedupeKey: "esc" });
    const fresh = db.listEvents({ severityAtLeast: "attention", notifiedIsNull: true });
    expect(fresh.map((e) => e.id)).toContain(esc.id);
    db.close();
  });
});

describe("extractJson balanced extraction (#20)", () => {
  it("ignores trailing prose after a balanced object", () => {
    expect(extractJson('{"a":1} then some chatter')).toBe('{"a":1}');
  });
  it("does not unbalance on braces inside strings", () => {
    expect(extractJson('prefix {"a":"}{"} tail')).toBe('{"a":"}{"}');
  });
  it("handles arrays", () => {
    expect(extractJson("noise [1,2,3] more")).toBe("[1,2,3]");
  });
});

function ctxFor(db: Db, mcp?: Partial<ToolContext["mcp"]>): ToolContext {
  return {
    config: { tier: "operator" } as ToolContext["config"],
    mcp: (mcp ?? { isConnected: () => false }) as ToolContext["mcp"],
    db,
    queue: {} as ToolContext["queue"],
    router: {} as ToolContext["router"],
    projectPath: "/tmp/p",
    actor: "main",
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
}

describe("timer payload validation (#11)", () => {
  it("rejects a call_safe_tool payload with no toolCall at schedule time", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(timerTools);
    const res = await reg.dispatch(
      "timer.schedule",
      { title: "x", delayMs: 1000, payload: { type: "call_safe_tool" } },
      ctxFor(db),
    );
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("INVALID_ARGS");
    db.close();
  });

  it("accepts a well-formed enqueue payload", async () => {
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    reg.registerAll(timerTools);
    const res = await reg.dispatch(
      "timer.schedule",
      { title: "x", delayMs: 1000, payload: { type: "enqueue", message: "hi" } },
      ctxFor(db),
    );
    expect(res.ok).toBe(true);
    db.close();
  });
});

describe("agentTask.spawnForEdits robustness (#8)", () => {
  const tool = agentTaskTools.find((t) => t.name === "agentTask.spawnForEdits")!;

  it("extracts terminalId from result text and creates a watcher", async () => {
    const db = new Db(":memory:");
    const mcp = {
      isConnected: () => true,
      callTool: async () => ({
        text: "launched agent; terminalId: term_9",
        content: [],
        structuredContent: {},
        isError: false,
      }),
    } as unknown as ToolContext["mcp"];
    const res = await tool.handler(
      { title: "fix", taskPrompt: "do it", watcher: { create: true } },
      ctxFor(db, mcp),
    );
    expect(res.ok).toBe(true);
    expect((res.result as { terminalId?: string }).terminalId).toBe("term_9");
    expect((res.result as { watcherId?: string }).watcherId).toBeTruthy();
    db.close();
  });

  it("classifies a launch with no terminalId as ambiguous, not a success (#79)", async () => {
    // #79 supersedes the prior warn-and-succeed behavior: a launch that returns no
    // terminalId is ambiguous (we don't know whether an agent started), so the tool
    // must return a recoverable failure rather than ok(). The same empty result is
    // returned for the terminal.list reconciliation read, so no match is found.
    const db = new Db(":memory:");
    const mcp = {
      isConnected: () => true,
      callTool: async () => ({ text: "ok", content: [], structuredContent: {}, isError: false }),
    } as unknown as ToolContext["mcp"];
    const res = await tool.handler(
      { title: "fix", taskPrompt: "do it", watcher: { create: true } },
      ctxFor(db, mcp),
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("AGENT_LAUNCH_AMBIGUOUS");
    expect(res.error?.recoverable).toBe(true);
    db.close();
  });
});
