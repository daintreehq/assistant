/**
 * Terminal watcher tools. These manage CLI-local watcher state in the durable
 * store; the daemon's watcher engine drives the actual periodic checks. All
 * mutations here only touch local daemon state (risk "local"); listing is
 * read-only. No file mutation, no terminal input.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { WatchCondition, type WatcherRecord } from "../schemas.js";

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

function summarizeWatcher(w: WatcherRecord): string {
  let targets: string[] = [];
  try {
    targets = JSON.parse(w.targetsJson) as string[];
  } catch {
    targets = [];
  }
  return `${w.id} [${w.status}] ${w.title} — ${targets.join(", ")} (every ${w.cadenceMs}ms, ${w.modelTier})`;
}

export const watcherTools: ToolDef[] = [
  {
    name: "watcher.terminal.create",
    description:
      "Create a terminal watcher that periodically classifies one or more Daintree terminals and raises attention events. Read-only orchestration; never edits files.",
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
          type: "object",
          description: "Condition that ends the watcher when met.",
        },
        alertWhen: {
          type: "object",
          description: "Condition that raises an attention event when met.",
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
          cadenceMs: args.cadenceMs ?? 120_000,
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
        // Always surface the foreground-only lifecycle, even when the scheduler
        // is running: supervision pauses the moment the assistant is closed.
        const schedulerRunning = ctx.daemonActive ? ctx.daemonActive() : true;
        const lifecycleNote = schedulerRunning
          ? " NOTE: supervision runs only while this assistant is open; this watcher pauses when you close the assistant and resumes on the next launch."
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
    name: "watcher.list",
    description: "List active terminal/worktree watchers (read-only).",
    risk: "read",
    readOnly: true,
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
