import { describe, it, expect } from "vitest";
import { AgentSession } from "../src/agent/loop.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";

/**
 * Regression for the watcher-loop failure: the model called one tool 11× with
 * byte-identical arguments, each failing the same way, and burned the entire
 * iteration budget before dying with "Reached the tool-iteration limit". The
 * circuit breaker must stop the turn early once a (tool, exact-args) signature
 * fails REPEAT_FAILURE_ABORT times, and return an explanatory message instead.
 */
function makeSession(stream: () => Promise<unknown>) {
  const db = new Db(":memory:");
  const router = {
    json: async () => ({
      recipeIds: [],
      confidence: 0,
      reason: "",
      taskType: "qa",
      keepExisting: false,
    }),
    stream,
    chat: async () => ({ content: "S", reasoning: "", toolCalls: [], finishReason: "stop" }),
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
    sessionId: "ses_breaker",
  });
  return { session };
}

describe("AgentSession circuit breaker", () => {
  it("stops a turn that repeats one identical failing tool call instead of looping to the iteration cap", async () => {
    let streamCalls = 0;
    // The empty registry makes every call fail with UNKNOWN_TOOL — a stand-in for
    // any persistently-failing call. Same wire name + same args every time.
    const { session } = makeSession(async () => {
      streamCalls++;
      return {
        content: null,
        reasoning: "",
        toolCalls: [
          {
            id: `call_${streamCalls}`,
            type: "function",
            function: { name: "watcher__terminal__create", arguments: '{"stopWhen":{}}' },
          },
        ],
        finishReason: "tool_calls",
      };
    });

    const reply = await session.send("attach a watcher");

    // Broke early with an explanatory message — not the iteration-limit death.
    expect(reply).toMatch(/^Stopped:/);
    expect(reply).not.toMatch(/iteration limit/);
    // Stopped well short of the 12-iteration cap (abort fires on the 3rd repeat).
    expect(streamCalls).toBeLessThanOrEqual(4);
  });

  it("does not trip when the model varies its arguments (genuine progress)", async () => {
    let streamCalls = 0;
    // Each call has DIFFERENT args, so no signature repeats — the breaker must not
    // fire; the turn ends naturally when the model stops requesting tools.
    const { session } = makeSession(async () => {
      streamCalls++;
      if (streamCalls >= 4) {
        return { content: "done", reasoning: "", toolCalls: [], finishReason: "stop" };
      }
      return {
        content: null,
        reasoning: "",
        toolCalls: [
          {
            id: `call_${streamCalls}`,
            type: "function",
            function: { name: "watcher__terminal__create", arguments: `{"attempt":${streamCalls}}` },
          },
        ],
        finishReason: "tool_calls",
      };
    });

    const reply = await session.send("attach a watcher");
    expect(reply).toBe("done");
    expect(streamCalls).toBe(4);
  });
});
