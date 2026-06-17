/**
 * In-memory registry of assistant recipes.
 *
 * Seeded from the built-in library; future hosted recipes can be added by passing
 * a different initial set. The selector only ever sees metadata (via
 * metadataForSelection) — full recipe bodies are loaded only after the small model
 * picks ids, keeping selection cheap and the loaded prompt minimal.
 */
import { Recipe, type RecipeMetadata } from "./types.js";
import { BUILTIN_RECIPES } from "./builtin.js";

export class RecipeRegistry {
  private recipes = new Map<string, Recipe>();

  constructor(initial: ReadonlyArray<unknown> = BUILTIN_RECIPES) {
    for (const raw of initial) {
      const recipe = Recipe.parse(raw);
      if (this.recipes.has(recipe.id)) {
        throw new Error(`Duplicate recipe id: ${recipe.id}`);
      }
      this.recipes.set(recipe.id, recipe);
    }
  }

  list(): Recipe[] {
    return [...this.recipes.values()];
  }

  has(id: string): boolean {
    return this.recipes.has(id);
  }

  get(id: string): Recipe | undefined {
    return this.recipes.get(id);
  }

  /** Resolve ids to recipes, silently dropping unknown ids. */
  getMany(ids: string[]): Recipe[] {
    return ids
      .map((id) => this.recipes.get(id))
      .filter((r): r is Recipe => r !== undefined);
  }

  /** Metadata-only view for the small-model selector (never includes bodies). */
  metadataForSelection(): RecipeMetadata[] {
    return this.list().map(({ id, title, summary, whenToUse, tags, priority }) => ({
      id,
      title,
      summary,
      whenToUse,
      tags,
      priority,
    }));
  }
}
