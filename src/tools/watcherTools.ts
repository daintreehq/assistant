/**
 * Terminal watcher tools. These manage CLI-local watcher state in the durable
 * store; the daemon's watcher engine drives the actual periodic checks. All
 * mutations here only touch local daemon state (risk "local"); listing is
 * read-only. No file mutation, no terminal input.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { WatchCondition, type WatcherRecord } from "../schemas.js";
import { MONITOR_DEFAULT_CADENCE_MS, PR_WATCHER_CADENCE_MS } from "../watcherCadence.js";
import type { PrWatcherOptions } from "../daemon/prWatcherEngine.js";
import { logDebug } from "../debugLog.js";

const CreateArgs = z.object({
  terminalIds: z
    .array(z.string())
    .min(1)
    .max(256)
    .describe("Daintree terminal ids to watch (max 256 — terminal.getStatus cap)."),
  title: z.string().describe("Short label for the watcher."),
  goal: z.string().describe("What the watcher is looking for / waiting on."),
  cadenceMs: z
    .number()
    .int()
    .positive()
    .optional()
    .describe("How often to check, in ms (default 120000)."),
  startAfterMs: z
    .number()
    .int()
    .nonnegative()
    .optional()
    .describe("Delay before the first check, in ms."),
  stopAfterMs: z
    .number()
    .int()
    .positive()
    .optional()
    .describe("Stop watching after this many ms (timeout)."),
  stopWhen: WatchCondition.optional().describe(
    "Condition that ends the watcher when met.",
  ),
  alertWhen: WatchCondition.optional().describe(
    "Condition that raises an attention event when met.",
  ),
  modelTier: z
    .enum(["small", "medium"])
    .optional()
    .describe("Model tier used to classify output (default small)."),
});

const CancelArgs = z.object({
  id: z.string().describe("Watcher id to cancel."),
});

const WatchPrArgs = z.object({
  prNumber: z
    .number()
    .int()
    .positive()
    .describe("Forge pull/merge request number to watch."),
  cwd: z
    .string()
    .optional()
    .describe("Repo working directory passed to forge.getPR (defaults to the bound project)."),
  title: z
    .string()
    .optional()
    .describe("Short label for the watcher (defaults to 'PR #N')."),
  startAfterMs: z
    .number()
    .int()
    .nonnegative()
    .optional()
    .describe("Delay before the first check, in ms."),
  stopAfterMs: z
    .number()
    .int()
    .positive()
    .optional()
    .describe("Stop watching after this many ms (timeout)."),
});

/*
 * JSON Schema for the WatchCondition DSL surfaced to the model on
 * `watcher.terminal.create`. The Zod `WatchCondition` union in schemas.ts is the
 * runtime authority — this hand-written shape is what Fireworks actually sees, so
 * the model can discover the full DSL (especially the `modelJudge` leaf, which is
 * otherwise invisible behind an opaque `{ type: "object" }`).
 *
 * Fireworks tool-calling constraints shape this schema — do not "clean it up":
 *  - Use `anyOf`, NEVER `oneOf` (Fireworks does not support `oneOf`).
 *  - No `$ref`/recursive schemas: grammar-guided decoding degrades on deeply
 *    recursive `anyOf`, so the combinators (all/any/not) are flattened to a single
 *    level — their children are atomic leaves only, which covers all real usage.
 *  - The DSL `not` member is a *property literally named "not"*, NOT the JSON-Schema
 *    `not` keyword (also unsupported by Fireworks, and a silent-drop hazard). Never
 *    hoist it to a top-level `not` key on a schema object.
 *
 * The `stateIs` enum mirrors `AgentState` in schemas.ts — keep the two in sync.
 */
const WATCH_CONDITION_LEAVES: Record<string, unknown>[] = [
  {
    type: "object",
    additionalProperties: false,
    properties: {
      // Mirror of AgentState in schemas.ts — keep in sync if the enum changes.
      stateIs: {
        type: "string",
        enum: ["idle", "working", "waiting", "directing", "completed", "exited"],
        description: "Fire when the watched terminal's agent reaches this lifecycle state.",
      },
    },
    required: ["stateIs"],
  },
  {
    type: "object",
    additionalProperties: false,
    properties: {
      runtimeStatusIs: {
        type: "string",
        enum: ["running", "exited"],
        description: "Fire on the terminal process runtime status. Free, deterministic.",
      },
    },
    required: ["runtimeStatusIs"],
  },
  {
    type: "object",
    additionalProperties: false,
    properties: {
      contains: {
        type: "string",
        description:
          "Fire when recent terminal output contains this substring (non-empty). Free, deterministic — prefer this over modelJudge when it suffices.",
      },
    },
    required: ["contains"],
  },
  {
    type: "object",
    additionalProperties: false,
    properties: {
      regex: {
        type: "string",
        description:
          "Fire when recent terminal output matches this regular expression (non-empty, valid). Free, deterministic.",
      },
    },
    required: ["regex"],
  },
  {
    type: "object",
    additionalProperties: false,
    properties: {
      noOutputForMs: {
        type: "integer",
        minimum: 1,
        description:
          "Fire when the terminal has produced no output for this many ms (positive integer). Free, deterministic.",
      },
    },
    required: ["noOutputForMs"],
  },
  {
    type: "object",
    additionalProperties: false,
    properties: {
      modelJudge: {
        type: "string",
        description:
          "Fire when a model answers yes to this natural-language yes/no question about the terminal's output. Use only when the deterministic leaves (contains/regex/stateIs/runtimeStatusIs/noOutputForMs) cannot express the condition: each distinct question costs one model call at the watcher's configured tier (modelTier, default small) per cadence tick (deduped across stopWhen and alertWhen; multiple distinct questions run in parallel). Example: \"Did the build finish successfully?\"",
      },
    },
    required: ["modelJudge"],
  },
];

// One level of combinators over atomic leaves. Children reference the leaf list
// only (no nested combinators) — see the recursion note above.
const WATCH_CONDITION_SCHEMA: Record<string, unknown> = {
  anyOf: [
    ...WATCH_CONDITION_LEAVES,
    {
      type: "object",
      additionalProperties: false,
      properties: {
        all: {
          type: "array",
          minItems: 1,
          items: { anyOf: WATCH_CONDITION_LEAVES },
          description: "Fire only when every child condition is met. Children are atomic leaves.",
        },
      },
      required: ["all"],
    },
    {
      type: "object",
      additionalProperties: false,
      properties: {
        any: {
          type: "array",
          minItems: 1,
          items: { anyOf: WATCH_CONDITION_LEAVES },
          description: "Fire when at least one child condition is met. Children are atomic leaves.",
        },
      },
      required: ["any"],
    },
    {
      type: "object",
      additionalProperties: false,
      // `not` is a property NAME here, not the JSON-Schema `not` keyword. Never
      // hoist it to a top-level `not` on this object — Fireworks would drop it.
      properties: {
        not: {
          anyOf: WATCH_CONDITION_LEAVES,
          description: "Fire when the child condition is NOT met. Child is an atomic leaf.",
        },
      },
      required: ["not"],
    },
  ],
};

function summarizeWatcher(w: WatcherRecord): string {
  let targets: string[] = [];
  try {
    targets = JSON.parse(w.targetsJson) as string[];
  } catch {
    targets = [];
  }
  // PR watchers carry a display label ("PR #N") in targetsJson and make no model
  // call, so the modelTier suffix would be noise — render them as their own kind.
  if (w.kind === "pr_state") {
    return `${w.id} [${w.status}] ${w.title} — ${targets.join(", ")} (pr_state, every ${w.cadenceMs}ms)`;
  }
  return `${w.id} [${w.status}] ${w.title} — ${targets.join(", ")} (every ${w.cadenceMs}ms, ${w.modelTier})`;
}

export const watcherTools: ToolDef[] = [
  {
    name: "watcher.terminal.create",
    description:
      "Create a terminal watcher that periodically classifies one or more Daintree terminals and raises attention events. Read-only orchestration; never edits files. The optional stopWhen/alertWhen conditions use the WatchCondition DSL: stateIs, runtimeStatusIs, contains, regex, noOutputForMs, modelJudge, plus one level of all/any/not combinators. Prefer the deterministic leaves (free); modelJudge runs a model call per check at the watcher's configured tier (default small).",
    risk: "local",
    schema: CreateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalIds: {
          type: "array",
          items: { type: "string" },
          description: "Daintree terminal ids to watch.",
        },
        title: { type: "string", description: "Short label for the watcher." },
        goal: {
          type: "string",
          description: "What the watcher is looking for / waiting on.",
        },
        cadenceMs: {
          type: "number",
          description: "How often to check, in ms (default 120000).",
        },
        startAfterMs: {
          type: "number",
          description: "Delay before the first check, in ms.",
        },
        stopAfterMs: {
          type: "number",
          description: "Stop watching after this many ms (timeout).",
        },
        stopWhen: {
          ...WATCH_CONDITION_SCHEMA,
          description:
            "Condition that ends the watcher when met. WatchCondition DSL — pick exactly one member per object (see the per-branch descriptions).",
        },
        alertWhen: {
          ...WATCH_CONDITION_SCHEMA,
          description:
            "Condition that raises an attention event when met. Same WatchCondition DSL as stopWhen.",
        },
        modelTier: {
          type: "string",
          enum: ["small", "medium"],
          description: "Model tier used to classify output (default small).",
        },
      },
      required: ["terminalIds", "title", "goal"],
    },
    async handler(args: z.infer<typeof CreateArgs>, ctx) {
      try {
        const w = ctx.db.insertWatcher({
          kind: "terminal",
          title: args.title,
          goal: args.goal,
          targetsJson: JSON.stringify(args.terminalIds),
          cadenceMs: args.cadenceMs ?? MONITOR_DEFAULT_CADENCE_MS,
          isSupervisor: false,
          modelTier: args.modelTier ?? "small",
          startAfterMs: args.startAfterMs,
          stopAfterMs: args.stopAfterMs,
          stopWhenJson: args.stopWhen
            ? JSON.stringify(args.stopWhen)
            : undefined,
          alertWhenJson: args.alertWhen
            ? JSON.stringify(args.alertWhen)
            : undefined,
          nextCheckAt: Date.now() + (args.startAfterMs ?? 0),
        });
        logDebug(ctx.config, "watcher.created", {
          watcherId: w.id,
          kind: "terminal",
          isSupervisor: false,
          via: "watcher.terminal.create",
          title: args.title,
          goal: args.goal,
          targets: args.terminalIds,
          cadenceMs: w.cadenceMs,
          modelTier: w.modelTier,
          startAfterMs: args.startAfterMs,
          stopAfterMs: args.stopAfterMs,
          stopWhen: args.stopWhen,
          alertWhen: args.alertWhen,
          nextCheckAt: w.nextCheckAt,
        });
        // Always surface the foreground-only lifecycle, even when the scheduler
        // is running: supervision pauses the moment the assistant is closed.
        const schedulerRunning = ctx.daemonActive ? ctx.daemonActive() : true;
        const lifecycleNote = schedulerRunning
          ? " NOTE: supervision runs only while this assistant is open; this watcher is discarded when you close the assistant and does not resume on the next launch (watchers are session-scoped)."
          : " NOTE: no scheduler is running in this session, so it will not check until the assistant runs interactively.";
        return ok(
          `Created terminal watcher ${w.id} for ${args.terminalIds.length} terminal(s).${lifecycleNote}`,
          {
            id: w.id,
            nextCheckAt: w.nextCheckAt,
            daemonActive: ctx.daemonActive ? ctx.daemonActive() : true,
          },
        );
      } catch (e) {
        return fail(
          "WATCHER_CREATE",
          `Could not create watcher: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "watcher.watchPR",
    description:
      "Create an in-session PR-state watcher that polls forge.getPR (~every 60s) and raises an attention event when the PR is merged or closed, becomes ready for review (draft→ready), or has new activity (comment/push/review). Read-only orchestration; never edits files or writes to the forge. IMPORTANT: it polls only while this assistant is open (session-scoped, foreground-only) and cannot see review-comment contents — only PR state, draft, and last-updated transitions. Use this to follow up on a PR without blocking the conversation.",
    risk: "local",
    schema: WatchPrArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        prNumber: {
          type: "number",
          description: "Forge pull/merge request number to watch.",
        },
        cwd: {
          type: "string",
          description:
            "Repo working directory passed to forge.getPR (defaults to the bound project).",
        },
        title: {
          type: "string",
          description: "Short label for the watcher (defaults to 'PR #N').",
        },
        startAfterMs: {
          type: "number",
          description: "Delay before the first check, in ms.",
        },
        stopAfterMs: {
          type: "number",
          description: "Stop watching after this many ms (timeout).",
        },
      },
      required: ["prNumber"],
    },
    async handler(args: z.infer<typeof WatchPrArgs>, ctx) {
      try {
        const options: PrWatcherOptions = {
          cwd: args.cwd,
          prNumber: args.prNumber,
          // No baseline yet — the first check records the PR's current state
          // silently (unless it is already merged/closed, which resolves it).
          lastState: undefined,
          lastIsDraft: undefined,
          lastUpdatedAt: undefined,
        };
        const w = ctx.db.insertWatcher({
          kind: "pr_state",
          title: args.title ?? `PR #${args.prNumber}`,
          goal: `Watch PR #${args.prNumber} for merge/close, ready-for-review, and activity.`,
          // A display label, not a terminal id — keeps the NOT NULL column valid
          // and lets watcher.list render the row.
          targetsJson: JSON.stringify([`PR #${args.prNumber}`]),
          // Fixed cadence — not user-configurable, to avoid hammering the forge.
          cadenceMs: PR_WATCHER_CADENCE_MS,
          isSupervisor: false,
          // No model is consulted; "small" simply satisfies the required field.
          modelTier: "small",
          startAfterMs: args.startAfterMs,
          stopAfterMs: args.stopAfterMs,
          optionsJson: JSON.stringify(options),
          nextCheckAt: Date.now() + (args.startAfterMs ?? 0),
        });
        logDebug(ctx.config, "watcher.created", {
          watcherId: w.id,
          kind: "pr_state",
          isSupervisor: false,
          via: "watcher.watchPR",
          title: w.title,
          prNumber: args.prNumber,
          cwd: args.cwd,
          cadenceMs: w.cadenceMs,
          startAfterMs: args.startAfterMs,
          stopAfterMs: args.stopAfterMs,
          nextCheckAt: w.nextCheckAt,
        });
        // Same foreground-only lifecycle note the terminal watcher emits: polling
        // pauses the moment the assistant is closed and never resumes next launch.
        const schedulerRunning = ctx.daemonActive ? ctx.daemonActive() : true;
        const lifecycleNote = schedulerRunning
          ? " NOTE: this PR watcher polls only while the assistant is open; it is discarded when you close the assistant and does not resume on the next launch (watchers are session-scoped). It cannot read review comments — only state, draft, and activity transitions."
          : " NOTE: no scheduler is running in this session, so it will not poll until the assistant runs interactively.";
        return ok(
          `Created PR watcher ${w.id} for PR #${args.prNumber}.${lifecycleNote}`,
          {
            id: w.id,
            prNumber: args.prNumber,
            cadenceMs: w.cadenceMs,
            nextCheckAt: w.nextCheckAt,
            daemonActive: ctx.daemonActive ? ctx.daemonActive() : true,
          },
        );
      } catch (e) {
        return fail(
          "WATCHER_CREATE",
          `Could not create PR watcher: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "watcher.list",
    description: "List active terminal and PR watchers (read-only).",
    risk: "read",
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {},
    },
    async handler(_args, ctx) {
      const watchers = ctx.db.listWatchers("active");
      const lines = watchers.map(summarizeWatcher);
      return ok(
        watchers.length
          ? `${watchers.length} active watcher(s):\n${lines.join("\n")}`
          : "No active watchers.",
        { watchers },
      );
    },
  },
  {
    name: "watcher.cancel",
    description: "Cancel a watcher by id (local daemon state only).",
    risk: "local",
    schema: CancelArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Watcher id to cancel." },
      },
      required: ["id"],
    },
    async handler(args: z.infer<typeof CancelArgs>, ctx) {
      const existing = ctx.db.getWatcher(args.id);
      if (!existing) {
        return fail("WATCHER_NOT_FOUND", `No watcher with id ${args.id}.`, {
          recoverable: false,
        });
      }
      ctx.db.updateWatcher(args.id, { status: "cancelled" });
      // A cancelled watcher must not retain any scoped authorization.
      ctx.db.revokeGrantsByActor(args.id);
      return ok(`Cancelled watcher ${args.id}.`, { id: args.id });
    },
  },
];
