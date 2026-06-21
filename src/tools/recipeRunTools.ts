/**
 * Recipe step-progress tools. Recipes are stateless prompt injections; these two
 * tools add a thin, durable layer of step-level progress on top — so a multi-step
 * recipe can be supervised as it runs and resumed where it left off if a turn is
 * interrupted (see issue #54).
 *
 *   - `recipe.step.advance` records that a numbered step finished and which step
 *     is now live. It upserts one `recipe_run_state` row keyed by the live
 *     (session, recipe) pair; risk "local" — daemon state only, never files.
 *   - `recipe.run.get` reads that checkpoint back (read-only) so the model can
 *     recover its place after an auto-compact or a fresh turn.
 *
 * Progress reaches the model the cheap way: as the tool result at the tail of the
 * conversation. These tools never touch message[0]/[1]/[2], so the cached prompt
 * prefix is undisturbed.
 */
import { z } from "zod";
import { ok, fail, type ToolContext, type ToolDef } from "./types.js";
import type {
  RecipeRunStateRecord,
  RecipeStepProgress,
  RecipeStepStatus,
} from "../schemas.js";

const StepStatusEnum = z.enum(["done", "skipped"]);

const AdvanceArgs = z.object({
  recipeId: z
    .string()
    .min(1)
    .describe("Id of the active recipe (the 'Recipe id:' shown in the loaded recipe)."),
  completedStep: z
    .number()
    .int()
    .min(1)
    .describe("The numbered step just finished (1-based)."),
  nextStep: z
    .number()
    .int()
    .min(1)
    .optional()
    .describe("The step starting next (1-based). Omit when the recipe is finished."),
  status: StepStatusEnum.default("done").describe(
    "Outcome of the completed step (default 'done').",
  ),
  notes: z
    .string()
    .optional()
    .describe("Optional brief checkpoint note for the completed step."),
});

const GetArgs = z.object({
  recipeId: z
    .string()
    .min(1)
    .describe("Id of the recipe whose checkpoint to read."),
});

const LoadArgs = z.object({
  recipeId: z
    .string()
    .trim()
    .min(1)
    .describe(
      "Id of the recipe to load (the stable dotted id, e.g. 'daintree.edits.spawn-visible-agent'). Use tool.search or the loaded-recipes header to discover ids.",
    ),
});

const FindArgs = z.object({
  query: z
    .string()
    .trim()
    .min(1)
    .describe(
      "A short natural-language description of what you need to figure out (e.g. 'how do I spawn an agent to edit files', 'start work on a forge issue'). A fast model matches it against every recipe and loads the best matches' full bodies into your context.",
    ),
});

/** Parse the stored step array, tolerating null/garbage. */
function parseSteps(s: string | undefined): RecipeStepProgress[] {
  if (!s) return [];
  try {
    const v = JSON.parse(s);
    if (!Array.isArray(v)) return [];
    return v
      .filter(
        (e): e is RecipeStepProgress =>
          e &&
          typeof e === "object" &&
          typeof e.index === "number" &&
          (e.status === "done" || e.status === "skipped"),
      )
      .map((e) => ({
        index: e.index,
        status: e.status as RecipeStepStatus,
        notes: typeof e.notes === "string" ? e.notes : undefined,
        ts: typeof e.ts === "number" ? e.ts : 0,
      }));
  } catch {
    return [];
  }
}

/** Insert or replace the entry for `index`, keeping the array sorted by step. */
function upsertStep(
  steps: RecipeStepProgress[],
  entry: RecipeStepProgress,
): RecipeStepProgress[] {
  const next = steps.filter((e) => e.index !== entry.index);
  next.push(entry);
  next.sort((a, b) => a.index - b.index);
  return next;
}

/** Deserialize a stored record into a model-friendly view (parsed step array). */
function toView(rec: RecipeRunStateRecord) {
  return {
    id: rec.id,
    sessionId: rec.sessionId,
    recipeId: rec.recipeId,
    currentStep: rec.currentStep,
    status: rec.status,
    steps: parseSteps(rec.stepsJson),
    startedAt: rec.startedAt,
    updatedAt: rec.updatedAt,
    completedAt: rec.completedAt,
  };
}

/** The session id, or undefined when running in a stripped-down context. */
function sessionOf(ctx: ToolContext): string | undefined {
  return ctx.sessionId && ctx.sessionId.trim() ? ctx.sessionId : undefined;
}

export const recipeRunTools: ToolDef[] = [
  {
    name: "recipe.step.advance",
    description:
      "Record progress through a multi-step recipe: mark the numbered step you just finished and name the one starting next (omit it when the recipe is done). Call once per step as you work the loaded recipe's runbook. Durable daemon state only; never edits files.",
    risk: "local",
    schema: AdvanceArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        recipeId: {
          type: "string",
          description:
            "Id of the active recipe (the 'Recipe id:' shown in the loaded recipe).",
        },
        completedStep: {
          type: "number",
          description: "The numbered step just finished (1-based).",
        },
        nextStep: {
          type: "number",
          description:
            "The step starting next (1-based). Omit when the recipe is finished.",
        },
        status: {
          type: "string",
          enum: ["done", "skipped"],
          description: "Outcome of the completed step (default 'done').",
        },
        notes: {
          type: "string",
          description: "Optional brief checkpoint note for the completed step.",
        },
      },
      required: ["recipeId", "completedStep"],
    },
    async handler(args: z.infer<typeof AdvanceArgs>, ctx) {
      const sessionId = sessionOf(ctx);
      if (!sessionId) {
        return fail(
          "RECIPE_RUN_NO_SESSION",
          "No session id is bound to this context, so recipe progress cannot be tracked.",
          { recoverable: false },
        );
      }
      try {
        const now = Date.now();
        // Default defensively rather than relying on the Zod default having run —
        // the registry applies it in production, but a direct handler call may not.
        const stepStatus: RecipeStepStatus = args.status ?? "done";
        // Closing the recipe out: no next step means the run is complete and
        // `currentStep` rests on the final step the model reported.
        const finished = args.nextStep === undefined;
        const existing = ctx.db.getRecipeRunState(sessionId, args.recipeId);
        const steps = upsertStep(parseSteps(existing?.stepsJson), {
          index: args.completedStep,
          status: stepStatus,
          notes: args.notes,
          ts: now,
        });
        // A stale, lower-numbered replay must not regress the live-step pointer:
        // clamp the next step to what's already been reached. When finishing,
        // currentStep rests on the final step the model reported.
        const currentStep = finished
          ? args.completedStep
          : Math.max(args.nextStep!, existing?.currentStep ?? 0);
        const stepsJson = JSON.stringify(steps);

        let rec: RecipeRunStateRecord;
        if (existing) {
          // Build the patch conditionally: an explicit `completedAt: undefined`
          // would make applyUpdate write SQL NULL (undefined keys are still
          // enumerable), wiping the stamp on a non-final replay of a finished
          // run. Only touch completedAt when the run is actually finishing.
          const patch: Partial<RecipeRunStateRecord> = {
            currentStep,
            stepsJson,
            status: finished ? "completed" : "active",
          };
          // Stamp completedAt the first time the run finishes; preserve the
          // original stamp if it completed once and is being touched again.
          if (finished) patch.completedAt = existing.completedAt ?? now;
          ctx.db.updateRecipeRunState(existing.id, patch);
          rec = ctx.db.getRecipeRunState(sessionId, args.recipeId)!;
        } else {
          rec = ctx.db.insertRecipeRunState({
            sessionId,
            recipeId: args.recipeId,
            currentStep,
            stepsJson,
            status: finished ? "completed" : "active",
            startedAt: now,
            completedAt: finished ? now : undefined,
          });
        }

        const verb = stepStatus === "skipped" ? "skipped" : "done";
        const tail = finished
          ? "recipe complete"
          : `step ${currentStep} active`;
        return ok(
          `Recipe ${args.recipeId}: step ${args.completedStep} ${verb} → ${tail} (${steps.length} step(s) recorded).`,
          { state: toView(rec) },
        );
      } catch (e) {
        return fail(
          "RECIPE_STEP_ADVANCE",
          `Could not record recipe step: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "recipe.run.get",
    description:
      "Read the step-level checkpoint for a recipe in this session — which step is live, which steps are done/skipped. Call at the start of a loaded-recipe turn to recover your place after an interruption. Read-only.",
    risk: "read",
    schema: GetArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        recipeId: {
          type: "string",
          description: "Id of the recipe whose checkpoint to read.",
        },
      },
      required: ["recipeId"],
    },
    async handler(args: z.infer<typeof GetArgs>, ctx) {
      const sessionId = sessionOf(ctx);
      if (!sessionId) {
        return fail(
          "RECIPE_RUN_NO_SESSION",
          "No session id is bound to this context, so recipe progress cannot be read.",
          { recoverable: false },
        );
      }
      const rec = ctx.db.getRecipeRunState(sessionId, args.recipeId);
      if (!rec) {
        // Absence is a normal answer (recipe not yet started), not an error.
        return ok(`No checkpoint for recipe ${args.recipeId} in this session.`, {
          state: null,
        });
      }
      const where =
        rec.status === "completed"
          ? "complete"
          : `at step ${rec.currentStep}`;
      return ok(`Recipe ${args.recipeId}: ${rec.status} (${where}).`, {
        state: toView(rec),
      });
    },
  },
  {
    name: "recipe.find",
    description:
      "Figure out how to do a Daintree operation by pulling in the right runbook. Pass a natural-language query describing what you need; a fast model matches it against the recipe catalog and loads the best 0-3 recipes' full bodies into your context for the rest of this turn. Use this whenever a task matches a catalog entry and you don't already have the runbook loaded. Read-only — selects and injects recipes, never edits files.",
    risk: "read",
    schema: FindArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        query: {
          type: "string",
          description:
            "A short natural-language description of what you need to figure out (e.g. 'how do I spawn an agent to edit files'). Matched against every recipe; the best matches are loaded into your context.",
        },
      },
      required: ["query"],
    },
    async handler(args: z.infer<typeof FindArgs>, ctx) {
      // findRecipes runs the selection + context rewrite; wired only for the
      // interactive main actor (watcher/timer turns have no live recipe set).
      if (!ctx.findRecipes) {
        return fail(
          "RECIPE_FIND_UNAVAILABLE",
          "Recipe lookup is not available in this context.",
          { recoverable: false },
        );
      }
      const result = await ctx.findRecipes(args.query, ctx.signal);
      if (!result.ok) {
        // The selector model failed — recoverable, the model can retry or proceed.
        return fail(
          "RECIPE_FIND_FAILED",
          "The recipe selector was unavailable; no recipes were loaded.",
          { recoverable: true },
        );
      }
      if (!result.matched) {
        // A genuine "nothing fits" is a normal answer, not an error: tell the model
        // to fall back to its base operating instructions.
        return ok(
          `No recipe matched "${args.query}". Use your base operating instructions.`,
          { query: args.query, selected: [], activeRecipeIds: result.activeRecipeIds },
        );
      }
      const labels = result.selected.map((r) => `${r.id} (${r.title})`).join(", ");
      return ok(
        `Loaded ${result.selected.length} recipe(s) for "${args.query}": ${labels}. Their full instructions are now in your context.`,
        {
          query: args.query,
          selected: result.selected,
          reason: result.reason,
          activeRecipeIds: result.activeRecipeIds,
        },
      );
    },
  },
  {
    name: "recipe.load",
    description:
      "Load a specific recipe (procedural runbook) by id into your context right now, when you already know which runbook you need (e.g. from the recipe catalog). The recipe's body becomes available to you on your next step this turn. The loaded set is capped; an explicit load takes priority. Prefer `recipe.find` when you only know what you need in words, not the exact id. Read-only — pulls the recipe into context, never edits files.",
    risk: "read",
    schema: LoadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        recipeId: {
          type: "string",
          description:
            "Id of the recipe to load (the stable dotted id, e.g. 'daintree.edits.spawn-visible-agent'). Use tool.search or the loaded-recipes header to discover ids.",
        },
      },
      required: ["recipeId"],
    },
    async handler(args: z.infer<typeof LoadArgs>, ctx) {
      // The registry view is needed to validate the id and read back a label;
      // absent only in stripped-down contexts that don't wire the recipe seam.
      if (!ctx.recipeSource) {
        return fail(
          "RECIPE_SOURCE_UNAVAILABLE",
          "No recipe source is bound to this context, so recipes cannot be loaded here.",
          { recoverable: false },
        );
      }
      const recipe = ctx.recipeSource.get(args.recipeId);
      if (!recipe) {
        // A wrong id is recoverable: the model can retry with a valid one.
        return fail(
          "RECIPE_NOT_FOUND",
          `No recipe with id '${args.recipeId}' is registered. Use tool.search to find a valid recipe id.`,
          { recoverable: true },
        );
      }
      // loadRecipes performs the actual context rewrite; it's wired only for the
      // interactive main actor (watcher/timer turns have no live recipe set).
      if (!ctx.loadRecipes) {
        return fail(
          "RECIPE_LOAD_UNAVAILABLE",
          "Recipe loading is not available in this context.",
          { recoverable: false },
        );
      }
      const activeRecipeIds = ctx.loadRecipes([recipe.id]);
      return ok(`Recipe ${recipe.id} loaded.`, {
        id: recipe.id,
        title: recipe.title,
        summary: recipe.summary,
        activeRecipeIds,
      });
    },
  },
];
