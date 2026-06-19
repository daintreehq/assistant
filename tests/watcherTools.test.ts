import { describe, it, expect } from "vitest";
import { watcherTools } from "../src/tools/watcherTools.js";
import { Db } from "../src/storage/db.js";
import { WatchCondition, AgentState } from "../src/schemas.js";
import type { ToolContext } from "../src/tools/types.js";

const create = watcherTools.find((t) => t.name === "watcher.terminal.create")!;
const cancel = watcherTools.find((t) => t.name === "watcher.cancel")!;

function ctxWith(daemonActive?: () => boolean): ToolContext {
  const db = new Db(":memory:");
  return {
    db,
    actor: "main",
    daemonActive,
  } as unknown as ToolContext;
}

describe("watcher.terminal.create defaults", () => {
  it("creates a slow monitor watcher (120s, isSupervisor false) by default", async () => {
    const ctx = ctxWith(() => true);
    const res = await create.handler(args, ctx);
    expect(res.ok).toBe(true);
    const w = ctx.db.getWatcher((res as { result: { id: string } }).result.id);
    expect(w?.cadenceMs).toBe(120_000);
    expect(w?.isSupervisor).toBe(false);
  });
});

const args = {
  terminalIds: ["term_1"],
  title: "build",
  goal: "wait for green",
};

describe("watcher.terminal.create lifecycle notice", () => {
  it("warns the watcher is discarded on close when the scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => true));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("discarded when you close the assistant");
    expect(res.summary).toContain("does not resume on the next launch");
    expect(res.summary).not.toContain("no scheduler is running");
  });

  it("warns it will not check when no scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => false));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not check");
  });

  it("assumes the scheduler is active (discarded-on-close wording) when daemonActive is absent", async () => {
    const res = await create.handler(args, ctxWith(undefined));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("discarded when you close the assistant");
  });
});

describe("WatchCondition rejects degenerate conditions", () => {
  it("rejects an empty contains string", () => {
    expect(WatchCondition.safeParse({ contains: "" }).success).toBe(false);
  });

  it("accepts a non-empty contains string", () => {
    expect(WatchCondition.safeParse({ contains: "done" }).success).toBe(true);
  });

  it("rejects an invalid regex pattern", () => {
    expect(WatchCondition.safeParse({ regex: "[" }).success).toBe(false);
  });

  it("rejects an empty regex pattern (matches everything)", () => {
    expect(WatchCondition.safeParse({ regex: "" }).success).toBe(false);
  });

  it("accepts a valid regex pattern", () => {
    expect(WatchCondition.safeParse({ regex: "done|failed" }).success).toBe(true);
  });

  it("rejects a zero or negative noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: 0 }).success).toBe(false);
    expect(WatchCondition.safeParse({ noOutputForMs: -1 }).success).toBe(false);
  });

  it("rejects a non-finite or non-integer noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: Infinity }).success).toBe(false);
    expect(WatchCondition.safeParse({ noOutputForMs: 0.5 }).success).toBe(false);
  });

  it("accepts a positive noOutputForMs", () => {
    expect(WatchCondition.safeParse({ noOutputForMs: 1 }).success).toBe(true);
  });

  it("rejects an empty or whitespace-only modelJudge string", () => {
    expect(WatchCondition.safeParse({ modelJudge: "" }).success).toBe(false);
    expect(WatchCondition.safeParse({ modelJudge: "  " }).success).toBe(false);
  });

  it("rejects a whitespace-only contains string", () => {
    expect(WatchCondition.safeParse({ contains: "   " }).success).toBe(false);
  });

  it("rejects an invalid leaf wrapped in not", () => {
    expect(WatchCondition.safeParse({ not: { contains: "" } }).success).toBe(false);
  });

  it("rejects empty all/any groups", () => {
    expect(WatchCondition.safeParse({ all: [] }).success).toBe(false);
    expect(WatchCondition.safeParse({ any: [] }).success).toBe(false);
  });

  it("rejects a nested invalid condition inside all", () => {
    expect(WatchCondition.safeParse({ all: [{ any: [] }] }).success).toBe(false);
  });

  it("accepts a well-formed nested group", () => {
    expect(
      WatchCondition.safeParse({ all: [{ contains: "done" }, { runtimeStatusIs: "exited" }] }).success,
    ).toBe(true);
  });
});

describe("watcher.terminal.create parameters schema surfaces the WatchCondition DSL", () => {
  // The hand-written JSON Schema in `parameters` is what Fireworks sees; these
  // tests guard the Fireworks-incompatible patterns and the modelJudge surfacing.
  const params = create.parameters as Record<string, any>;
  const stopWhen = params.properties.stopWhen as Record<string, any>;
  const alertWhen = params.properties.alertWhen as Record<string, any>;
  const branches = stopWhen.anyOf as Record<string, any>[];

  const leafFor = (key: string) =>
    branches.find((b) => b.properties && key in b.properties);

  it("describes stopWhen and alertWhen as a 9-branch anyOf (6 leaves + all/any/not)", () => {
    expect(Array.isArray(branches)).toBe(true);
    expect(branches).toHaveLength(9);
    expect(Array.isArray(alertWhen.anyOf)).toBe(true);
    expect((alertWhen.anyOf as unknown[]).length).toBe(9);
    for (const key of [
      "stateIs",
      "runtimeStatusIs",
      "contains",
      "regex",
      "noOutputForMs",
      "modelJudge",
      "all",
      "any",
      "not",
    ]) {
      expect(leafFor(key), `missing branch for ${key}`).toBeDefined();
    }
  });

  it("mirrors the AgentState enum on the stateIs leaf (catches drift from schemas.ts)", () => {
    const stateIs = leafFor("stateIs")!;
    // Compare against the authoritative Zod enum so a new state in schemas.ts
    // breaks this test rather than letting the hand-written schema drift.
    expect(stateIs.properties.stateIs.enum).toEqual([...AgentState.options]);
  });

  it("documents the modelJudge per-check cost at the watcher's configured tier", () => {
    const modelJudge = leafFor("modelJudge")!;
    const desc = modelJudge.properties.modelJudge.description as string;
    expect(desc).toMatch(/model call/i);
    // Cost must reference the configurable tier, not a blanket "small-model".
    expect(desc).toMatch(/tier/i);
  });

  it("flattens all and any combinators to an anyOf of atomic leaves with minItems", () => {
    for (const key of ["all", "any"] as const) {
      const combinator = leafFor(key)!;
      const arr = combinator.properties[key];
      expect(Array.isArray(arr.items.anyOf)).toBe(true);
      expect(arr.minItems).toBe(1);
      // Children are the atomic leaf set only — no nested all/any/not branch.
      const childKeys = (arr.items.anyOf as Record<string, any>[]).flatMap(
        (b) => Object.keys(b.properties ?? {}),
      );
      expect(childKeys).not.toContain("all");
      expect(childKeys).not.toContain("any");
      expect(childKeys).not.toContain("not");
    }
  });

  it("expresses the DSL `not` as a property, not the JSON-Schema `not` keyword", () => {
    const notBranch = leafFor("not")!;
    expect(notBranch.properties.not.anyOf).toBeDefined();
    expect(notBranch.required).toContain("not");
    // The schema object itself must never carry a top-level JSON-Schema `not`.
    expect("not" in notBranch).toBe(false);
    expect("not" in stopWhen).toBe(false);
  });

  it("uses no Fireworks-incompatible keywords (oneOf / $ref) anywhere", () => {
    const json = JSON.stringify(params);
    expect(json).not.toContain("oneOf");
    expect(json).not.toContain("$ref");
  });
});

describe("watcher.cancel revokes scoped grants", () => {
  it("cancels the watcher and revokes its live automation grants", async () => {
    const db = new Db(":memory:");
    const ctx = { db, actor: "main" } as unknown as ToolContext;
    const w = db.insertWatcher({
      kind: "terminal",
      title: "w",
      goal: "g",
      targetsJson: JSON.stringify(["term_1"]),
      cadenceMs: 1000,
      modelTier: "small",
      nextCheckAt: Date.now(),
    });
    db.insertGrant({
      actorId: w.id,
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 3,
    });
    expect(db.listGrants(w.id)).toHaveLength(1);

    const res = await cancel.handler({ id: w.id }, ctx);
    expect(res.ok).toBe(true);
    expect(db.listGrants(w.id)).toHaveLength(0);
    db.close();
  });
});
