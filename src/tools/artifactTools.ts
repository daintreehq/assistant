/**
 * artifactTools — read-only retrieval for oversized tool results.
 *
 * When a serialized tool result overflows the inline size limit, the agent loop
 * stashes the full JSON envelope in the session's `artifactStore` and hands the
 * model a compact stub carrying an `artifactId` (see `serializeToolResult`). This
 * tool lets the model page back through that full output by character range instead
 * of trying to reason about a clipped blob.
 *
 * The store is in-memory and session-scoped: ids are only valid within the session
 * that produced them, so a stale or replayed id fails gracefully rather than crashing.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";

/**
 * Default and ceiling on how many characters one read returns. The ceiling is kept
 * below MAX_TOOL_RESULT_CHARS so a single read's own result doesn't overflow and get
 * re-stashed as yet another artifact; the model pages large artifacts across calls.
 */
const DEFAULT_READ_CHARS = 4000;
const MAX_READ_CHARS = 6000;

const ReadArgs = z.object({
  artifactId: z
    .string()
    .min(1)
    .describe("The artifactId from a truncated tool result."),
  offset: z
    .number()
    .int()
    .min(0)
    .optional()
    .describe("Character offset to start reading from (default 0)."),
  limit: z
    .number()
    .int()
    .min(1)
    .max(MAX_READ_CHARS)
    .optional()
    .describe(`Max characters to return (default ${DEFAULT_READ_CHARS}, max ${MAX_READ_CHARS}).`),
});

export const artifactTools: ToolDef[] = [
  {
    name: "artifact.read",
    description:
      "Read a slice of a large tool result that was stored as an artifact because it overflowed the inline size limit. Pass the artifactId from a truncated result, plus an optional character offset and limit, to page through the full output. Use the returned nextOffset/eof to continue.",
    risk: "read",
    readOnly: true,
    schema: ReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        artifactId: {
          type: "string",
          description: "The artifactId from a truncated tool result.",
        },
        offset: {
          type: "number",
          description: "Character offset to start reading from (default 0).",
        },
        limit: {
          type: "number",
          description: `Max characters to return (default ${DEFAULT_READ_CHARS}, max ${MAX_READ_CHARS}).`,
        },
      },
      required: ["artifactId"],
    },
    async handler(args, ctx) {
      const store = ctx.artifactStore;
      if (!store) {
        return fail(
          "ARTIFACT_UNAVAILABLE",
          "Artifact storage is not available in this context, so the full output cannot be retrieved.",
          { recoverable: false },
        );
      }
      const full = store.get(args.artifactId);
      if (full === undefined) {
        return fail(
          "ARTIFACT_NOT_FOUND",
          `No artifact found with id "${args.artifactId}". Artifacts live only for the current session and may already have been replaced.`,
          { recoverable: false },
        );
      }
      const totalChars = full.length;
      // Clamp the offset into range so a past-the-end read returns empty-at-eof
      // rather than an error, and bound the limit so one read never overflows.
      const offset = Math.min(Math.max(args.offset ?? 0, 0), totalChars);
      const limit = Math.min(args.limit ?? DEFAULT_READ_CHARS, MAX_READ_CHARS);
      const content = full.slice(offset, offset + limit);
      const nextOffset = offset + content.length;
      const eof = nextOffset >= totalChars;
      return ok(
        `Read ${content.length} of ${totalChars} chars from ${args.artifactId} (offset ${offset}${eof ? ", end of artifact" : `, ${totalChars - nextOffset} remaining`}).`,
        {
          artifactId: args.artifactId,
          offset,
          limit,
          totalChars,
          content,
          nextOffset,
          eof,
        },
      );
    },
  },
];
