import { describe, it, expect } from "vitest";
import { AgentSession } from "../src/agent/loop.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";

function makeSession() {
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
  } as unknown as ModelRouter;
  const ctx = { db, actor: "main" } as unknown as ToolContext;
  const promptContext: MainPromptContext = {
    tier: "operator",
    projectPath: "/proj",
    mcpConnected: true,
    mcpStatusLine: "connected",
    largeModel: "L",
    smallModel: "S",
  };
  const session = new AgentSession({
    router,
    registry: new ToolRegistry(),
    recipeRegistry: new RecipeRegistry(),
    ctx,
    promptContext,
    sessionId: "ses_compact",
  });
  return { session, db };
}

describe("AgentSession.compact (#7)", () => {
  it("actually drops working history, keeping the 3 control messages + one summary", async () => {
    const { session } = makeSession();
    // Simulate a few turns of accumulated history.
    session.injectNote("first");
    session.injectNote("second");
    session.injectNote("third");
    expect(session.getMessages().length).toBeGreaterThan(4);

    session.compact("goals: X. open: none. next: Y.");

    const msgs = session.getMessages();
    // 3 control messages + exactly one compacted summary note.
    expect(msgs.length).toBe(4);
    expect(msgs[0].role).toBe("system");
    expect(msgs[1].content).toContain("# Runtime context");
    expect(msgs[2].content).toContain("# Loaded recipes");
    expect(msgs[3].role).toBe("user");
    expect(msgs[3].content).toContain("compacted summary");
    expect(msgs[3].content).toContain("goals: X");
    // The old turns are gone from context.
    expect(msgs.some((m) => m.content?.includes("first"))).toBe(false);
  });
});
