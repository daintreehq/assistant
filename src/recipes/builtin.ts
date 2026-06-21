/**
 * The built-in assistant recipe library.
 *
 * Recipes are no longer hard-coded here — they live as content files under the
 * repo-root `recipes/` directory (one markdown file per recipe) and are loaded +
 * validated at import time by `loadRecipesFromDir()`. Authoring a new recipe is
 * "add a file" (see docs/RECIPES.md); this module just exposes the loaded set.
 *
 * `BUILTIN_RECIPES` stays the canonical handle every consumer imports, so the
 * folder swap is invisible to callers. A future hosted recipe service replaces the
 * loader behind the same `RecipeSource` seam (see source.ts) without touching them.
 */
import type { Recipe } from "./types.js";
import { loadRecipesFromDir } from "./fileSource.js";

/** Every recipe found in `recipes/`, validated, sorted by filename. */
export const BUILTIN_RECIPES: Recipe[] = loadRecipesFromDir();

/** Look up a built-in recipe by id, throwing if the expected file is missing. */
function byId(id: string): Recipe {
  const found = BUILTIN_RECIPES.find((r) => r.id === id);
  if (!found) throw new Error(`Built-in recipe '${id}' is missing from recipes/`);
  return found;
}

// Named handles for the recipes that code/tests reference directly. These assert
// the file exists at load time, so a renamed/removed recipe fails fast.
export const BASIC_DAINTREE_ORCHESTRATION_RECIPE = byId(
  "daintree.orchestration.basic",
);
export const SPAWN_AGENT_FOR_EDITS_RECIPE = byId(
  "daintree.edits.spawn-visible-agent",
);
export const DAINTREE_RECIPE_RUNNER_RECIPE = byId(
  "daintree.recipe.run-or-create",
);
export const WORKFLOW_START_WORK_RECIPE = byId(
  "daintree.workflow.start-work-on-issue",
);
export const WORKFLOW_PREP_BRANCH_RECIPE = byId(
  "daintree.workflow.prep-branch-for-review",
);
