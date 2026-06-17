import { describe, it, expect } from "vitest";
import { selectRecipes } from "../src/recipes/selector.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ChatOptions } from "../src/models/fireworks.js";

function fakeRouter(selection: unknown, capture?: (o: ChatOptions) => void): ModelRouter {
  return {
    json: async (_tier: string, opts: ChatOptions) => {
      capture?.(opts);
      return selection;
    },
  } as unknown as ModelRouter;
}

describe("selectRecipes", () => {
  it("returns the small model's chosen recipe ids", async () => {
    const router = fakeRouter({
      recipeIds: ["daintree.edits.spawn-visible-agent"],
      confidence: 0.9,
      reason: "User asked to implement a fix.",
      taskType: "code_edit",
      keepExisting: false,
    });
    const res = await selectRecipes({
      router,
      registry: new RecipeRegistry(),
      userInput: "implement a fix for the parser",
      recentMessages: [],
      activeRecipeIds: [],
    });
    expect(res.recipeIds).toEqual(["daintree.edits.spawn-visible-agent"]);
    expect(res.taskType).toBe("code_edit");
  });

  it("sends only recipe metadata to the small model — never bodies", async () => {
    let captured: ChatOptions | undefined;
    const router = fakeRouter(
      { recipeIds: [], confidence: 0.1, reason: "qa", taskType: "qa", keepExisting: false },
      (o) => (captured = o),
    );
    await selectRecipes({
      router,
      registry: new RecipeRegistry(),
      userInput: "what is going on?",
      recentMessages: [
        { role: "user", content: "earlier message" },
        { role: "assistant", content: "earlier reply" },
      ],
      activeRecipeIds: [],
    });
    const userMsg =
      captured?.messages.find((m) => m.role === "user")?.content ?? "";
    // Metadata (ids) present...
    expect(userMsg).toContain("daintree.orchestration.basic");
    // ...but no recipe body content leaks (body marker).
    expect(userMsg).not.toContain("Procedure:");
  });
});
