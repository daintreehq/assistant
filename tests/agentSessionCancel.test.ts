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
