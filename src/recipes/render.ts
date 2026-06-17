/**
 * Render a set of selected recipes into a stable bundle.
 *
 * Recipes are sorted by id and hashed by `id@version` so the same selection always
 * produces the same bundle and the same cache hash. The hash is useful for logging
 * and debugging; the actual Fireworks prompt_cache_key stays stable (see docs §6)
 * so changing which recipes are loaded does not churn the cache prefix.
 */
import crypto from "node:crypto";
import type { Recipe } from "./types.js";

export interface RenderedRecipeBundle {
  /** Selected recipe ids, sorted. */
  ids: string[];
  /** 12-char content hash over `id@version|...`. */
  hash: string;
  /** Debug/log cache key derived from the hash. */
  cacheKey: string;
  /** The selected recipes, sorted by id (bodies included). */
  items: Recipe[];
}

export function renderRecipeBundle(recipes: Recipe[]): RenderedRecipeBundle {
  const sorted = [...recipes].sort((a, b) => a.id.localeCompare(b.id));
  const signature = sorted.map((r) => `${r.id}@${r.version}`).join("|");
  const hash = crypto
    .createHash("sha256")
    .update(signature)
    .digest("hex")
    .slice(0, 12);
  return {
    ids: sorted.map((r) => r.id),
    hash,
    cacheKey: `daintree-main-v1-recipes-${hash}`,
    items: sorted,
  };
}
