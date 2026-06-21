/**
 * In-memory registry of assistant skills.
 *
 * Seeded from the built-in library; future hosted skills can be added by passing
 * a different initial set. The selector only ever sees metadata (via
 * metadataForSelection) — full skill bodies are loaded only after the small model
 * picks ids, keeping selection cheap and the loaded prompt minimal.
 */
import { Skill, type SkillMetadata } from "./types.js";
import { BUILTIN_SKILLS } from "./builtin.js";

export class SkillRegistry {
  private skills = new Map<string, Skill>();

  constructor(initial: ReadonlyArray<unknown> = BUILTIN_SKILLS) {
    for (const raw of initial) {
      const skill = Skill.parse(raw);
      if (this.skills.has(skill.id)) {
        throw new Error(`Duplicate skill id: ${skill.id}`);
      }
      this.skills.set(skill.id, skill);
    }
  }

  list(): Skill[] {
    return [...this.skills.values()];
  }

  has(id: string): boolean {
    return this.skills.has(id);
  }

  get(id: string): Skill | undefined {
    return this.skills.get(id);
  }

  /** Resolve ids to skills, silently dropping unknown ids. */
  getMany(ids: string[]): Skill[] {
    return ids
      .map((id) => this.skills.get(id))
      .filter((r): r is Skill => r !== undefined);
  }

  /** Metadata-only view for the small-model selector (never includes bodies). */
  metadataForSelection(): SkillMetadata[] {
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
