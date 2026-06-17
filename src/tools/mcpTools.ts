/**
 * Daintree MCP tools: status, tool discovery, and the raw passthrough call.
 *
 * These tools expose the Daintree MCP surface to the agent. `daintree.status`
 * works even when disconnected so the model can reason about degraded mode;
 * `daintree.listTools` and `tool.search` fail cleanly with MCP_UNAVAILABLE when
 * the client is not connected. `daintree.call` is the escape hatch — a raw
 * passthrough marked risk "project" so it always confirms.
 */
import { z } from "zod";
import { ok, fail, NO_ARGS, type ToolDef } from "./types.js";
import type { ToolContext } from "./types.js";
import type { ToolResult } from "../schemas.js";

/**
 * Forward a call to a named Daintree MCP tool. Shared by the typed wrappers
 * below and structurally identical to daintree.call — but each wrapper carries an
 * accurate risk class, so operators can run recipes / focus terminals without the
 * system-tier raw escape hatch. The arguments object is forwarded verbatim, so
 * these stay agnostic to Daintree's exact per-tool argument schema.
 */
async function passthrough(
  ctx: ToolContext,
  mcpName: string,
  args: Record<string, unknown>,
  requestKey?: string,
): Promise<ToolResult> {
  if (!ctx.mcp.isConnected()) {
    return fail("MCP_UNAVAILABLE", `Daintree MCP is not connected; cannot call ${mcpName}.`);
  }
  try {
    const callArgs: Record<string, unknown> = {
      ...args,
      ...(requestKey ? { requestKey } : {}),
    };
    const res = await ctx.mcp.callTool(mcpName, callArgs);
    if (res.isError) {
      return fail("MCP_TOOL_ERROR", res.text || `Daintree tool ${mcpName} returned an error.`, {
        details: { structuredContent: res.structuredContent },
      });
    }
    return ok(`Called ${mcpName}.`, {
      text: res.text,
      structuredContent: res.structuredContent,
    });
  } catch (e) {
    return fail(
      "MCP_TOOL_ERROR",
      `Daintree call ${mcpName} failed: ${e instanceof Error ? e.message : String(e)}`,
    );
  }
}

const ListToolsArgs = z.object({}).strict();

const RecipeListArgs = z
  .object({ arguments: z.record(z.string(), z.unknown()).optional() })
  .strict();

const RecipeRunArgs = z.object({
  recipeId: z.string().describe("Daintree workspace recipe id to run."),
  arguments: z
    .record(z.string(), z.unknown())
    .optional()
    .describe("Recipe arguments forwarded to Daintree (e.g. worktreeId)."),
  requestKey: z.string().optional(),
});

const WorktreeCreateArgs = z.object({
  arguments: z
    .record(z.string(), z.unknown())
    .describe("Arguments for worktree.createWithRecipe (recipe id, name, etc.)."),
  requestKey: z.string().optional(),
});

const FocusArgs = z.object({
  terminalId: z.string().describe("Daintree terminal id to focus in the UI."),
});

const SearchArgs = z.object({
  query: z.string().describe("Keyword to match against MCP tool names/descriptions."),
  max: z.number().int().positive().max(100).optional(),
});

const CallArgs = z.object({
  name: z.string().describe("Daintree MCP tool name to invoke."),
  arguments: z.record(z.string(), z.unknown()).optional(),
  requestKey: z.string().optional(),
});

export const mcpTools: ToolDef[] = [
  {
    name: "daintree.status",
    description:
      "Report Daintree MCP connection status (connected, transport, tool count). Works even when disconnected.",
    risk: "read",
    readOnly: true,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      const status = ctx.mcp.status();
      const summary = status.connected
        ? `Daintree MCP connected via ${status.transport ?? "unknown"}${
            status.toolCount != null ? ` (${status.toolCount} tools)` : ""
          }.`
        : `Daintree MCP disconnected${status.error ? `: ${status.error}` : "."}`;
      return ok(summary, status);
    },
  },
  {
    name: "daintree.listTools",
    description:
      "List the Daintree MCP tools available, with their names and descriptions.",
    risk: "read",
    readOnly: true,
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected; cannot list tools.",
        );
      }
      try {
        const tools = await ctx.mcp.listTools();
        const list = tools.map((t) => ({
          name: t.name,
          description: t.description ?? "",
        }));
        return ok(`Found ${list.length} Daintree MCP tool(s).`, { tools: list });
      } catch (e) {
        return fail(
          "MCP_UNAVAILABLE",
          `Could not list Daintree MCP tools: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "tool.search",
    description:
      "Search Daintree MCP tools by keyword (substring match on name/description). Local CLI tools are always available regardless of results.",
    risk: "read",
    readOnly: true,
    schema: SearchArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        query: {
          type: "string",
          description: "Keyword to match against MCP tool names/descriptions.",
        },
        max: { type: "number", description: "Max results to return (default 20)." },
      },
      required: ["query"],
    },
    async handler(args, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected; cannot search MCP tools.",
        );
      }
      try {
        const max = args.max ?? 20;
        const q = args.query.toLowerCase();
        const tools = await ctx.mcp.listTools();
        const matches = tools
          .filter(
            (t) =>
              t.name.toLowerCase().includes(q) ||
              (t.description ?? "").toLowerCase().includes(q),
          )
          .slice(0, max)
          .map((t) => ({ name: t.name, description: t.description ?? "" }));
        return ok(
          `Found ${matches.length} Daintree MCP tool(s) matching "${args.query}". Local CLI tools are always available.`,
          {
            query: args.query,
            matches,
            note: "Local CLI tools are always available regardless of these results.",
          },
        );
      } catch (e) {
        return fail(
          "MCP_UNAVAILABLE",
          `Could not search Daintree MCP tools: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "daintree.call",
    description:
      "Raw passthrough to ANY Daintree MCP tool. Escape hatch — highest risk ('system'), always confirmed, requires the 'system' tier. Prefer purpose-built tools; use this only when no wrapper exists.",
    risk: "system",
    schema: CallArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        name: { type: "string", description: "Daintree MCP tool name to invoke." },
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Arguments object passed to the MCP tool.",
        },
        requestKey: {
          type: "string",
          description: "Optional idempotency / request key forwarded to the tool.",
        },
      },
      required: ["name"],
    },
    async handler(args, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          `Daintree MCP is not connected; cannot call ${args.name}.`,
        );
      }
      try {
        const callArgs: Record<string, unknown> = {
          ...(args.arguments ?? {}),
          ...(args.requestKey ? { requestKey: args.requestKey } : {}),
        };
        const res = await ctx.mcp.callTool(args.name, callArgs);
        if (res.isError) {
          return fail(
            "MCP_TOOL_ERROR",
            res.text || `Daintree MCP tool ${args.name} returned an error.`,
            { details: { structuredContent: res.structuredContent } },
          );
        }
        return ok(`Called ${args.name}.`, {
          text: res.text,
          structuredContent: res.structuredContent,
          isError: res.isError,
        });
      } catch (e) {
        return fail(
          "MCP_TOOL_ERROR",
          `Daintree MCP call ${args.name} failed: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "recipe.list",
    description:
      "List available Daintree workspace recipes (read-only). Typed wrapper around the Daintree recipe.list MCP tool.",
    risk: "read",
    readOnly: true,
    schema: RecipeListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Optional filters forwarded to Daintree (e.g. projectId).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "recipe.list", args.arguments ?? {});
    },
  },
  {
    name: "recipe.run",
    description:
      "Run a Daintree workspace recipe against the current/active context. Mutates real workspace state, so it always confirms.",
    risk: "project",
    schema: RecipeRunArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        recipeId: { type: "string", description: "Daintree workspace recipe id to run." },
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Recipe arguments forwarded to Daintree (e.g. worktreeId).",
        },
        requestKey: { type: "string", description: "Optional idempotency key." },
      },
      required: ["recipeId"],
    },
    async handler(args, ctx) {
      // Explicit recipeId wins — a nested arguments.recipeId must not override
      // the confirmed/audited top-level value.
      return passthrough(
        ctx,
        "recipe.run",
        { ...(args.arguments ?? {}), recipeId: args.recipeId },
        args.requestKey,
      );
    },
  },
  {
    name: "worktree.createWithRecipe",
    description:
      "Create a new Daintree worktree with a startup recipe. Mutates real workspace state, so it always confirms.",
    risk: "project",
    schema: WorktreeCreateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Arguments for worktree.createWithRecipe (recipe id, name, etc.).",
        },
        requestKey: { type: "string", description: "Optional idempotency key." },
      },
      required: ["arguments"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "worktree.createWithRecipe", args.arguments, args.requestKey);
    },
  },
  {
    name: "terminal.focus",
    description:
      "Focus a Daintree terminal in the UI (read-only side effect on the UI; no state mutation).",
    risk: "ui",
    schema: FocusArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalId: { type: "string", description: "Daintree terminal id to focus in the UI." },
      },
      required: ["terminalId"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "terminal.focus", { terminalId: args.terminalId });
    },
  },
];
