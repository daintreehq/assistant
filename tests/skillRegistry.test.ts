import { describe, it, expect } from "vitest";
import { SkillRegistry } from "../src/skills/registry.js";
import { BUILTIN_SKILLS } from "../src/skills/builtin.js";
import { buildAllTools } from "../src/tools/index.js";

describe("SkillRegistry", () => {
  it("validates the built-in skills and exposes all of them", () => {
    const reg = new SkillRegistry();
    expect(reg.list().length).toBe(BUILTIN_SKILLS.length);
    for (const r of BUILTIN_SKILLS) {
      expect(reg.has(r.id)).toBe(true);
      expect(reg.get(r.id)?.title).toBe(r.title);
    }
  });

  it("throws on a duplicate skill id", () => {
    const dup = BUILTIN_SKILLS[0];
    expect(() => new SkillRegistry([dup, { ...dup }])).toThrow(/Duplicate skill id/);
  });

  it("rejects a structurally invalid skill", () => {
    expect(() => new SkillRegistry([{ id: "x" }])).toThrow();
  });

  it("metadataForSelection excludes skill bodies", () => {
    const reg = new SkillRegistry();
    const meta = reg.metadataForSelection();
    expect(meta.length).toBe(BUILTIN_SKILLS.length);
    for (const m of meta) {
      expect(m).not.toHaveProperty("body");
    }
    // No skill body text should leak through the serialized metadata.
    const serialized = JSON.stringify(meta);
    expect(serialized).not.toContain("Procedure:");
  });

  it("getMany resolves known ids and drops unknown ones", () => {
    const reg = new SkillRegistry();
    const known = BUILTIN_SKILLS[0].id;
    const got = reg.getMany([known, "does.not.exist"]);
    expect(got.map((r) => r.id)).toEqual([known]);
  });

  it("every built-in skill's requiredTools names a real registered tool", () => {
    // requiredTools is a per-turn execution allowlist (see agent/loop.ts). A typo
    // or a renamed tool would silently starve the model of that tool with no
    // runtime error, so guard the names against the actual tool registry here.
    const toolNames = new Set(buildAllTools().map((t) => t.name));
    const unknown = BUILTIN_SKILLS.flatMap((r) =>
      r.requiredTools
        .filter((name) => !toolNames.has(name))
        .map((name) => `${r.id} → ${name}`),
    );
    expect(unknown).toEqual([]);
  });
});
