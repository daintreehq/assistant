import { describe, it, expect } from "vitest";
import { selectSkills } from "../src/skills/selector.js";
import { SkillRegistry } from "../src/skills/registry.js";
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

describe("selectSkills", () => {
  it("returns the small model's chosen skill ids for the query", async () => {
    const router = fakeRouter({
      skillIds: ["daintree.edits.spawn-visible-agent"],
      confidence: 0.9,
      reason: "Query is about implementing a fix.",
      taskType: "code_edit",
    });
    const res = await selectSkills({
      router,
      candidates: new SkillRegistry().metadataForSelection(),
      query: "how do I implement a fix for the parser",
    });
    expect(res.skillIds).toEqual(["daintree.edits.spawn-visible-agent"]);
    expect(res.taskType).toBe("code_edit");
  });

  it("sends the query and skill headers to the small model — never bodies", async () => {
    let captured: ChatOptions | undefined;
    const router = fakeRouter(
      { skillIds: [], confidence: 0.1, reason: "qa", taskType: "qa" },
      (o) => (captured = o),
    );
    await selectSkills({
      router,
      candidates: new SkillRegistry().metadataForSelection(),
      query: "what is going on?",
    });
    const userMsg =
      captured?.messages.find((m) => m.role === "user")?.content ?? "";
    // The query and metadata (ids) are present...
    expect(userMsg).toContain("what is going on?");
    expect(userMsg).toContain("daintree.orchestration.basic");
    // ...but no skill body content leaks (body marker).
    expect(userMsg).not.toContain("Procedure:");
  });

  it("bounds an over-long query before sending it to the model", async () => {
    let captured: ChatOptions | undefined;
    const router = fakeRouter(
      { skillIds: [], confidence: 0.1, reason: "qa", taskType: "qa" },
      (o) => (captured = o),
    );
    const huge = "x".repeat(5000);
    await selectSkills({
      router,
      candidates: new SkillRegistry().metadataForSelection(),
      query: huge,
    });
    const userMsg =
      captured?.messages.find((m) => m.role === "user")?.content ?? "";
    // Query is clipped to the 2000-char cap, so the full 5000-char string never lands.
    expect(userMsg).not.toContain(huge);
    expect(userMsg).toContain("x".repeat(2000));
  });
});
