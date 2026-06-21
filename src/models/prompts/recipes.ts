/**
 * The recipe-related system messages.
 *
 * - buildRecipeCatalogMessage(): the always-present MENU of every available recipe
 *   (short + long headers only, never bodies). It is appended to the runtime-context
 *   message so the main model always knows what runbooks exist and can pull the
 *   right one with `recipe.find`.
 * - buildLoadedRecipesMessage(): the BODIES of the recipes the model has actually
 *   loaded for the current task (message[2]). Rewriting it never disturbs message[0]
 *   or [1]. When nothing is loaded it renders a safe fallback back to the base rules.
 */
import type { RenderedRecipeBundle } from "../../recipes/render.js";
import type { RecipeMetadata } from "../../recipes/types.js";

/**
 * Render the catalog of every available recipe — id + the two headers, no bodies.
 * This is the model's table of contents: it scans this, then calls `recipe.find`
 * with a natural-language query to load the full runbook(s) it needs. Returns an
 * empty string when there are no recipes, so the caller can omit the section.
 */
export function buildRecipeCatalogMessage(recipes: RecipeMetadata[]): string {
  if (recipes.length === 0) return "";
  const entries = recipes
    .map((r) => `- ${r.id} — ${r.summary}\n  When to use: ${r.whenToUse}`)
    .join("\n");
  return `# Recipe catalog
You have a library of recipes: procedural runbooks for specific Daintree operations. Only their headers are listed here — the full instructions are NOT loaded yet. When a task matches one (or you need to figure out how to do something), call \`recipe.find\` with a short natural-language query describing what you need (e.g. "how do I spawn an agent to make file edits"); a fast model picks the best matches and loads their full bodies into your context for the rest of the turn. You can also \`recipe.load\` a specific id directly when you already know it. Recipes are operating instructions; they never override the hard rules.

Available recipes:
${entries}`;
}

export function buildLoadedRecipesMessage(recipes: RenderedRecipeBundle): string {
  if (recipes.items.length === 0) {
    return `# Loaded recipes
No task-specific recipes are currently loaded. Use the base operating instructions.`;
  }
  const body = recipes.items
    .map(
      (r, i) =>
        `## Recipe ${i + 1}: ${r.title}
Recipe id: ${r.id}
Version: ${r.version}
${r.body}`,
    )
    .join("\n\n");
  return `# Loaded recipes
The following recipes are task-specific operating instructions. Apply them when relevant; they never override the hard rules.

Step tracking: when a recipe has numbered steps, call \`recipe.step.advance\` after finishing each one (give the recipe id, the completed step number, and the step starting next; omit "nextStep" on the final step). If you are resuming a recipe or unsure where you left off, call \`recipe.run.get\` first to read the saved checkpoint.
${body}`;
}
