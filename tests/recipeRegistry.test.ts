import { describe, it, expect } from "vitest";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { BUILTIN_RECIPES } from "../src/recipes/builtin.js";
import { buildAllTools } from "../src/tools/index.js";

describe("RecipeRegistry", () => {
  it("validates the built-in recipes and exposes all of them", () => {
    const reg = new RecipeRegistry();
    expect(reg.list().length).toBe(BUILTIN_RECIPES.length);
    for (const r of BUILTIN_RECIPES) {
      expect(reg.has(r.id)).toBe(true);
      expect(reg.get(r.id)?.title).toBe(r.title);
    }
  });

  it("throws on a duplicate recipe id", () => {
    const dup = BUILTIN_RECIPES[0];
    expect(() => new RecipeRegistry([dup, { ...dup }])).toThrow(/Duplicate recipe id/);
  });

  it("rejects a structurally invalid recipe", () => {
    expect(() => new RecipeRegistry([{ id: "x" }])).toThrow();
  });

  it("metadataForSelection excludes recipe bodies", () => {
    const reg = new RecipeRegistry();
    const meta = reg.metadataForSelection();
    expect(meta.length).toBe(BUILTIN_RECIPES.length);
    for (const m of meta) {
      expect(m).not.toHaveProperty("body");
    }
    // No recipe body text should leak through the serialized metadata.
    const serialized = JSON.stringify(meta);
    expect(serialized).not.toContain("Procedure:");
  });

  it("getMany resolves known ids and drops unknown ones", () => {
    const reg = new RecipeRegistry();
    const known = BUILTIN_RECIPES[0].id;
    const got = reg.getMany([known, "does.not.exist"]);
    expect(got.map((r) => r.id)).toEqual([known]);
  });

  it("every built-in recipe's requiredTools names a real registered tool", () => {
    // requiredTools is a per-turn execution allowlist (see agent/loop.ts). A typo
    // or a renamed tool would silently starve the model of that tool with no
    // runtime error, so guard the names against the actual tool registry here.
    const toolNames = new Set(buildAllTools().map((t) => t.name));
    const unknown = BUILTIN_RECIPES.flatMap((r) =>
      r.requiredTools
        .filter((name) => !toolNames.has(name))
        .map((name) => `${r.id} → ${name}`),
    );
    expect(unknown).toEqual([]);
  });
});
