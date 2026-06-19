/**
 * Attention-queue tools. Sub-threads (timers, watchers, workflows) report to the
 * main thread through the queue instead of interrupting it. These tools publish
 * events, read the digest, and resolve handled items. Publish/resolve mutate only
 * CLI-local daemon state (risk "local"); digest is read-only.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { QueuePublishArgs, Severity } from "../schemas.js";

const DigestArgs = z.object({
  severityAtLeast: Severity.optional().describe(
    "Only include events at or above this severity.",
  ),
  maxItems: z
    .number()
    .int()
    .positive()
    .optional()
    .describe("Maximum number of events to return."),
  includeResolved: z
    .boolean()
    .optional()
    .describe("Include already-resolved events."),
});

const ResolveArgs = z.object({
  id: z.string().describe("Id of the queue event to resolve."),
});

export const queueTools: ToolDef[] = [
  {
    name: "queue.publish",
    description:
      "Publish an event to the attention queue (deduplicated by dedupeKey). Used to surface status, alerts, or completions to the main thread.",
    risk: "local",
    schema: QueuePublishArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        source: {
          type: "string",
          enum: [
            "timer",
            "terminal_watcher",
            "worktree_watcher",
            "pr_watcher",
            "workflow",
            "model_worker",
            "system",
            "user",
          ],
          description: "Origin of the event.",
        },
        severity: {
          type: "string",
          enum: [
            "debug",
            "info",
            "attention",
            "urgent",
            "blocked",
            "done",
            "error",
          ],
          description: "Severity of the event.",
        },
        title: { type: "string", description: "Short event title." },
        summary: { type: "string", description: "One-line summary of the event." },
        target: {
          type: "object",
          additionalProperties: false,
          description: "What the event is about.",
          properties: {
            projectId: { type: "string" },
            worktreeId: { type: "string" },
            terminalId: { type: "string" },
            workflowRunId: { type: "string" },
          },
        },
        evidence: {
          type: "array",
          items: { type: "string" },
          description: "Supporting evidence lines.",
        },
        recommendedActions: {
          type: "array",
          description: "Actions the main thread could take.",
          items: {
            type: "object",
            additionalProperties: false,
            properties: {
              label: { type: "string" },
              toolName: { type: "string" },
              args: {},
              risk: { type: "string" },
              requiresConfirmation: { type: "boolean" },
            },
            required: ["label", "toolName"],
          },
        },
        dedupeKey: {
          type: "string",
          description: "Events sharing this key collapse into one.",
        },
        ttlMs: {
          type: "number",
          description: "Time-to-live in ms before the event expires.",
        },
      },
      required: ["source", "severity", "title", "summary"],
    },
    async handler(args, ctx) {
      try {
        const event = ctx.queue.publish(args);
        const dup = event.count > 1 ? ` (×${event.count})` : "";
        return ok(`Published ${event.id}: ${event.title}${dup}.`, event);
      } catch (e) {
        return fail(
          "QUEUE_PUBLISH",
          `Could not publish event: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "queue.digest",
    description:
      "Read the attention queue: returns the open (or filtered) events plus a formatted, human-readable digest (read-only).",
    risk: "read",
    readOnly: true,
    schema: DigestArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        severityAtLeast: {
          type: "string",
          enum: [
            "debug",
            "info",
            "attention",
            "urgent",
            "blocked",
            "done",
            "error",
          ],
          description: "Only include events at or above this severity.",
        },
        maxItems: {
          type: "number",
          description: "Maximum number of events to return.",
        },
        includeResolved: {
          type: "boolean",
          description: "Include already-resolved events.",
        },
      },
    },
    async handler(args, ctx) {
      try {
        const events = ctx.queue.digest(args ?? {});
        const text = ctx.queue.format(events);
        return ok(`Inbox has ${events.length} event(s).`, { events, text });
      } catch (e) {
        return fail(
          "QUEUE_DIGEST",
          `Could not read queue: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "queue.resolve",
    description:
      "Resolve (dismiss) a queue event by id so it no longer appears in the digest.",
    risk: "local",
    schema: ResolveArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Id of the queue event to resolve." },
      },
      required: ["id"],
    },
    async handler(args, ctx) {
      try {
        const resolved = ctx.queue.resolve(args.id);
        if (!resolved) {
          return fail(
            "QUEUE_NOT_FOUND",
            `No open queue event with id ${args.id}.`,
          );
        }
        return ok(`Resolved ${args.id}.`, { id: args.id, resolved });
      } catch (e) {
        return fail(
          "QUEUE_RESOLVE",
          `Could not resolve ${args.id}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
