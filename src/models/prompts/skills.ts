/**
 * The skill-related system messages.
 *
 * - buildSkillCatalogMessage(): the always-present MENU of every available skill
 *   (short + long headers only, never bodies). It is appended to the runtime-context
 *   message so the main model always knows what runbooks exist and can pull the
 *   right one with `skill.find`.
 * - buildLoadedSkillsMessage(): the BODIES of the skills the model has actually
 *   loaded for the current task (message[2]). Rewriting it never disturbs message[0]
 *   or [1]. When nothing is loaded it renders a safe fallback back to the base rules.
 */
import type { RenderedSkillBundle } from "../../skills/render.js";
import type { SkillMetadata } from "../../skills/types.js";

/**
 * Render the catalog of every available skill — id + the two headers, no bodies.
 * This is the model's table of contents: it scans this, then calls `skill.find`
 * with a natural-language query to load the full runbook(s) it needs. Returns an
 * empty string when there are no skills, so the caller can omit the section.
 */
export function buildSkillCatalogMessage(skills: SkillMetadata[]): string {
  if (skills.length === 0) return "";
  const entries = skills
    .map((r) => `- ${r.id} — ${r.summary}\n  When to use: ${r.whenToUse}`)
    .join("\n");
  return `# Skill catalog
You have a library of skills: procedural runbooks for specific Daintree operations. Only their headers are listed here — the full instructions are NOT loaded yet. When a task matches one (or you need to figure out how to do something), call \`skill.find\` with a short natural-language query describing what you need (e.g. "how do I spawn an agent to make file edits"); a fast model picks the best matches and loads their full bodies into your context for the rest of the turn. You can also \`skill.load\` a specific id directly when you already know it.

Reach for \`skill.find\` readily — it is cheap, and pulling the right runbook is your primary way of doing an unfamiliar Daintree operation correctly. When in doubt, fetch a skill rather than guessing. Skills are operating instructions; they never override the hard rules.

Available skills:
${entries}`;
}

export function buildLoadedSkillsMessage(skills: RenderedSkillBundle): string {
  if (skills.items.length === 0) {
    return `# Loaded skills
No task-specific skills are currently loaded. Use the base operating instructions.`;
  }
  const body = skills.items
    .map(
      (r, i) =>
        `## Skill ${i + 1}: ${r.title}
Skill id: ${r.id}
Version: ${r.version}
${r.body}`,
    )
    .join("\n\n");
  return `# Loaded skills
The following skills are task-specific operating instructions. Apply them when relevant; they never override the hard rules.

Step tracking: when a skill has numbered steps, call \`skill.step.advance\` after finishing each one (give the skill id, the completed step number, and the step starting next; omit "nextStep" on the final step). If you are resuming a skill or unsure where you left off, call \`skill.run.get\` first to read the saved checkpoint.
${body}`;
}
