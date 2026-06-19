/**
 * The swappable seam between recipe consumers and where recipes come from.
 *
 * Today the only source is the in-memory `RecipeRegistry` (seeded from the
 * built-in library); a future hosted recipe service can satisfy the same shape
 * without touching callers. The model-facing `recipe.load` tool depends on this
 * narrow interface — just enough to validate an id and read a recipe back — so
 * swapping the backing store later is a one-line wiring change, not a refactor.
 *
 * `RecipeRegistry` already satisfies this structurally; the interface exists to
 * pin the contract callers are allowed to rely on, not to add a new class.
 */
import type { Recipe } from "./types.js";

export interface RecipeSource {
  /** Whether a recipe with this id exists in the source. */
  has(id: string): boolean;
  /** The full recipe for this id, or undefined when unknown. */
  get(id: string): Recipe | undefined;
}
