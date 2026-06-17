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

const ListToolsArgs = z.object({}).strict();

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
];
