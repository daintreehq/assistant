import { describe, it, expect } from "vitest";
import { AgentSession } from "../src/agent/loop.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { ChatOptions } from "../src/models/fireworks.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";
import type { RecipeSelection } from "../src/recipes/types.js";

function promptCtx(over: Partial<MainPromptContext> = {}): MainPromptContext {
  return {
    tier: "operator",
    projectPath: "/proj",
    projectId: "prj_1",
    mcpConnected: true,
    mcpStatusLine: "connected (injected, 3 tools)",
    largeModel: "large-x",
    smallModel: "small-x",
    ...over,
  };
}

const NO_RECIPES: RecipeSelection = {
  recipeIds: [],
  confidence: 0.1,
  reason: "simple question",
  taskType: "qa",
  keepExisting: false,
};

function makeSession(opts: {
  selection?: RecipeSelection;
  json?: () => Promise<RecipeSelection>;
  onStream?: (o: ChatOptions) => void;
} = {}) {
  const db = new Db(":memory:");
  const recipeRegistry = new RecipeRegistry();
  const registry = new ToolRegistry();
  const calls = { json: 0, stream: 0 };
  const json =
    opts.json ?? (async () => opts.selection ?? NO_RECIPES);
  const router = {
    json: async () => {
      calls.json++;
      return json();
    },
    stream: async (_tier: string, o: ChatOptions) => {
      calls.stream++;
      opts.onStream?.(o);
      return { content: "ok", reasoning: "", toolCalls: [], finishReason: "stop" };
    },
  } as unknown as ModelRouter;
  const ctx = { db, actor: "main" } as unknown as ToolContext;
  const session = new AgentSession({
    router,
    registry,
    recipeRegistry,
    ctx,
    promptContext: promptCtx(),
    sessionId: "ses_test",
  });
  return { session, db, calls };
}

describe("AgentSession control messages", () => {
  it("starts with [base, runtime, recipes] system messages", () => {
    const { session } = makeSession();
    const msgs = session.getMessages();
    expect(msgs.length).toBe(3);
    expect(msgs.every((m) => m.role === "system")).toBe(true);
    expect(msgs[0].content).toContain("Daintree Assistant");
    expect(msgs[1].content).toContain("# Runtime context");
    expect(msgs[2].content).toContain("# Loaded recipes");
  });

  it("refreshRuntimeContext rewrites only message index 1", () => {
    const { session } = makeSession();
    const before = session.getMessages();
    const base = before[0].content;
    const recipes = before[2].content;
    session.refreshRuntimeContext(promptCtx({ tier: "supervisor" }));
    const after = session.getMessages();
    expect(after[0].content).toBe(base);
    expect(after[2].content).toBe(recipes);
    expect(after[1].content).toContain("supervisor");
  });

  it("setRecipes rewrites only message index 2", () => {
    const { session } = makeSession();
    const before = session.getMessages();
    const base = before[0].content;
    const runtime = before[1].content;
    session.setRecipes(["daintree.edits.spawn-visible-agent"]);
    const after = session.getMessages();
    expect(after[0].content).toBe(base);
    expect(after[1].content).toBe(runtime);
    expect(after[2].content).toContain("Spawn a visible agent for file changes");
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
  });

  it("drops unknown recipe ids and falls back to the empty bundle", () => {
    const { session } = makeSession();
    session.setRecipes(["nope.not.real"]);
    expect(session.getActiveRecipeIds()).toEqual([]);
    expect(session.getMessages()[2].content).toContain("No task-specific recipes");
  });

  it("appends user/assistant turns after the control messages", async () => {
    const { session } = makeSession();
    await session.send("hello there");
    const msgs = session.getMessages();
    expect(msgs[3].role).toBe("user");
    expect(msgs[3].content).toBe("hello there");
    expect(msgs[4].role).toBe("assistant");
  });

  it("passes the stable promptCacheKey to the large model stream", async () => {
    let captured: ChatOptions | undefined;
    const { session } = makeSession({ onStream: (o) => (captured = o) });
    await session.send("hi");
    expect(captured?.promptCacheKey).toBe("daintree-main-system-v1");
  });

  it("loads the recipe the small model selects and logs the decision", async () => {
    const { session, db } = makeSession({
      selection: {
        recipeIds: ["daintree.edits.spawn-visible-agent"],
        confidence: 0.9,
        reason: "user asked to implement",
        taskType: "code_edit",
        keepExisting: false,
      },
    });
    await session.send("implement the new feature");
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
    expect(session.getMessages()[2].content).toContain(
      "Recipe id: daintree.edits.spawn-visible-agent",
    );
    const log = db.listRecipeSelections();
    expect(log.length).toBe(1);
    expect(log[0].taskType).toBe("code_edit");
    expect(JSON.parse(log[0].selectedRecipeIdsJson)).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
  });

  it("keeps existing recipes when the selector returns only unknown ids", async () => {
    const { session } = makeSession({
      selection: {
        recipeIds: ["hallucinated.recipe.id"],
        confidence: 0.4,
        reason: "made something up",
        taskType: "unknown",
        keepExisting: false,
      },
    });
    session.setRecipes(["daintree.edits.spawn-visible-agent"]);
    await session.forceRecipeRefresh("do a thing");
    // Hallucinated-only selection must not clear the active set.
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
  });

  it("clears recipes when the selector returns an explicitly empty selection", async () => {
    const { session } = makeSession({ selection: NO_RECIPES });
    session.setRecipes(["daintree.edits.spawn-visible-agent"]);
    await session.forceRecipeRefresh("just a question");
    expect(session.getActiveRecipeIds()).toEqual([]);
  });

  it("does not push known ids out of the cap with unknown ones", () => {
    const { session } = makeSession();
    session.setRecipes([
      "x.unknown.1",
      "x.unknown.2",
      "x.unknown.3",
      "daintree.orchestration.basic",
    ]);
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.orchestration.basic",
    ]);
  });

  it("keepExisting keeps the active set even if recipeIds differ", async () => {
    const { session } = makeSession({
      selection: {
        recipeIds: ["daintree.recipe.run-or-create"],
        confidence: 0.5,
        reason: "task unchanged",
        taskType: "same",
        keepExisting: true,
      },
    });
    session.setRecipes(["daintree.orchestration.basic"]);
    await session.forceRecipeRefresh("more of the same");
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.orchestration.basic",
    ]);
  });

  it("forceRecipeRefresh returns false and keeps recipes when the selector throws", async () => {
    const { session } = makeSession({
      json: async () => {
        throw new Error("flash model down");
      },
    });
    session.setRecipes(["daintree.orchestration.basic"]);
    const ok = await session.forceRecipeRefresh("anything");
    expect(ok).toBe(false);
    expect(session.getActiveRecipeIds()).toEqual([
      "daintree.orchestration.basic",
    ]);
  });

  it("throttles selection: re-runs on a trigger term, skips a plain follow-up", async () => {
    const { session, calls } = makeSession();
    await session.send("hello"); // turn 0 → always selects
    expect(calls.json).toBe(1);
    await session.send("just chatting about the weather"); // no trigger term
    expect(calls.json).toBe(1); // skipped
    await session.send("please implement the fix"); // trigger term
    expect(calls.json).toBe(2);
  });
});
