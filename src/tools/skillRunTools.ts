/**
 * Skill step-progress tools. Skills are stateless prompt injections; these two
 * tools add a thin, durable layer of step-level progress on top — so a multi-step
 * skill can be supervised as it runs and resumed where it left off if a turn is
 * interrupted (see issue #54).
 *
 *   - `skill.step.advance` records that a numbered step finished and which step
 *     is now live. It upserts one `skill_run_state` row keyed by the live
 *     (session, skill) pair; risk "local" — daemon state only, never files.
 *   - `skill.run.get` reads that checkpoint back (read-only) so the model can
 *     recover its place after an auto-compact or a fresh turn.
 *
 * Progress reaches the model the cheap way: as the tool result at the tail of the
 * conversation. These tools never touch message[0]/[1]/[2], so the cached prompt
 * prefix is undisturbed.
 */
import { z } from "zod";
import { ok, fail, type ToolContext, type ToolDef } from "./types.js";
import type {
  SkillRunStateRecord,
  SkillStepProgress,
  SkillStepStatus,
} from "../schemas.js";

const StepStatusEnum = z.enum(["done", "skipped"]);

const AdvanceArgs = z.object({
  skillId: z
    .string()
    .min(1)
    .describe("Id of the active skill (the 'Skill id:' shown in the loaded skill)."),
  completedStep: z
    .number()
    .int()
    .min(1)
    .describe("The numbered step just finished (1-based)."),
  nextStep: z
    .number()
    .int()
    .min(1)
    .optional()
    .describe("The step starting next (1-based). Omit when the skill is finished."),
  status: StepStatusEnum.default("done").describe(
    "Outcome of the completed step (default 'done').",
  ),
  notes: z
    .string()
    .optional()
    .describe("Optional brief checkpoint note for the completed step."),
});

const GetArgs = z.object({
  skillId: z
    .string()
    .min(1)
    .describe("Id of the skill whose checkpoint to read."),
});

const LoadArgs = z.object({
  skillId: z
    .string()
    .trim()
    .min(1)
    .describe(
      "Id of the skill to load (the stable dotted id, e.g. 'daintree.edits.spawn-visible-agent'). Use tool.search or the loaded-skills header to discover ids.",
    ),
});

const FindArgs = z.object({
  query: z
    .string()
    .trim()
    .min(1)
    .describe(
      "A short natural-language description of what you need to figure out (e.g. 'how do I spawn an agent to edit files', 'start work on a forge issue'). A fast model matches it against every skill and loads the best matches' full bodies into your context.",
    ),
});

/** Parse the stored step array, tolerating null/garbage. */
function parseSteps(s: string | undefined): SkillStepProgress[] {
  if (!s) return [];
  try {
    const v = JSON.parse(s);
    if (!Array.isArray(v)) return [];
    return v
      .filter(
        (e): e is SkillStepProgress =>
          e &&
          typeof e === "object" &&
          typeof e.index === "number" &&
          (e.status === "done" || e.status === "skipped"),
      )
      .map((e) => ({
        index: e.index,
        status: e.status as SkillStepStatus,
        notes: typeof e.notes === "string" ? e.notes : undefined,
        ts: typeof e.ts === "number" ? e.ts : 0,
      }));
  } catch {
    return [];
  }
}

/** Insert or replace the entry for `index`, keeping the array sorted by step. */
function upsertStep(
  steps: SkillStepProgress[],
  entry: SkillStepProgress,
): SkillStepProgress[] {
  const next = steps.filter((e) => e.index !== entry.index);
  next.push(entry);
  next.sort((a, b) => a.index - b.index);
  return next;
}

/** Deserialize a stored record into a model-friendly view (parsed step array). */
function toView(rec: SkillRunStateRecord) {
  return {
    id: rec.id,
    sessionId: rec.sessionId,
    skillId: rec.skillId,
    currentStep: rec.currentStep,
    status: rec.status,
    steps: parseSteps(rec.stepsJson),
    startedAt: rec.startedAt,
    updatedAt: rec.updatedAt,
    completedAt: rec.completedAt,
  };
}

/** The session id, or undefined when running in a stripped-down context. */
function sessionOf(ctx: ToolContext): string | undefined {
  return ctx.sessionId && ctx.sessionId.trim() ? ctx.sessionId : undefined;
}

export const skillRunTools: ToolDef[] = [
  {
    name: "skill.step.advance",
    description:
      "Record progress through a multi-step skill: mark the numbered step you just finished and name the one starting next (omit it when the skill is done). Call once per step as you work the loaded skill's runbook. Durable daemon state only; never edits files.",
    risk: "local",
    schema: AdvanceArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        skillId: {
          type: "string",
          description:
            "Id of the active skill (the 'Skill id:' shown in the loaded skill).",
        },
        completedStep: {
          type: "number",
          description: "The numbered step just finished (1-based).",
        },
        nextStep: {
          type: "number",
          description:
            "The step starting next (1-based). Omit when the skill is finished.",
        },
        status: {
          type: "string",
          enum: ["done", "skipped"],
          description: "Outcome of the completed step (default 'done').",
        },
        notes: {
          type: "string",
          description: "Optional brief checkpoint note for the completed step.",
        },
      },
      required: ["skillId", "completedStep"],
    },
    async handler(args: z.infer<typeof AdvanceArgs>, ctx) {
      const sessionId = sessionOf(ctx);
      if (!sessionId) {
        return fail(
          "SKILL_RUN_NO_SESSION",
          "No session id is bound to this context, so skill progress cannot be tracked.",
          { recoverable: false },
        );
      }
      try {
        const now = Date.now();
        // Default defensively rather than relying on the Zod default having run —
        // the registry applies it in production, but a direct handler call may not.
        const stepStatus: SkillStepStatus = args.status ?? "done";
        // Closing the skill out: no next step means the run is complete and
        // `currentStep` rests on the final step the model reported.
        const finished = args.nextStep === undefined;
        const existing = ctx.db.getSkillRunState(sessionId, args.skillId);
        const steps = upsertStep(parseSteps(existing?.stepsJson), {
          index: args.completedStep,
          status: stepStatus,
          notes: args.notes,
          ts: now,
        });
        // A stale, lower-numbered replay must not regress the live-step pointer:
        // clamp the next step to what's already been reached. When finishing,
        // currentStep rests on the final step the model reported.
        const currentStep = finished
          ? args.completedStep
          : Math.max(args.nextStep!, existing?.currentStep ?? 0);
        const stepsJson = JSON.stringify(steps);

        let rec: SkillRunStateRecord;
        if (existing) {
          // Build the patch conditionally: an explicit `completedAt: undefined`
          // would make applyUpdate write SQL NULL (undefined keys are still
          // enumerable), wiping the stamp on a non-final replay of a finished
          // run. Only touch completedAt when the run is actually finishing.
          const patch: Partial<SkillRunStateRecord> = {
            currentStep,
            stepsJson,
            status: finished ? "completed" : "active",
          };
          // Stamp completedAt the first time the run finishes; preserve the
          // original stamp if it completed once and is being touched again.
          if (finished) patch.completedAt = existing.completedAt ?? now;
          ctx.db.updateSkillRunState(existing.id, patch);
          rec = ctx.db.getSkillRunState(sessionId, args.skillId)!;
        } else {
          rec = ctx.db.insertSkillRunState({
            sessionId,
            skillId: args.skillId,
            currentStep,
            stepsJson,
            status: finished ? "completed" : "active",
            startedAt: now,
            completedAt: finished ? now : undefined,
          });
        }

        const verb = stepStatus === "skipped" ? "skipped" : "done";
        const tail = finished
          ? "skill complete"
          : `step ${currentStep} active`;
        return ok(
          `Skill ${args.skillId}: step ${args.completedStep} ${verb} → ${tail} (${steps.length} step(s) recorded).`,
          { state: toView(rec) },
        );
      } catch (e) {
        return fail(
          "SKILL_STEP_ADVANCE",
          `Could not record skill step: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "skill.run.get",
    description:
      "Read the step-level checkpoint for a skill in this session — which step is live, which steps are done/skipped. Call at the start of a loaded-skill turn to recover your place after an interruption. Read-only.",
    risk: "read",
    schema: GetArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        skillId: {
          type: "string",
          description: "Id of the skill whose checkpoint to read.",
        },
      },
      required: ["skillId"],
    },
    async handler(args: z.infer<typeof GetArgs>, ctx) {
      const sessionId = sessionOf(ctx);
      if (!sessionId) {
        return fail(
          "SKILL_RUN_NO_SESSION",
          "No session id is bound to this context, so skill progress cannot be read.",
          { recoverable: false },
        );
      }
      const rec = ctx.db.getSkillRunState(sessionId, args.skillId);
      if (!rec) {
        // Absence is a normal answer (skill not yet started), not an error.
        return ok(`No checkpoint for skill ${args.skillId} in this session.`, {
          state: null,
        });
      }
      const where =
        rec.status === "completed"
          ? "complete"
          : `at step ${rec.currentStep}`;
      return ok(`Skill ${args.skillId}: ${rec.status} (${where}).`, {
        state: toView(rec),
      });
    },
  },
  {
    name: "skill.find",
    description:
      "Figure out how to do a Daintree operation by pulling in the right runbook. Pass a natural-language query describing what you need; a fast model matches it against the skill catalog and loads the best 0-3 skills' full bodies into your context for the rest of this turn. Use this whenever a task matches a catalog entry and you don't already have the runbook loaded. Read-only — selects and injects skills, never edits files.",
    risk: "read",
    schema: FindArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        query: {
          type: "string",
          description:
            "A short natural-language description of what you need to figure out (e.g. 'how do I spawn an agent to edit files'). Matched against every skill; the best matches are loaded into your context.",
        },
      },
      required: ["query"],
    },
    async handler(args: z.infer<typeof FindArgs>, ctx) {
      // findSkills runs the selection + context rewrite; wired only for the
      // interactive main actor (watcher/timer turns have no live skill set).
      if (!ctx.findSkills) {
        return fail(
          "SKILL_FIND_UNAVAILABLE",
          "Skill lookup is not available in this context.",
          { recoverable: false },
        );
      }
      const result = await ctx.findSkills(args.query, ctx.signal);
      if (!result.ok) {
        // The selector model failed — recoverable, the model can retry or proceed.
        return fail(
          "SKILL_FIND_FAILED",
          "The skill selector was unavailable; no skills were loaded.",
          { recoverable: true },
        );
      }
      if (!result.matched) {
        // A genuine "nothing fits" is a normal answer, not an error: tell the model
        // to fall back to its base operating instructions.
        return ok(
          `No skill matched "${args.query}". Use your base operating instructions.`,
          { query: args.query, selected: [], activeSkillIds: result.activeSkillIds },
        );
      }
      const labels = result.selected.map((r) => `${r.id} (${r.title})`).join(", ");
      return ok(
        `Loaded ${result.selected.length} skill(s) for "${args.query}": ${labels}. Their full instructions are now in your context.`,
        {
          query: args.query,
          selected: result.selected,
          reason: result.reason,
          activeSkillIds: result.activeSkillIds,
        },
      );
    },
  },
  {
    name: "skill.load",
    description:
      "Load a specific skill (procedural runbook) by id into your context right now, when you already know which runbook you need (e.g. from the skill catalog). The skill's body becomes available to you on your next step this turn. The loaded set is capped; an explicit load takes priority. Prefer `skill.find` when you only know what you need in words, not the exact id. Read-only — pulls the skill into context, never edits files.",
    risk: "read",
    schema: LoadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        skillId: {
          type: "string",
          description:
            "Id of the skill to load (the stable dotted id, e.g. 'daintree.edits.spawn-visible-agent'). Use tool.search or the loaded-skills header to discover ids.",
        },
      },
      required: ["skillId"],
    },
    async handler(args: z.infer<typeof LoadArgs>, ctx) {
      // The registry view is needed to validate the id and read back a label;
      // absent only in stripped-down contexts that don't wire the skill seam.
      if (!ctx.skillSource) {
        return fail(
          "SKILL_SOURCE_UNAVAILABLE",
          "No skill source is bound to this context, so skills cannot be loaded here.",
          { recoverable: false },
        );
      }
      const skill = ctx.skillSource.get(args.skillId);
      if (!skill) {
        // A wrong id is recoverable: the model can retry with a valid one.
        return fail(
          "SKILL_NOT_FOUND",
          `No skill with id '${args.skillId}' is registered. Use tool.search to find a valid skill id.`,
          { recoverable: true },
        );
      }
      // loadSkills performs the actual context rewrite; it's wired only for the
      // interactive main actor (watcher/timer turns have no live skill set).
      if (!ctx.loadSkills) {
        return fail(
          "SKILL_LOAD_UNAVAILABLE",
          "Skill loading is not available in this context.",
          { recoverable: false },
        );
      }
      const activeSkillIds = ctx.loadSkills([skill.id]);
      return ok(`Skill ${skill.id} loaded.`, {
        id: skill.id,
        title: skill.title,
        summary: skill.summary,
        activeSkillIds,
      });
    },
  },
];
