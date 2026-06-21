/**
 * The built-in assistant skill library.
 *
 * Skills are no longer hard-coded here — they live as content files under the
 * repo-root `skills/` directory (one markdown file per skill) and are loaded +
 * validated at import time by `loadSkillsFromDir()`. Authoring a new skill is
 * "add a file" (see docs/SKILLS.md); this module just exposes the loaded set.
 *
 * `BUILTIN_SKILLS` stays the canonical handle every consumer imports, so the
 * folder swap is invisible to callers. A future hosted skill service replaces the
 * loader behind the same `SkillSource` seam (see source.ts) without touching them.
 */
import type { Skill } from "./types.js";
import { loadSkillsFromDir } from "./fileSource.js";

/** Every skill found in `skills/`, validated, sorted by filename. */
export const BUILTIN_SKILLS: Skill[] = loadSkillsFromDir();

/** Look up a built-in skill by id, throwing if the expected file is missing. */
function byId(id: string): Skill {
  const found = BUILTIN_SKILLS.find((r) => r.id === id);
  if (!found) throw new Error(`Built-in skill '${id}' is missing from skills/`);
  return found;
}

// Named handles for the skills that code/tests reference directly. These assert
// the file exists at load time, so a renamed/removed skill fails fast.
export const BASIC_DAINTREE_ORCHESTRATION_SKILL = byId(
  "daintree.orchestration.basic",
);
export const SPAWN_AGENT_FOR_EDITS_SKILL = byId(
  "daintree.edits.spawn-visible-agent",
);
export const DAINTREE_RECIPE_RUNNER_SKILL = byId(
  "daintree.recipe.run-or-create",
);
export const WORKFLOW_START_WORK_SKILL = byId(
  "daintree.workflow.start-work-on-issue",
);
export const WORKFLOW_PREP_BRANCH_SKILL = byId(
  "daintree.workflow.prep-branch-for-review",
);
