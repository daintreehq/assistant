import { AgentSession } from "../src/agent/loop.js";
import {
  type AgentEventSink,
  type RunIdRef,
  multiSink,
  RunEventSink,
} from "../src/agent/events.js";
import { Db } from "../src/storage/db.js";
import type { MainPromptContext } from "../src/models/prompts/index.js";
import { RecipeRegistry } from "../src/recipes/registry.js";

// Recipe selection runs at the top of send(); a stub keeps it a no-op so these
// tests stay focused on the event sink.
const recipeRegistry = new RecipeRegistry();
const selectNone = async () => ({
  recipeIds: [],
  confidence: 0,
  reason: "test",
  taskType: "none",
  keepExisting: false,
});

function recordingSink() {
  const events: string[] = [];
  const sink: AgentEventSink = {
    assistantStart: () => events.push("start"),
    assistantToken: (t) => events.push(`tok:${t}`),
    assistantEnd: (c) => events.push(`end:${c}`),
    assistantCancelled: (c) => events.push(`cancelled:${c}`),
    toolCall: ({ id, name }) => events.push(`call:${name}:${id}`),
    toolResult: ({ id, name, result }) =>
      events.push(`result:${name}:${result.ok}:${id}`),
    error: (m) => events.push(`error:${m}`),
    info: (m) => events.push(`info:${m}`),
  };
  return { events, sink };
}

const PROMPT_CTX: MainPromptContext = {
  tier: "operator",
  projectPath: "/tmp/x",
  mcpConnected: false,
  mcpStatusLine: "degraded",
  largeModel: "large",
  smallModel: "small",
  schedulerActive: true,
};

const ctx = {
  db: { insertMessage: () => {}, insertRecipeSelection: () => {} },
} as any;

function chatResult(over: Partial<{ content: string; toolCalls: any[] }>) {
  return {
    content: over.content ?? "",
    reasoning: "",
    toolCalls: over.toolCalls ?? [],
    finishReason: "stop",
  };
}

describe("AgentSession emits structured events instead of rendering", () => {
  it("streams tokens then ends a plain answer", async () => {
    const { events, sink } = recordingSink();
    const router = {
      stream: async (
        _tier: string,
        _opts: unknown,
        onToken?: (t: string) => void,
      ) => {
        onToken?.("Hel");
        onToken?.("lo");
        return chatResult({ content: "Hello" });
      },
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t1",
      events: sink,
    });

    const out = await session.send("hi");
    expect(out).toBe("Hello");
    expect(events).toEqual(["start", "tok:Hel", "tok:lo", "end:Hello"]);
  });

  it("emits toolCall/toolResult then the final answer", async () => {
    const { events, sink } = recordingSink();
    const responses = [
      chatResult({
        // The model returns the OpenAI-legal wire name; the loop must translate
        // it back to the internal dotted name before dispatch and events.
        toolCalls: [
          { id: "c1", type: "function", function: { name: "fs__search", arguments: "{}" } },
        ],
      }),
      chatResult({ content: "done" }),
    ];
    let n = 0;
    const router = { stream: async () => responses[n++], json: selectNone } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: (w: string) => w.replaceAll("__", "."),
      dispatch: async () => ({ ok: true, summary: "found 2 files" }),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t2",
      events: sink,
    });

    const out = await session.send("search");
    expect(out).toBe("done");
    // The call id (c1) flows through both events so results match by id.
    expect(events).toContain("call:fs.search:c1");
    expect(events).toContain("result:fs.search:true:c1");
    expect(events[events.length - 1]).toBe("end:done");
  });

  it("emits a usage event with context pressure and a priced cost", async () => {
    const usageEvents: any[] = [];
    const sink: AgentEventSink = {
      assistantStart: () => {},
      assistantToken: () => {},
      assistantEnd: () => {},
      assistantCancelled: () => {},
      toolCall: () => {},
      toolResult: () => {},
      error: () => {},
      info: () => {},
      usage: (e) => usageEvents.push(e),
    };
    const router = {
      modelFor: () => "minimax-m3",
      stream: async () => ({
        content: "hi",
        reasoning: "",
        toolCalls: [],
        finishReason: "stop",
        usage: { promptTokens: 1000, completionTokens: 200, totalTokens: 1200 },
      }),
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "tu",
      events: sink,
    });

    await session.send("hi");
    expect(usageEvents).toHaveLength(1);
    const u = usageEvents[0];
    expect(u.promptTokens).toBe(1000);
    expect(u.completionTokens).toBe(200);
    expect(u.totalTokens).toBe(1200);
    expect(u.model).toBe("minimax-m3");
    expect(u.tier).toBe("large");
    // Context pressure is measured against the auto-compact threshold.
    expect(u.contextThreshold).toBe(60_000);
    expect(u.contextTokens).toBeGreaterThan(0);
    // minimax-m3 has a known rate, so cost is a concrete number.
    expect(typeof u.costUsd).toBe("number");
    expect(u.costUsd).toBeGreaterThan(0);
  });

  it("strips the Fireworks account path from the usage event model id", async () => {
    const usageEvents: any[] = [];
    const sink: AgentEventSink = {
      assistantStart: () => {},
      assistantToken: () => {},
      assistantEnd: () => {},
      assistantCancelled: () => {},
      toolCall: () => {},
      toolResult: () => {},
      error: () => {},
      info: () => {},
      usage: (e) => usageEvents.push(e),
    };
    const router = {
      // Production config carries the full account path; the UI must see the bare id.
      modelFor: () => "accounts/fireworks/models/minimax-m3",
      stream: async () => ({
        content: "hi",
        reasoning: "",
        toolCalls: [],
        finishReason: "stop",
        usage: { promptTokens: 100, completionTokens: 20, totalTokens: 120 },
      }),
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;
    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "tm",
      events: sink,
    });

    await session.send("hi");
    expect(usageEvents[0].model).toBe("minimax-m3");
    // Cost is still computed correctly from the (path-stripped) priced model.
    expect(usageEvents[0].costUsd).toBeGreaterThan(0);
  });

  it("leaves cost undefined (not $0) when the provider reports no usage", async () => {
    const usageEvents: any[] = [];
    const sink: AgentEventSink = {
      assistantStart: () => {},
      assistantToken: () => {},
      assistantEnd: () => {},
      assistantCancelled: () => {},
      toolCall: () => {},
      toolResult: () => {},
      error: () => {},
      info: () => {},
      usage: (e) => usageEvents.push(e),
    };
    const router = {
      modelFor: () => "minimax-m3",
      // No `usage` field — mirrors a stream that produced no usage chunk.
      stream: async () => ({
        content: "hi",
        reasoning: "",
        toolCalls: [],
        finishReason: "stop",
      }),
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;
    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "tn",
      events: sink,
    });

    await session.send("hi");
    expect(usageEvents).toHaveLength(1);
    // Context pressure is still meaningful (estimated over our own buffer)...
    expect(usageEvents[0].contextTokens).toBeGreaterThan(0);
    // ...but cost is "no data", not a misleading zero.
    expect(usageEvents[0].costUsd).toBeUndefined();
  });

  it("reports model errors through the sink", async () => {
    const { events, sink } = recordingSink();
    const router = {
      stream: async () => {
        throw new Error("boom");
      },
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t3",
      events: sink,
    });

    const out = await session.send("hi");
    expect(out).toContain("Model error: boom");
    expect(events.some((e) => e.startsWith("error:"))).toBe(true);
  });
});

describe("multiSink", () => {
  it("delivers to a healthy sink even when another sink throws", () => {
    const healthy = recordingSink();
    const throwing: AgentEventSink = {
      assistantStart: () => {
        throw new Error("boom");
      },
      assistantToken: () => {
        throw new Error("boom");
      },
      assistantEnd: () => {
        throw new Error("boom");
      },
      toolCall: () => {
        throw new Error("boom");
      },
      toolResult: () => {
        throw new Error("boom");
      },
      error: () => {
        throw new Error("boom");
      },
      info: () => {
        throw new Error("boom");
      },
    };
    // Throwing sink first, so its failure can't short-circuit the healthy one.
    const fan = multiSink(throwing, healthy.sink);
    expect(() => {
      fan.assistantStart();
      fan.assistantEnd("hi");
      fan.info("note");
    }).not.toThrow();
    expect(healthy.events).toEqual(["start", "end:hi", "info:note"]);
  });
});

describe("RunEventSink", () => {
  let db: Db;
  beforeEach(() => {
    db = new Db(":memory:");
  });
  afterEach(() => {
    db.close();
  });

  it("is a no-op when no run is active (writes no rows at all)", () => {
    const ref: RunIdRef = { current: undefined };
    const sink = new RunEventSink(db, ref);
    sink.assistantStart();
    sink.assistantToken("hi");
    sink.assistantEnd("hi");
    const count = db.raw().prepare("SELECT COUNT(*) AS n FROM run_events").get() as {
      n: number;
    };
    expect(count.n).toBe(0);
  });

  it("writes typed, seq-ordered rows scoped to the active run", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    sink.assistantStart();
    sink.toolCall({ id: "c1", name: "fs.read", args: { path: "a" }, startedAt: 0 });
    sink.toolResult({
      id: "c1",
      name: "fs.read",
      result: { ok: true, summary: "read a" },
      endedAt: 1,
    });
    sink.assistantEnd("done");

    const rows = db.listRunEvents("run_1");
    expect(rows.map((r) => r.type)).toEqual([
      "assistant:start",
      "tool:call",
      "tool:result",
      "assistant:end",
    ]);
    expect(rows.map((r) => r.seq)).toEqual([0, 1, 2, 3]);
    expect(JSON.parse(rows[1].payload!)).toEqual({
      id: "c1",
      name: "fs.read",
      args: { path: "a" },
    });
    expect(JSON.parse(rows[2].payload!)).toEqual({
      id: "c1",
      name: "fs.read",
      ok: true,
      summary: "read a",
    });
    expect(JSON.parse(rows[3].payload!)).toEqual({ content: "done" });
  });

  it("does not persist tokens of the final round (assistant:end carries them)", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    sink.assistantStart();
    sink.assistantToken("Hel");
    sink.assistantToken("lo");
    sink.assistantEnd("Hello");
    // The buffered tokens are dropped — assistant:end is authoritative, so no
    // duplicate assistant:content row, and no per-token rows.
    expect(db.listRunEvents("run_1").map((r) => r.type)).toEqual([
      "assistant:start",
      "assistant:end",
    ]);
  });

  it("flushes intermediate prose (tokens before a tool call) as one assistant:content row", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    // Round 1: the model says something, then calls a tool — no assistantEnd fires.
    sink.assistantStart();
    sink.assistantToken("Let me ");
    sink.assistantToken("check.");
    sink.toolCall({ id: "c1", name: "fs.read", args: {}, startedAt: 0 });
    sink.toolResult({
      id: "c1",
      name: "fs.read",
      result: { ok: true, summary: "ok" },
      endedAt: 1,
    });
    // Round 2: the final answer.
    sink.assistantStart();
    sink.assistantEnd("done");

    const rows = db.listRunEvents("run_1");
    expect(rows.map((r) => r.type)).toEqual([
      "assistant:start",
      "assistant:content",
      "tool:call",
      "tool:result",
      "assistant:start",
      "assistant:end",
    ]);
    // The interstitial prose survives, scoped to the run, ahead of the tool call.
    expect(JSON.parse(rows[1].payload!)).toEqual({ content: "Let me check." });
  });

  it("records tool:result with the auditId cross-reference into audit_log", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    sink.toolResult({
      id: "c1",
      name: "fs.read",
      result: { ok: true, summary: "ok", auditId: "aud_1234" },
      endedAt: 0,
    });
    expect(JSON.parse(db.listRunEvents("run_1")[0].payload!).auditId).toBe("aud_1234");
  });

  it("caps an oversized payload to a valid-JSON truncation marker", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    sink.assistantEnd("x".repeat(100_000));
    const payload = JSON.parse(db.listRunEvents("run_1")[0].payload!);
    expect(payload.truncated).toBe(true);
    expect(payload.bytes).toBeGreaterThan(100_000);
    expect(typeof payload.preview).toBe("string");
  });

  it("resets the seq counter when the run id changes", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    sink.assistantStart();
    sink.assistantEnd("a");
    ref.current = "run_2";
    sink.assistantStart();
    sink.assistantEnd("b");
    expect(db.listRunEvents("run_1").map((r) => r.seq)).toEqual([0, 1]);
    expect(db.listRunEvents("run_2").map((r) => r.seq)).toEqual([0, 1]);
  });

  it("swallows a DB write failure rather than breaking the turn", () => {
    const ref: RunIdRef = { current: "run_1" };
    const brokenDb = {
      insertRunEvent: () => {
        throw new Error("disk full");
      },
    } as unknown as Db;
    const sink = new RunEventSink(brokenDb, ref);
    expect(() => {
      sink.assistantStart();
      sink.assistantEnd("done");
    }).not.toThrow();
  });

  it("writes a fallback payload for an unserializable value", () => {
    const ref: RunIdRef = { current: "run_1" };
    const sink = new RunEventSink(db, ref);
    // A cyclic object can't be JSON.stringify'd; the sink must still record a row.
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    sink.toolResult({
      id: "c1",
      name: "weird",
      result: { ok: true, summary: cyclic as unknown as string },
      endedAt: 0,
    });
    const rows = db.listRunEvents("run_1");
    expect(rows).toHaveLength(1);
    expect(JSON.parse(rows[0].payload!)).toEqual({ error: "unserializable" });
  });
});
