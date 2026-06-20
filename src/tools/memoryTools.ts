/**
 * Cross-session project memory tools. They let the assistant persist and recall
 * durable facts/decisions/procedures across sessions — closing the gap left by
 * lossy session summaries and the linear-only conversation log.
 *
 * Recall is on-demand and tool-based, NOT injected into the system prompt: the
 * base prompt is the cached prefix and must stay byte-stable (see CLAUDE.md), so
 * the model pulls relevant memories with `memory.recall`/`memory.list` within a
 * turn instead. Reads are risk "read" (always surfaced via CORE_TOOL_NAMES);
 * writes (save/forget/pin/unpin) are risk "local" — local daemon state only, no
 * file or terminal mutation. Backed by an FTS5 index in `src/storage/db.ts`.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import type { MemoryRecord } from "../schemas.js";

const RecallArgs = z.object({
  query: z
    .string()
    .describe("Free-text search; matched against memory content (BM25-ranked)."),
  category: z
    .string()
    .optional()
    .describe("Only recall memories with this exact category tag."),
  limit: z
    .number()
    .int()
    .positive()
    .max(50)
    .optional()
    .describe("Max results (default 10)."),
});

const ListArgs = z.object({
  category: z
    .string()
    .optional()
    .describe("Only list memories with this exact category tag."),
  pinnedOnly: z
    .boolean()
    .optional()
    .describe("Only list pinned memories."),
  limit: z
    .number()
    .int()
    .positive()
    .max(200)
    .optional()
    .describe("Max results (default 50)."),
});

const SaveArgs = z.object({
  content: z
    .string()
    .min(1)
    .describe("The fact, decision, or procedure to remember."),
  category: z
    .string()
    .optional()
    .describe('Optional tag, e.g. "convention", "decision", "fix".'),
  // "compact" is intentionally excluded — it is reserved for internal
  // auto-compaction, not a value a tool caller may set.
  source: z
    .enum(["user", "assistant"])
    .optional()
    .describe('Who the memory came from (default "assistant").'),
});

const IdArgs = z.object({
  id: z.string().describe("Memory id (mem_…)."),
});

/** Model-friendly view of a stored memory. */
function toView(rec: MemoryRecord) {
  return {
    id: rec.id,
    content: rec.content,
    category: rec.category,
    source: rec.source,
    pinned: rec.pinnedAt != null,
    createdAt: rec.createdAt,
    updatedAt: rec.updatedAt,
  };
}

function summarize(rec: MemoryRecord): string {
  const tag = rec.category ? ` [${rec.category}]` : "";
  const pin = rec.pinnedAt != null ? " 📌" : "";
  const text = rec.content.length > 80 ? `${rec.content.slice(0, 77)}…` : rec.content;
  return `${rec.id}${tag}${pin} ${text}`;
}

export const memoryTools: ToolDef[] = [
  {
    name: "memory.recall",
    description:
      "Search durable project memory for facts/decisions/procedures relevant to the current task (full-text, best-match first). Use this to recover context that earlier sessions saved. Read-only.",
    risk: "read",
    schema: RecallArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        query: {
          type: "string",
          description: "Free-text search; matched against memory content (BM25-ranked).",
        },
        category: {
          type: "string",
          description: "Only recall memories with this exact category tag.",
        },
        limit: { type: "number", description: "Max results (default 10)." },
      },
      required: ["query"],
    },
    async handler(args: z.infer<typeof RecallArgs>, ctx) {
      try {
        const rows = ctx.db.recallMemories(args.query, {
          category: args.category,
          limit: args.limit,
        });
        const summary = rows.length
          ? `Recalled ${rows.length} ${rows.length === 1 ? "memory" : "memories"}:\n` +
            rows.map((r) => summarize(r)).join("\n")
          : "No matching memories.";
        return ok(summary, { memories: rows.map(toView) });
      } catch (e) {
        return fail(
          "MEMORY_RECALL",
          `Could not recall memories: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "memory.list",
    description:
      "List stored project memories (pinned first, then most recent), optionally filtered by category or to pinned-only. Read-only; use memory.recall for relevance search.",
    risk: "read",
    schema: ListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        category: {
          type: "string",
          description: "Only list memories with this exact category tag.",
        },
        pinnedOnly: { type: "boolean", description: "Only list pinned memories." },
        limit: { type: "number", description: "Max results (default 50)." },
      },
      required: [],
    },
    async handler(args: z.infer<typeof ListArgs>, ctx) {
      try {
        const rows = ctx.db.listMemories({
          category: args.category,
          pinnedOnly: args.pinnedOnly,
          limit: args.limit,
        });
        const summary = rows.length
          ? `${rows.length} ${rows.length === 1 ? "memory" : "memories"}:\n` +
            rows.map((r) => summarize(r)).join("\n")
          : "No memories stored.";
        return ok(summary, { memories: rows.map(toView) });
      } catch (e) {
        return fail(
          "MEMORY_LIST",
          `Could not list memories: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "memory.save",
    description:
      "Persist a durable project fact/decision/procedure so future sessions can recall it. Use for non-obvious, lasting context (conventions, decisions, gotchas) — not transient task state. Local daemon state only; never edits files.",
    risk: "local",
    schema: SaveArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        content: {
          type: "string",
          description: "The fact, decision, or procedure to remember.",
        },
        category: {
          type: "string",
          description: 'Optional tag, e.g. "convention", "decision", "fix".',
        },
        source: {
          type: "string",
          enum: ["user", "assistant"],
          description: 'Who the memory came from (default "assistant").',
        },
      },
      required: ["content"],
    },
    async handler(args: z.infer<typeof SaveArgs>, ctx) {
      try {
        const rec = ctx.db.insertMemory({
          content: args.content,
          category: args.category,
          source: args.source,
        });
        return ok(`Saved memory ${rec.id}.`, { id: rec.id, memory: toView(rec) });
      } catch (e) {
        return fail(
          "MEMORY_SAVE",
          `Could not save memory: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "memory.forget",
    description:
      "Forget a stored memory by id (soft delete — it stops being recalled but is retained for audit). Local daemon state only.",
    risk: "local",
    schema: IdArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: { id: { type: "string", description: "Memory id (mem_…)." } },
      required: ["id"],
    },
    async handler(args: z.infer<typeof IdArgs>, ctx) {
      const forgotten = ctx.db.forgetMemory(args.id);
      if (!forgotten) {
        return fail(
          "MEMORY_NOT_FOUND",
          `No live memory ${args.id} to forget (already forgotten or unknown id).`,
        );
      }
      return ok(`Forgot memory ${args.id}.`, { id: args.id });
    },
  },
  {
    name: "memory.pin",
    description:
      "Pin a memory by id so it stays prioritized in listings and survives any future pruning. Idempotent. Local daemon state only.",
    risk: "local",
    schema: IdArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: { id: { type: "string", description: "Memory id (mem_…)." } },
      required: ["id"],
    },
    async handler(args: z.infer<typeof IdArgs>, ctx) {
      const rec = ctx.db.pinMemory(args.id);
      if (!rec) {
        return fail(
          "MEMORY_NOT_FOUND",
          `No live memory ${args.id} to pin (forgotten or unknown id).`,
        );
      }
      return ok(`Pinned memory ${args.id}.`, { id: args.id, memory: toView(rec) });
    },
  },
  {
    name: "memory.unpin",
    description: "Unpin a previously pinned memory by id. Idempotent. Local daemon state only.",
    risk: "local",
    schema: IdArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: { id: { type: "string", description: "Memory id (mem_…)." } },
      required: ["id"],
    },
    async handler(args: z.infer<typeof IdArgs>, ctx) {
      const rec = ctx.db.unpinMemory(args.id);
      if (!rec) {
        return fail(
          "MEMORY_NOT_FOUND",
          `No live memory ${args.id} to unpin (forgotten or unknown id).`,
        );
      }
      return ok(`Unpinned memory ${args.id}.`, { id: args.id, memory: toView(rec) });
    },
  },
];
