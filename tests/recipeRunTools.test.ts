import { describe, it, expect, vi, afterEach } from "vitest";
import { recipeRunTools } from "../src/tools/recipeRunTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const advance = recipeRunTools.find((t) => t.name === "recipe.step.advance")!;
const get = recipeRunTools.find((t) => t.name === "recipe.run.get")!;

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
