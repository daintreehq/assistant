/**
 * The loaded-recipes system message — message[2].
 *
 * Holds the bodies of the recipes the small model selected for the current task.
 * Rewriting this message does not disturb message[0] or message[1]. When no recipe
 * is loaded it renders a safe fallback that points the model back at the base rules.
 */
import type { RenderedRecipeBundle } from "../../recipes/render.js";

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
