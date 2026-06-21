import { AgentSession } from "../src/agent/loop.js";
import { RunEventSink, multiSink, type RunIdRef } from "../src/agent/events.js";
import { Db } from "../src/storage/db.js";
import type { MainPromptContext } from "../src/models/prompts/index.js";
import { SkillRegistry } from "../src/skills/registry.js";

// Integration: AgentSession.send() drives the durable RunEventSink end-to-end,
// mirroring the App.create() wiring (multiSink(RunEventSink, …) + a shared
// runIdRef the session stamps per turn).

const skillRegistry = new SkillRegistry();
const selectNone = async () => ({
  skillIds: [],
  confidence: 0,
  reason: "test",
  taskType: "none",
  keepExisting: false,
});

const PROMPT_CTX: MainPromptContext = {
  tier: "operator",
  projectPath: "/tmp/x",
  mcpConnected: false,
  mcpStatusLine: "degraded",
  largeModel: "large",
  smallModel: "small",
  schedulerActive: true,
};

function chatResult(
  over: Partial<{ content: string; reasoning: string; toolCalls: any[] }>,
) {
  return {
    content: over.content ?? "",
    reasoning: over.reasoning ?? "",
    toolCalls: over.toolCalls ?? [],
    finishReason: "stop",
  };
}

function buildSession(db: Db, ref: RunIdRef, router: any) {
  const registry = {
    toOpenAITools: () => [],
    resolveWireName: (w: string) => w.replaceAll("__", "."),
    dispatch: async () => ({ ok: true, summary: "found 2 files" }),
  } as any;
  return new AgentSession({
    router,
    registry,
    skillRegistry,
    ctx: { db } as any,
    promptContext: PROMPT_CTX,
    sessionId: "ses_t",
    events: multiSink(new RunEventSink(db, ref), {
      assistantStart() {},
      assistantToken() {},
      assistantEnd() {},
      toolCall() {},
      toolResult() {},
      error() {},
      info() {},
    }),
    runIdRef: ref,
  });
}

describe("AgentSession persists a run's event log", () => {
  let db: Db;
  beforeEach(() => {
    db = new Db(":memory:");
  });
  afterEach(() => {
    db.close();
  });

  it("writes start, tool call/result, and end rows in seq order", async () => {
    const ref: RunIdRef = { current: undefined };
    const responses = [
      chatResult({
        toolCalls: [
          { id: "c1", type: "function", function: { name: "fs__search", arguments: "{}" } },
        ],
      }),
      chatResult({ content: "done" }),
    ];
    let n = 0;
    const router = { stream: async () => responses[n++], json: selectNone } as any;
    const session = buildSession(db, ref, router);

    await session.send("search");

    // Exactly one run id was minted and recorded in the audit-correlatable log.
    const runIds = (
      db
        .raw()
        .prepare("SELECT DISTINCT runId FROM run_events")
        .all() as Array<{ runId: string }>
    ).map((r) => r.runId);
    expect(runIds).toHaveLength(1);
    expect(runIds[0]).toMatch(/^run_[0-9a-f]{8}$/);

    // The loop emits assistantStart per model round, so the tool round and the
    // final answer round each open with an "assistant:start". Each round also
    // records a "usage" row (token/cost/context accounting) right after the model
    // call returns, before any tool calls or the final answer.
    const rows = db.listRunEvents(runIds[0]);
    expect(rows.map((r) => r.type)).toEqual([
      "assistant:start",
      "usage",
      "tool:call",
      "tool:result",
      "assistant:start",
      "usage",
      "assistant:end",
    ]);
    expect(rows.map((r) => r.seq)).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });

  it("mints a distinct run id per send() and clears the ref after each turn", async () => {
    const router = {
      stream: async () => chatResult({ content: "ok" }),
      json: selectNone,
    } as any;
    const ref: RunIdRef = { current: undefined };
    const session = buildSession(db, ref, router);

    await session.send("one");
    expect(ref.current).toBeUndefined(); // cleared in finally

    await session.send("two");
    expect(ref.current).toBeUndefined();

    const runIds = (
      db
        .raw()
        .prepare("SELECT DISTINCT runId FROM run_events")
        .all() as Array<{ runId: string }>
    ).map((r) => r.runId);
    expect(runIds).toHaveLength(2);
    expect(runIds[0]).not.toBe(runIds[1]);
  });

  it("stamps the same run id on audit_log and run_events for a dispatched tool", async () => {
    const ref: RunIdRef = { current: undefined };
    const responses = [
      chatResult({
        toolCalls: [
          { id: "c1", type: "function", function: { name: "fs__search", arguments: "{}" } },
        ],
      }),
      chatResult({ content: "done" }),
    ];
    let n = 0;
    const router = { stream: async () => responses[n++], json: selectNone } as any;
    // A registry stub that writes a real audit row carrying ctx.runId, so the
    // cross-layer correlation (audit_log.runId === run_events.runId) is exercised.
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: (w: string) => w.replaceAll("__", "."),
      dispatch: async (_name: string, _args: unknown, ctx: any) => {
        const row = db.insertAudit({
          actor: "main",
          toolName: "fs.search",
          argsJson: "{}",
          outcome: "ok",
          durationMs: 1,
          summary: "ok",
          runId: ctx.runId,
        });
        return { ok: true, summary: "ok", auditId: row.id };
      },
    } as any;
    const session = new AgentSession({
      router,
      registry,
      skillRegistry,
      ctx: { db } as any,
      promptContext: PROMPT_CTX,
      sessionId: "ses_x",
      events: multiSink(new RunEventSink(db, ref), {
        assistantStart() {},
        assistantToken() {},
        assistantEnd() {},
        toolCall() {},
        toolResult() {},
        error() {},
        info() {},
      }),
      runIdRef: ref,
    });

    await session.send("search");

    const auditRunId = db.listAudit()[0].runId;
    const eventRunIds = (
      db
        .raw()
        .prepare("SELECT DISTINCT runId FROM run_events")
        .all() as Array<{ runId: string }>
    ).map((r) => r.runId);
    expect(auditRunId).toBeDefined();
    expect(eventRunIds).toEqual([auditRunId]);
    // The tool:result event carries the audit row id for a precise join.
    const toolResult = db
      .listRunEvents(auditRunId!)
      .find((e) => e.type === "tool:result");
    expect(JSON.parse(toolResult!.payload!).auditId).toBe(db.listAudit()[0].id);
  });

  it("persists the model's final-round reasoning into the assistant:end payload", async () => {
    const ref: RunIdRef = { current: undefined };
    const router = {
      stream: async () =>
        chatResult({ content: "answer", reasoning: "step-by-step rationale" }),
      json: selectNone,
    } as any;
    const session = buildSession(db, ref, router);

    await session.send("think");

    const runId = (
      db
        .raw()
        .prepare("SELECT DISTINCT runId FROM run_events")
        .all() as Array<{ runId: string }>
    )[0].runId;
    const end = db.listRunEvents(runId).find((e) => e.type === "assistant:end")!;
    const payload = JSON.parse(end.payload!);
    expect(payload.content).toBe("answer");
    expect(payload.reasoning).toBe("step-by-step rationale");
  });

  it("omits reasoning from assistant:end when the model produced none", async () => {
    const ref: RunIdRef = { current: undefined };
    const router = {
      stream: async () => chatResult({ content: "answer" }), // reasoning: ""
      json: selectNone,
    } as any;
    const session = buildSession(db, ref, router);

    await session.send("answer");

    const runId = (
      db
        .raw()
        .prepare("SELECT DISTINCT runId FROM run_events")
        .all() as Array<{ runId: string }>
    )[0].runId;
    const end = db.listRunEvents(runId).find((e) => e.type === "assistant:end")!;
    const payload = JSON.parse(end.payload!);
    expect(payload.content).toBe("answer");
    expect("reasoning" in payload).toBe(false); // no spurious reasoning: "" key
  });

  it("clears the run id even when the model errors mid-turn", async () => {
    const router = {
      stream: async () => {
        throw new Error("boom");
      },
      json: selectNone,
    } as any;
    const ref: RunIdRef = { current: undefined };
    const session = buildSession(db, ref, router);

    const out = await session.send("hi");
    expect(out).toContain("Model error: boom");
    expect(ref.current).toBeUndefined();
    // The error event was still scoped to the run before the ref cleared.
    const rows = db
      .raw()
      .prepare("SELECT type FROM run_events")
      .all() as Array<{ type: string }>;
    expect(rows.map((r) => r.type)).toContain("error");
  });
});
