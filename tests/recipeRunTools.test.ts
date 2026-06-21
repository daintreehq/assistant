import { describe, it, expect, vi, afterEach } from "vitest";
import { recipeRunTools } from "../src/tools/recipeRunTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const advance = recipeRunTools.find((t) => t.name === "recipe.step.advance")!;
const get = recipeRunTools.find((t) => t.name === "recipe.run.get")!;
const load = recipeRunTools.find((t) => t.name === "recipe.load")!;
const find = recipeRunTools.find((t) => t.name === "recipe.find")!;

function ctx(sessionId = "ses_test"): ToolContext {
  const db = new Db(":memory:");
  return { db, actor: "main", sessionId } as unknown as ToolContext;
}

/** A context with no bound session — recipe progress tools should fail on it. */
function noSessionCtx(): ToolContext {
  const db = new Db(":memory:");
  return { db, actor: "main" } as unknown as ToolContext;
}

afterEach(() => {
  vi.useRealTimers();
});

type StepProgress = { index: number; status: string; notes?: string; ts: number };
type AdvanceResult = {
  result: {
    state: {
      id: string;
      recipeId: string;
      currentStep: number;
      status: string;
      steps: StepProgress[];
      completedAt?: number;
    };
  };
};
type GetResult = {
  result: { state: AdvanceResult["result"]["state"] | null };
};

describe("recipe.step.advance", () => {
  it("creates an active run on the first advance and records the completed step", async () => {
    const c = ctx();
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2, status: "done" },
      c,
    );
    expect(res.ok).toBe(true);
    const { state } = (res as AdvanceResult).result;
    expect(state.id).toMatch(/^rrs_[0-9a-f]{8}$/);
    expect(state.status).toBe("active");
    expect(state.currentStep).toBe(2);
    expect(state.steps).toEqual([
      expect.objectContaining({ index: 1, status: "done" }),
    ]);
    expect(state.completedAt).toBeUndefined();

    // Persisted under the (session, recipe) natural key.
    const stored = c.db.getRecipeRunState("ses_test", "r.flow")!;
    expect(stored.currentStep).toBe(2);
  });

  it("defaults the step status to 'done' when omitted", async () => {
    const c = ctx();
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2 },
      c,
    );
    expect((res as AdvanceResult).result.state.steps[0].status).toBe("done");
  });

  it("advances an existing run and accumulates the step checkpoint array", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 2, nextStep: 3 },
      c,
    );
    const { state } = (res as AdvanceResult).result;
    expect(state.currentStep).toBe(3);
    expect(state.status).toBe("active");
    expect(state.steps.map((s) => s.index)).toEqual([1, 2]);
  });

  it("marks the run completed and stamps completedAt when nextStep is omitted", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 2, status: "done" },
      c,
    );
    const { state } = (res as AdvanceResult).result;
    expect(state.status).toBe("completed");
    expect(state.currentStep).toBe(2);
    expect(state.completedAt).toBeGreaterThan(0);
    expect((res as { summary: string }).summary).toContain("recipe complete");
  });

  it("is idempotent for a repeated advance of the same step (no duplicate entry)", async () => {
    const c = ctx();
    await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2, notes: "first" },
      c,
    );
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2, notes: "second" },
      c,
    );
    const { state } = (res as AdvanceResult).result;
    expect(state.steps).toHaveLength(1);
    // The latest note wins — the entry is replaced, not duplicated.
    expect(state.steps[0].notes).toBe("second");
  });

  it("records a skipped step with its note", async () => {
    const c = ctx();
    const res = await advance.handler(
      {
        recipeId: "r.flow",
        completedStep: 1,
        nextStep: 2,
        status: "skipped",
        notes: "not applicable here",
      },
      c,
    );
    const { state } = (res as AdvanceResult).result;
    expect(state.steps[0].status).toBe("skipped");
    expect(state.steps[0].notes).toBe("not applicable here");
    expect((res as { summary: string }).summary).toContain("skipped");
  });

  it("does not regress currentStep on a stale lower-numbered replay", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    await advance.handler({ recipeId: "r.flow", completedStep: 2, nextStep: 3 }, c);
    // A late replay of an earlier step must not pull the live pointer back.
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2 },
      c,
    );
    expect((res as AdvanceResult).result.state.currentStep).toBe(3);
  });

  it("preserves completedAt when a finished run is touched again by a non-final replay", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    const done = await advance.handler(
      { recipeId: "r.flow", completedStep: 2 },
      c,
    );
    const completedAt = (done as AdvanceResult).result.state.completedAt;
    expect(completedAt).toBeGreaterThan(0);
    // A later non-final advance must not wipe the original completion stamp.
    const replay = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2 },
      c,
    );
    expect((replay as AdvanceResult).result.state.completedAt).toBe(completedAt);
  });

  it("drops corrupted stored step entries and still records the new step", async () => {
    const c = ctx();
    c.db.insertRecipeRunState({
      sessionId: "ses_test",
      recipeId: "r.corrupt",
      currentStep: 1,
      // An invalid status and a non-object entry — both must be tolerated.
      stepsJson: JSON.stringify([{ index: 1, status: "blocked", ts: 1 }, "garbage"]),
    });
    const res = await advance.handler(
      { recipeId: "r.corrupt", completedStep: 2, nextStep: 3 },
      c,
    );
    const { state } = (res as AdvanceResult).result;
    expect(state.steps.map((s) => s.index)).toEqual([2]);
    expect(state.steps[0].status).toBe("done");
  });

  it("keeps separate runs per recipe within the same session", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.a", completedStep: 1, nextStep: 2 }, c);
    await advance.handler({ recipeId: "r.b", completedStep: 1, nextStep: 2 }, c);
    expect(c.db.listRecipeRunStates("ses_test")).toHaveLength(2);
  });

  it("fails cleanly when no session id is bound to the context", async () => {
    const res = await advance.handler(
      { recipeId: "r.flow", completedStep: 1, nextStep: 2 },
      noSessionCtx(),
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_RUN_NO_SESSION");
  });
});

describe("recipe.run.get", () => {
  it("returns the stored checkpoint for an active run", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    const res = await get.handler({ recipeId: "r.flow" }, c);
    expect(res.ok).toBe(true);
    const { state } = (res as GetResult).result;
    expect(state).not.toBeNull();
    expect(state!.currentStep).toBe(2);
    expect(state!.status).toBe("active");
  });

  it("reports completed status and completedAt for a finished run", async () => {
    const c = ctx();
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    await advance.handler({ recipeId: "r.flow", completedStep: 2 }, c);
    const res = await get.handler({ recipeId: "r.flow" }, c);
    const { state } = (res as GetResult).result;
    expect(state).not.toBeNull();
    expect(state!.status).toBe("completed");
    expect(state!.completedAt).toBeGreaterThan(0);
  });

  it("returns ok with a null state (not a failure) when no checkpoint exists", async () => {
    const c = ctx();
    const res = await get.handler({ recipeId: "r.never-started" }, c);
    expect(res.ok).toBe(true);
    expect((res as GetResult).result.state).toBeNull();
    expect(res.summary).toContain("No checkpoint");
  });

  it("isolates checkpoints by session", async () => {
    const c = ctx("ses_one");
    await advance.handler({ recipeId: "r.flow", completedStep: 1, nextStep: 2 }, c);
    // A different session sharing the same DB sees no checkpoint.
    const other = { db: c.db, actor: "main", sessionId: "ses_two" } as unknown as ToolContext;
    const res = await get.handler({ recipeId: "r.flow" }, other);
    expect((res as GetResult).result.state).toBeNull();
  });

  it("fails cleanly when no session id is bound to the context", async () => {
    const res = await get.handler({ recipeId: "r.flow" }, noSessionCtx());
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_RUN_NO_SESSION");
  });
});

describe("recipe.load", () => {
  const recipe = {
    id: "r.known",
    title: "Known Recipe",
    summary: "A recipe that exists in the source.",
  };

  /** A context with a recipe source and a spy loadRecipes callback. */
  function loadCtx(opts: { hasSource?: boolean; hasCallback?: boolean } = {}) {
    const { hasSource = true, hasCallback = true } = opts;
    const loadRecipes = vi.fn((ids: string[]) => ids);
    const recipeSource = {
      has: (id: string) => id === recipe.id,
      get: (id: string) => (id === recipe.id ? (recipe as never) : undefined),
    };
    const c = {
      actor: "main",
      recipeSource: hasSource ? recipeSource : undefined,
      loadRecipes: hasCallback ? loadRecipes : undefined,
    } as unknown as ToolContext;
    return { c, loadRecipes };
  }

  type LoadResult = {
    result: {
      id: string;
      title: string;
      summary: string;
      activeRecipeIds: string[];
    };
  };

  it("loads a known recipe: calls loadRecipes with its id and returns its label", async () => {
    const { c, loadRecipes } = loadCtx();
    const res = await load.handler({ recipeId: "r.known" }, c);
    expect(res.ok).toBe(true);
    expect(loadRecipes).toHaveBeenCalledWith(["r.known"]);
    const { result } = res as LoadResult;
    expect(result.id).toBe("r.known");
    expect(result.title).toBe("Known Recipe");
    expect(result.summary).toBe("A recipe that exists in the source.");
    // The callback echoes the resulting active set back to the tool.
    expect(result.activeRecipeIds).toEqual(["r.known"]);
  });

  it("returns a recoverable RECIPE_NOT_FOUND for an unknown id and never loads", async () => {
    const { c, loadRecipes } = loadCtx();
    const res = await load.handler({ recipeId: "r.missing" }, c);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_NOT_FOUND");
    expect(res.error?.recoverable).toBe(true);
    expect(loadRecipes).not.toHaveBeenCalled();
  });

  it("fails cleanly when no recipe source is bound to the context", async () => {
    const { c } = loadCtx({ hasSource: false });
    const res = await load.handler({ recipeId: "r.known" }, c);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_SOURCE_UNAVAILABLE");
  });

  it("fails cleanly when recipe loading is unavailable (e.g. a watcher context)", async () => {
    const { c } = loadCtx({ hasCallback: false });
    const res = await load.handler({ recipeId: "r.known" }, c);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_LOAD_UNAVAILABLE");
  });
});

describe("recipe.find", () => {
  type FindResult = {
    result: {
      query: string;
      selected: { id: string; title: string; summary: string }[];
      activeRecipeIds: string[];
    };
  };

  /** A context with a spy findRecipes callback returning a canned result. */
  function findCtx(
    findRecipes?: (query: string) => Promise<unknown>,
  ): ToolContext {
    return {
      actor: "main",
      findRecipes: findRecipes
        ? vi.fn((query: string) => findRecipes(query))
        : undefined,
    } as unknown as ToolContext;
  }

  it("loads the matched recipes the selector resolved and reports them", async () => {
    const c = findCtx(async (query) => ({
      ok: true,
      matched: true,
      query,
      reason: "matches the edit flow",
      confidence: 0.9,
      selected: [
        {
          id: "daintree.edits.spawn-visible-agent",
          title: "Spawn a visible agent",
          summary: "short header",
        },
      ],
      activeRecipeIds: ["daintree.edits.spawn-visible-agent"],
    }));
    const res = await find.handler({ query: "how do I edit files" }, c);
    expect(res.ok).toBe(true);
    expect(c.findRecipes).toHaveBeenCalledWith("how do I edit files", undefined);
    const { result } = res as FindResult;
    expect(result.selected.map((r) => r.id)).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
    expect(result.activeRecipeIds).toEqual([
      "daintree.edits.spawn-visible-agent",
    ]);
  });

  it("returns ok with an empty selection (not a failure) when nothing matches", async () => {
    const c = findCtx(async (query) => ({
      ok: true,
      matched: false,
      query,
      reason: "no fit",
      confidence: 0.1,
      selected: [],
      activeRecipeIds: [],
    }));
    const res = await find.handler({ query: "unrelated trivia" }, c);
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("No recipe matched");
    expect((res as FindResult).result.selected).toEqual([]);
  });

  it("returns a recoverable failure when the selector model errored", async () => {
    const c = findCtx(async (query) => ({
      ok: false,
      matched: false,
      query,
      reason: "recipe selector unavailable",
      confidence: 0,
      selected: [],
      activeRecipeIds: [],
    }));
    const res = await find.handler({ query: "anything" }, c);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_FIND_FAILED");
    expect(res.error?.recoverable).toBe(true);
  });

  it("fails cleanly when recipe lookup is unavailable (e.g. a watcher context)", async () => {
    const res = await find.handler({ query: "anything" }, findCtx(undefined));
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("RECIPE_FIND_UNAVAILABLE");
  });
});
