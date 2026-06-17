/**
 * Recipe schemas.
 *
 * An **assistant recipe** is a short, procedural runbook injected into the main
 * model's context when it is relevant to the user's current task. Recipes are the
 * behavior layer that replaces fine-tuning: a growing, validated library, selected
 * cheaply by the small model and loaded only when useful.
 *
 * Do not confuse these with **Daintree workspace recipes** (the MCP `recipe.list`
 * / `recipe.run` / `worktree.createWithRecipe` actions). Those are user-facing
 * workspace setups; these are hidden prompt instructions. See docs §16.
 */
import { z } from "zod";

/** Coarse risk of the actions a recipe tends to drive, mirroring tool risk classes. */
export const RecipeRisk = z.enum([
  "read",
  "local",
  "ui",
  "terminal",
  "project",
  "git",
  "external",
  "system",
]);
export type RecipeRisk = z.infer<typeof RecipeRisk>;

export const Recipe = z.object({
  /** Stable, dotted id (e.g. "daintree.edits.spawn-visible-agent"). */
  id: z.string().min(1),
  /** Human-readable title shown in /recipes and the loaded-recipes header. */
  title: z.string().min(1),
  /** Semver-ish version; bump when the body changes so cache hashes shift. */
  version: z.string().min(1),
  /** One-line summary the selector sees as metadata. */
  summary: z.string().min(1),
  /** When to use it — the primary signal for the small-model selector. */
  whenToUse: z.string().min(1),
  /** Free-form tags to bias selection. */
  tags: z.array(z.string()).default([]),
  /** Higher priority recipes win ties when more than three match. */
  priority: z.number().int().default(0),
  /** Soft cap on how many turns a recipe should stay loaded. */
  maxTurns: z.number().int().positive().default(8),
  /** The riskiest action class this recipe tends to drive. */
  risk: RecipeRisk.default("read"),
  /**
   * Tools the recipe needs. When a recipe is active this acts as a per-turn
   * allowlist (unioned with the core tools — see CORE_TOOL_NAMES in
   * agent/loop.ts), so any tool the body instructs the model to use MUST be
   * listed here or the model never sees it. Under-declaring silently starves
   * the model; an empty list means "core tools only".
   */
  requiredTools: z.array(z.string()).default([]),
  /** The actual runbook injected into the main model. Keep it short + procedural. */
  body: z.string().min(1),
});
export type Recipe = z.infer<typeof Recipe>;

/** Just the fields the small-model selector needs to choose recipes (no bodies). */
export const RecipeMetadata = Recipe.pick({
  id: true,
  title: true,
  summary: true,
  whenToUse: true,
  tags: true,
  priority: true,
});
export type RecipeMetadata = z.infer<typeof RecipeMetadata>;

/** The small model's structured selection output. */
export const RecipeSelection = z.object({
  recipeIds: z.array(z.string()).max(3),
  confidence: z.number().min(0).max(1),
  reason: z.string(),
  taskType: z.string(),
  keepExisting: z.boolean().default(false),
});
export type RecipeSelection = z.infer<typeof RecipeSelection>;
