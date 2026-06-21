/**
 * Skill schemas.
 *
 * An **assistant skill** is a short, procedural runbook injected into the main
 * model's context when it is relevant to the user's current task. Skills are the
 * behavior layer that replaces fine-tuning: a growing, validated library, selected
 * cheaply by the small model and loaded only when useful.
 *
 * Do not confuse these with **Daintree workspace recipes** (the MCP `recipe.list`
 * / `recipe.run` / `worktree.createWithRecipe` actions). Those are user-facing
 * workspace setups; these are hidden prompt instructions. See docs §16.
 */
import { z } from "zod";

/** Coarse risk of the actions a skill tends to drive, mirroring tool risk classes. */
export const SkillRisk = z.enum([
  "read",
  "local",
  "ui",
  "terminal",
  "project",
  "git",
  "external",
  "system",
]);
export type SkillRisk = z.infer<typeof SkillRisk>;

export const Skill = z.object({
  /** Stable, dotted id (e.g. "daintree.edits.spawn-visible-agent"). */
  id: z.string().min(1),
  /** Human-readable title shown in /skills and the loaded-skills header. */
  title: z.string().min(1),
  /** Semver-ish version; bump when the body changes so cache hashes shift. */
  version: z.string().min(1),
  /**
   * The SHORT header (~8-10 words): a one-line "what this skill does" the main
   * model reads in the skill catalog and the selector sees as metadata. Keep it
   * terse and scannable — it sits next to every other skill in the catalog.
   */
  summary: z.string().min(1),
  /**
   * The LONG header (1-2 sentences): the detailed "when to reach for this" signal.
   * This is the primary thing the small-model selector matches a query against, so
   * spell out the situations/verbs that should trigger the skill.
   */
  whenToUse: z.string().min(1),
  /** Free-form tags to bias selection. */
  tags: z.array(z.string()).default([]),
  /** Higher priority skills win ties when more than three match. */
  priority: z.number().int().default(0),
  /** Soft cap on how many turns a skill should stay loaded. */
  maxTurns: z.number().int().positive().default(8),
  /** The riskiest action class this skill tends to drive. */
  risk: SkillRisk.default("read"),
  /**
   * Tools the skill needs. When a skill is active this acts as a per-turn
   * allowlist (unioned with the core tools — see CORE_TOOL_NAMES in
   * agent/loop.ts), so any tool the body instructs the model to use MUST be
   * listed here or the model never sees it. Under-declaring silently starves
   * the model; an empty list means "core tools only".
   */
  requiredTools: z.array(z.string()).default([]),
  /** The actual runbook injected into the main model. Keep it short + procedural. */
  body: z.string().min(1),
});
export type Skill = z.infer<typeof Skill>;

/** Just the fields the small-model selector needs to choose skills (no bodies). */
export const SkillMetadata = Skill.pick({
  id: true,
  title: true,
  summary: true,
  whenToUse: true,
  tags: true,
  priority: true,
});
export type SkillMetadata = z.infer<typeof SkillMetadata>;

/**
 * The small model's structured selection output for a `skill.find` query. The
 * selector reads the query + every skill's headers and returns the 0-3 skills
 * whose bodies should be loaded into the main model's context.
 */
export const SkillSelection = z.object({
  skillIds: z.array(z.string()).max(3),
  confidence: z.number().min(0).max(1),
  reason: z.string(),
  taskType: z.string(),
});
export type SkillSelection = z.infer<typeof SkillSelection>;

/** Result of a `skill.find` query: what the small model resolved and loaded. */
export interface SkillFindResult {
  /** False only when the selector model errored (loaded set left unchanged). */
  ok: boolean;
  /** Whether the query resolved to at least one skill. */
  matched: boolean;
  /** The query that was run (echoed back for the tool result). */
  query: string;
  /** The selector's one-line rationale. */
  reason: string;
  /** The selector's confidence in [0, 1]. */
  confidence: number;
  /** The skills this query pulled in (headers only — bodies go to the context). */
  selected: { id: string; title: string; summary: string }[];
  /** The full loaded skill id set after this find. */
  activeSkillIds: string[];
}
