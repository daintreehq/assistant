/**
 * Render a set of selected skills into a stable bundle.
 *
 * Skills are sorted by id and hashed by `id@version` so the same selection always
 * produces the same bundle and the same cache hash. The hash is useful for logging
 * and debugging; the actual Fireworks prompt_cache_key stays stable (see docs §6)
 * so changing which skills are loaded does not churn the cache prefix.
 */
import crypto from "node:crypto";
import type { Skill } from "./types.js";

export interface RenderedSkillBundle {
  /** Selected skill ids, sorted. */
  ids: string[];
  /** 12-char content hash over `id@version|...`. */
  hash: string;
  /** Debug/log cache key derived from the hash. */
  cacheKey: string;
  /** The selected skills, sorted by id (bodies included). */
  items: Skill[];
}

export function renderSkillBundle(skills: Skill[]): RenderedSkillBundle {
  const sorted = [...skills].sort((a, b) => a.id.localeCompare(b.id));
  const signature = sorted.map((r) => `${r.id}@${r.version}`).join("|");
  const hash = crypto
    .createHash("sha256")
    .update(signature)
    .digest("hex")
    .slice(0, 12);
  return {
    ids: sorted.map((r) => r.id),
    hash,
    cacheKey: `daintree-main-v1-skills-${hash}`,
    items: sorted,
  };
}
