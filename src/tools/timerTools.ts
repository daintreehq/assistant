/**
 * Durable timer tools (CLI-local). These schedule, list, and cancel one-shot or
 * repeating timers persisted in the SQLite store; the daemon scheduler fires them
 * and enqueues attention events, runs watcher-style checks, or invokes safe tools.
 * All state lives in the CLI daemon, so these are risk "local" (or "read" for the
 * listing). Timers never edit files.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";

const TimerPayload = z
  .object({
    type: z.enum(["enqueue", "run_check", "call_safe_tool"]),
    message: z.string().optional(),
    checkPrompt: z.string().optional(),
    toolCall: z
      .object({
        toolName: z.string(),
        args: z.record(z.unknown()).optional(),
      })
      .optional(),
  })
  .strict();

const TimerTarget = z
  .object({
    projectId: z.string().optional(),
    worktreeId: z.string().optional(),
    terminalId: z.string().optional(),
    workflowRunId: z.string().optional(),
  })
  .strict();

const ScheduleArgs = z.object({
  title: z.string().describe("Short human-readable label for the timer."),
  fireAt: z
    .string()
    .datetime()
    .optional()
    .describe("Absolute fire time as an ISO-8601 string. Overrides delayMs."),
  delayMs: z
    .number()
    .int()
    .positive()
    .optional()
    .describe("Fire after this many milliseconds from now. Used if fireAt is absent."),
  repeat: z
    .object({
      everyMs: z.number().int().positive().describe("Interval between repeats, in ms."),
      maxRuns: z.number().int().positive().optional().describe("Stop after this many fires."),
      until: z
        .string()
        .datetime()
        .optional()
        .describe("Stop repeating after this ISO-8601 time."),
    })
    .optional(),
  payload: TimerPayload.describe("What to do when the timer fires."),
  target: TimerTarget.optional().describe("Optional Daintree entity this timer relates to."),
});
type ScheduleArgs = z.infer<typeof ScheduleArgs>;

const CancelArgs = z.object({
  id: z.string().describe("Timer id to cancel."),
});
type CancelArgs = z.infer<typeof CancelArgs>;

const ListArgs = z.object({});

export const timerTools: ToolDef[] = [
  {
    name: "timer.schedule",
    description:
      "Schedule a durable one-shot or repeating timer. When it fires it can enqueue an attention event, run a check, or call a safe tool.",
    risk: "local",
    schema: ScheduleArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        title: { type: "string", description: "Short human-readable label for the timer." },
        fireAt: {
          type: "string",
          description: "Absolute fire time as an ISO-8601 string. Overrides delayMs.",
        },
        delayMs: {
          type: "number",
          description: "Fire after this many milliseconds from now. Used if fireAt is absent.",
        },
        repeat: {
          type: "object",
          additionalProperties: false,
          description: "Repeat configuration. Omit for a one-shot timer.",
          properties: {
            everyMs: { type: "number", description: "Interval between repeats, in ms." },
            maxRuns: { type: "number", description: "Stop after this many fires." },
            until: { type: "string", description: "Stop repeating after this ISO-8601 time." },
          },
          required: ["everyMs"],
        },
        payload: {
          type: "object",
          additionalProperties: false,
          description: "What to do when the timer fires.",
          properties: {
            type: {
              type: "string",
              enum: ["enqueue", "run_check", "call_safe_tool"],
              description: "Action kind.",
            },
            message: { type: "string", description: "Message text for an enqueue payload." },
            checkPrompt: { type: "string", description: "Prompt for a run_check payload." },
            toolCall: {
              type: "object",
              additionalProperties: false,
              description: "Tool to invoke for a call_safe_tool payload.",
              properties: {
                toolName: { type: "string" },
                args: { type: "object", additionalProperties: true },
              },
              required: ["toolName"],
            },
          },
          required: ["type"],
        },
        target: {
          type: "object",
          additionalProperties: false,
          description: "Optional Daintree entity this timer relates to.",
          properties: {
            projectId: { type: "string" },
            worktreeId: { type: "string" },
            terminalId: { type: "string" },
            workflowRunId: { type: "string" },
          },
        },
      },
      required: ["title", "payload"],
    },
    async handler(args: ScheduleArgs, ctx) {
      try {
        const fireAt =
          args.fireAt !== undefined
            ? Date.parse(args.fireAt)
            : args.delayMs !== undefined
              ? Date.now() + args.delayMs
              : NaN;
        if (Number.isNaN(fireAt)) {
          return fail(
            "TIMER_FIRE_AT",
            "Provide either a valid fireAt ISO string or a delayMs to compute the fire time.",
            { recoverable: false },
          );
        }

        const repeatUntil =
          args.repeat?.until !== undefined ? Date.parse(args.repeat.until) : undefined;
        if (repeatUntil !== undefined && Number.isNaN(repeatUntil)) {
          return fail("TIMER_REPEAT_UNTIL", "repeat.until is not a valid ISO-8601 string.", {
            recoverable: false,
          });
        }

        const rec = ctx.db.insertTimer({
          title: args.title,
          fireAt,
          repeatEveryMs: args.repeat?.everyMs,
          repeatUntil,
          maxRuns: args.repeat?.maxRuns,
          payloadType: args.payload.type,
          payloadJson: JSON.stringify(args.payload),
          targetJson: args.target ? JSON.stringify(args.target) : undefined,
        });

        const fireAtIso = new Date(rec.fireAt).toISOString();
        const repeatNote = rec.repeatEveryMs ? ` (repeats every ${rec.repeatEveryMs}ms)` : "";
        return ok(`Scheduled timer ${rec.id} "${rec.title}" for ${fireAtIso}${repeatNote}.`, {
          timerId: rec.id,
          fireAt: fireAtIso,
        });
      } catch (e) {
        return fail(
          "TIMER_SCHEDULE",
          `Could not schedule timer: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "timer.list",
    description: "List scheduled (pending) timers with their fire times and payload types.",
    risk: "read",
    readOnly: true,
    schema: ListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {},
    },
    async handler(_args, ctx) {
      try {
        const timers = ctx.db.listTimers("scheduled");
        const items = timers.map((t) => ({
          id: t.id,
          title: t.title,
          fireAt: new Date(t.fireAt).toISOString(),
          repeatEveryMs: t.repeatEveryMs,
          maxRuns: t.maxRuns,
          runCount: t.runCount,
          payloadType: t.payloadType,
        }));
        const summary =
          items.length === 0
            ? "No scheduled timers."
            : `${items.length} scheduled timer${items.length === 1 ? "" : "s"}: ${items
                .map((t) => `${t.id} "${t.title}" @ ${t.fireAt}`)
                .join("; ")}`;
        return ok(summary, { timers: items });
      } catch (e) {
        return fail(
          "TIMER_LIST",
          `Could not list timers: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "timer.cancel",
    description: "Cancel a scheduled timer so it will not fire.",
    risk: "local",
    schema: CancelArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Timer id to cancel." },
      },
      required: ["id"],
    },
    async handler(args: CancelArgs, ctx) {
      try {
        const existing = ctx.db.getTimer(args.id);
        if (!existing) {
          return fail("TIMER_NOT_FOUND", `No timer with id ${args.id}.`, { recoverable: false });
        }
        ctx.db.updateTimer(args.id, { status: "cancelled" });
        return ok(`Cancelled timer ${args.id}.`, { timerId: args.id, status: "cancelled" });
      } catch (e) {
        return fail(
          "TIMER_CANCEL",
          `Could not cancel timer ${args.id}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
