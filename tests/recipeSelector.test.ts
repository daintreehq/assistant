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
  it("returns the small model's chosen recipe ids for the query", async () => {
    const router = fakeRouter({
      recipeIds: ["daintree.edits.spawn-visible-agent"],
      confidence: 0.9,
      reason: "Query is about implementing a fix.",
      taskType: "code_edit",
    });
    const res = await selectRecipes({
      router,
      candidates: new RecipeRegistry().metadataForSelection(),
      query: "how do I implement a fix for the parser",
    });
    expect(res.recipeIds).toEqual(["daintree.edits.spawn-visible-agent"]);
    expect(res.taskType).toBe("code_edit");
  });

  it("sends the query and recipe headers to the small model — never bodies", async () => {
    let captured: ChatOptions | undefined;
    const router = fakeRouter(
      { recipeIds: [], confidence: 0.1, reason: "qa", taskType: "qa" },
      (o) => (captured = o),
    );
    await selectRecipes({
      router,
      candidates: new RecipeRegistry().metadataForSelection(),
      query: "what is going on?",
    });
    const userMsg =
      captured?.messages.find((m) => m.role === "user")?.content ?? "";
    // The query and metadata (ids) are present...
    expect(userMsg).toContain("what is going on?");
    expect(userMsg).toContain("daintree.orchestration.basic");
    // ...but no recipe body content leaks (body marker).
    expect(userMsg).not.toContain("Procedure:");
  });

  it("bounds an over-long query before sending it to the model", async () => {
    let captured: ChatOptions | undefined;
    const router = fakeRouter(
      { recipeIds: [], confidence: 0.1, reason: "qa", taskType: "qa" },
      (o) => (captured = o),
    );
    const huge = "x".repeat(5000);
    await selectRecipes({
      router,
      candidates: new RecipeRegistry().metadataForSelection(),
      query: huge,
    });
    const userMsg =
      captured?.messages.find((m) => m.role === "user")?.content ?? "";
    // Query is clipped to the 2000-char cap, so the full 5000-char string never lands.
    expect(userMsg).not.toContain(huge);
    expect(userMsg).toContain("x".repeat(2000));
  });
});
