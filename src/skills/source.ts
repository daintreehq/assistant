/**
 * The swappable seam between skill consumers and where skills come from.
 *
 * Today the only source is the in-memory `SkillRegistry` (seeded from the
 * built-in library); a future hosted skill service can satisfy the same shape
 * without touching callers. The model-facing `skill.load` tool depends on this
 * narrow interface — just enough to validate an id and read a skill back — so
 * swapping the backing store later is a one-line wiring change, not a refactor.
 *
 * `SkillRegistry` already satisfies this structurally; the interface exists to
 * pin the contract callers are allowed to rely on, not to add a new class.
 */
import type { Skill } from "./types.js";

export interface SkillSource {
  /** Whether a skill with this id exists in the source. */
  has(id: string): boolean;
  /** The full skill for this id, or undefined when unknown. */
  get(id: string): Skill | undefined;
}
