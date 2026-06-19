import { describe, it, expect } from "vitest";
import { AgentSession, CANCELLED_REPLY } from "../src/agent/loop.js";
import { CancelledError } from "../src/models/fireworks.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { AgentEventSink } from "../src/agent/events.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";

function recordingEvents() {
  const events: string[] = [];
  const sink: AgentEventSink = {
    assistantStart: () => events.push("start"),
    assistantToken: (t) => events.push(`tok:${t}`),
    assistantEnd: (c) => events.push(`end:${c}`),
    assistantCancelled: (c) => events.push(`cancelled:${c}`),
    toolCall: ({ name }) => events.push(`call:${name}`),
    toolResult: ({ name, result }) => events.push(`result:${name}:${result.ok}`),
    error: (m) => events.push(`error:${m}`),
    info: (m) => events.push(`info:${m}`),
  };
  return { events, sink };
}

function makeSession(
  routerOverrides: Partial<Record<string, unknown>>,
  sink: AgentEventSink,
) {
  const db = new Db(":memory:");
  const router = {
    json: async () => ({
      recipeIds: [],
      confidence: 0,
      reason: "",
      taskType: "qa",
      keepExisting: false,
    }),
    stream: async () => ({ content: "ok", reasoning: "", toolCalls: [], finishReason: "stop" }),
    chat: async () => ({ content: "S", reasoning: "", toolCalls: [], finishReason: "stop" }),
    ...routerOverrides,
  } as unknown as ModelRouter;
  const ctx = { db, actor: "main" } as unknown as ToolContext;
  const promptContext: MainPromptContext = {
    tier: "operator",
    projectPath: "/proj",
    mcpConnected: true,
    mcpStatusLine: "connected",
    largeModel: "L",
    smallModel: "S",
    schedulerActive: true,
  };
  const session = new AgentSession({
    router,
    registry: new ToolRegistry(),
    recipeRegistry: new RecipeRegistry(),
    ctx,
    promptContext,
    sessionId: "ses_cancel",
    events: sink,
  });
  return { session };
}

describe("AgentSession cancellation (#45)", () => {
  it("returns the cancelled sentinel and emits assistantCancelled (not error) when the stream aborts", async () => {
    const { events, sink } = recordingEvents();
    const { session } = makeSession(
      {
        stream: async () => {
          throw new CancelledError();
        },
      },
      sink,
    );

    const reply = await session.send("hello", { signal: new AbortController().signal });

    expect(reply).toBe(CANCELLED_REPLY);
    expect(events).toContain("cancelled:");
    // A clean cancellation must never surface as a red error.
    expect(events.some((e) => e.startsWith("error:"))).toBe(false);
  });

  it("forwards the abort signal through to router.stream()", async () => {
    const { sink } = recordingEvents();
    let seenSignal: AbortSignal | undefined;
    const controller = new AbortController();
    const { session } = makeSession(
      {
        stream: async (_tier: unknown, opts: { signal?: AbortSignal }) => {
          seenSignal = opts.signal;
          return { content: "done", reasoning: "", toolCalls: [], finishReason: "stop" };
        },
      },
      sink,
    );

    await session.send("hi", { signal: controller.signal });

    expect(seenSignal).toBe(controller.signal);
  });

  it("stamps the turn signal onto the dispatch context (ctx.signal)", async () => {
    const { sink } = recordingEvents();
    const controller = new AbortController();
    const seen: (AbortSignal | undefined)[] = [];

    const registry = new ToolRegistry();
    registry.register({
      name: "probe",
      description: "records the signal it was dispatched with",
      risk: "read",
      readOnly: true,
      parameters: { type: "object", properties: {}, additionalProperties: false },
      async handler(_args, ctx) {
        seen.push(ctx.signal);
        return { ok: true, summary: "ok" };
      },
    });

    let streamCalls = 0;
    const db = new Db(":memory:");
    const router = {
      json: async () => ({
        recipeIds: [],
        confidence: 0,
        reason: "",
        taskType: "qa",
        keepExisting: false,
      }),
      chat: async () => ({ content: "S", reasoning: "", toolCalls: [], finishReason: "stop" }),
      stream: async () => {
        streamCalls++;
        // First turn: ask for the probe tool. Second turn: finish.
        if (streamCalls === 1) {
          return {
            content: "",
            reasoning: "",
            toolCalls: [
              { id: "c1", type: "function" as const, function: { name: "probe", arguments: "{}" } },
            ],
            finishReason: "tool_calls",
          };
        }
        return { content: "done", reasoning: "", toolCalls: [], finishReason: "stop" };
      },
    } as unknown as ModelRouter;
    const ctx = { db, actor: "main", config: { tier: "system" } } as unknown as ToolContext;
    const promptContext: MainPromptContext = {
      tier: "system",
      projectPath: "/proj",
      mcpConnected: true,
      mcpStatusLine: "connected",
      largeModel: "L",
      smallModel: "S",
      schedulerActive: true,
    };
    const session = new AgentSession({
      router,
      registry,
      recipeRegistry: new RecipeRegistry(),
      ctx,
      promptContext,
      sessionId: "ses_probe",
      events: sink,
    });

    await session.send("go", { signal: controller.signal });

    // The handler saw the exact turn signal — proof it propagates past router.stream
    // all the way into registry.dispatch (the gap #81 was about).
    expect(seen).toHaveLength(1);
    expect(seen[0]).toBe(controller.signal);
  });

  it("stubs remaining tool calls as CANCELLED when the abort lands mid-dispatch (no dangling tool_calls)", async () => {
    const { events, sink } = recordingEvents();
    const controller = new AbortController();
    let dispatchCount = 0;

    const registry = new ToolRegistry();
    registry.register({
      name: "abortertool",
      description: "aborts the turn from inside the first dispatch",
      risk: "read",
      readOnly: true,
      parameters: { type: "object", properties: {}, additionalProperties: false },
      async handler() {
        dispatchCount++;
        // Simulate the user hitting Escape while this tool is executing.
        controller.abort();
        return { ok: true, summary: "did work before the cancel landed" };
      },
    });

    let streamCalls = 0;
    const db = new Db(":memory:");
    const router = {
      json: async () => ({
        recipeIds: [],
        confidence: 0,
        reason: "",
        taskType: "qa",
        keepExisting: false,
      }),
      chat: async () => ({ content: "S", reasoning: "", toolCalls: [], finishReason: "stop" }),
      stream: async () => {
        streamCalls++;
        return {
          content: "",
          reasoning: "",
          toolCalls: [
            { id: "call_a", type: "function" as const, function: { name: "abortertool", arguments: "{}" } },
            { id: "call_b", type: "function" as const, function: { name: "abortertool", arguments: "{}" } },
          ],
          finishReason: "tool_calls",
        };
      },
    } as unknown as ModelRouter;
    const ctx = { db, actor: "main", config: { tier: "system" } } as unknown as ToolContext;
    const promptContext: MainPromptContext = {
      tier: "system",
      projectPath: "/proj",
      mcpConnected: true,
      mcpStatusLine: "connected",
      largeModel: "L",
      smallModel: "S",
      schedulerActive: true,
    };
    const session = new AgentSession({
      router,
      registry,
      recipeRegistry: new RecipeRegistry(),
      ctx,
      promptContext,
      sessionId: "ses_midloop",
      events: sink,
    });

    const reply = await session.send("go", { signal: controller.signal });

    expect(reply).toBe(CANCELLED_REPLY);
    // Only the first tool ran; the second was stubbed, never dispatched.
    expect(dispatchCount).toBe(1);
    // The model was not called a second time after the cancel.
    expect(streamCalls).toBe(1);

    const msgs = session.getMessages();
    const assistant = msgs.find((m) => m.role === "assistant" && m.tool_calls?.length);
    expect(assistant?.tool_calls?.map((t) => t.id)).toEqual(["call_a", "call_b"]);
    // History integrity: BOTH tool_call ids have a matching tool reply, so the
    // persisted transcript replays cleanly next turn (no Fireworks 400).
    const replies = msgs.filter((m) => m.role === "tool");
    expect(replies.map((m) => m.tool_call_id).sort()).toEqual(["call_a", "call_b"]);
    // The stubbed reply for the un-run call is explicitly marked CANCELLED.
    const stub = replies.find((m) => m.tool_call_id === "call_b");
    expect(stub?.content).toContain("CANCELLED");

    expect(events).toContain("cancelled:");
    expect(events.some((e) => e.startsWith("error:"))).toBe(false);
  });

  it("does NO model work when the signal is already aborted at entry", async () => {
    const { events, sink } = recordingEvents();
    let streamCalls = 0;
    let jsonCalls = 0;
    let chatCalls = 0;
    const controller = new AbortController();
    controller.abort();
    const { session } = makeSession(
      {
        stream: async () => {
          streamCalls++;
          return { content: "x", reasoning: "", toolCalls: [], finishReason: "stop" };
        },
        json: async () => {
          jsonCalls++;
          return {
            recipeIds: [],
            confidence: 0,
            reason: "",
            taskType: "qa",
            keepExisting: false,
          };
        },
        chat: async () => {
          chatCalls++;
          return { content: "S", reasoning: "", toolCalls: [], finishReason: "stop" };
        },
      },
      sink,
    );

    const reply = await session.send("hi", { signal: controller.signal });

    expect(reply).toBe(CANCELLED_REPLY);
    // Not the stream, nor the recipe selector (json), nor auto-compact (chat).
    expect(streamCalls).toBe(0);
    expect(jsonCalls).toBe(0);
    expect(chatCalls).toBe(0);
    expect(events).toContain("cancelled:");
    // The user message was never pushed into model history (no orphan turn).
    expect(session.getMessages().some((m) => m.content === "hi")).toBe(false);
  });
});
