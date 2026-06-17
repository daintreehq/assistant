/**
 * Context tools: compact main-thread snapshots and cheap terminal summaries.
 *
 * These are read-only orchestration helpers. They keep the main model's context
 * clean by collapsing Daintree state and raw terminal scrollback into terse,
 * structured digests instead of dumping everything inline. `context.snapshot`
 * is best-effort and must never throw even when Daintree MCP is down;
 * `terminal.summarize` fails cleanly if MCP is unavailable.
 */
import { z } from "zod";
import { ok, fail, NO_ARGS, type ToolDef } from "./types.js";
import {
  SUMMARIZER_SYSTEM_PROMPT,
  buildSummarizerUserPrompt,
} from "../models/prompts/index.js";

/** Best-effort MCP call: returns the result text or undefined if it fails. */
async function tryCall(
  ctx: Parameters<ToolDef["handler"]>[1],
  name: string,
  args: Record<string, unknown> = {},
): Promise<{ text: string; structuredContent?: unknown } | undefined> {
  if (!ctx.mcp.isConnected()) return undefined;
  try {
    const res = await ctx.mcp.callTool(name, args);
    if (res.isError) return undefined;
    return { text: res.text, structuredContent: res.structuredContent };
  } catch {
    return undefined;
  }
}

const SummarizeArgs = z.object({
  terminalId: z.string().describe("Daintree terminal id to summarize."),
  purpose: z
    .string()
    .optional()
    .describe("What this summary is for (focuses the model)."),
  tailBytes: z
    .number()
    .int()
    .positive()
    .max(100_000)
    .optional()
    .describe("Max characters of terminal tail to summarize."),
});
type SummarizeArgs = z.infer<typeof SummarizeArgs>;

export const contextTools: ToolDef[] = [
  {
    name: "context.snapshot",
    description:
      "Build a compact snapshot of the current workspace: Daintree MCP status, and (when connected) action context, worktrees, terminals, plus the open attention queue. Best-effort and read-only; degrades gracefully when Daintree is offline.",
    risk: "read",
    readOnly: true,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      const mcp = ctx.mcp.status();

      // Best-effort Daintree reads — each wrapped, skipped when disconnected.
      const actionContext = await tryCall(ctx, "actions.getContext");
      const worktrees = await tryCall(ctx, "worktree.list");
      const terminals = await tryCall(ctx, "terminal.list");

      // Open attention queue (CLI-local, always available).
      const inbox = ctx.queue.digest({
        severityAtLeast: "attention",
        maxItems: 10,
      });
      const inboxText = ctx.queue.format(inbox);

      const lines: string[] = [];
      lines.push(
        `Daintree MCP: ${mcp.connected ? "connected" : "disconnected"}` +
          (mcp.transport ? ` (${mcp.transport})` : "") +
          (mcp.toolCount != null ? `, ${mcp.toolCount} tools` : "") +
          (!mcp.connected && mcp.error ? ` — ${mcp.error}` : ""),
      );
      if (!mcp.connected) {
        lines.push(
          "Degraded local mode: worktree/terminal/action context unavailable until Daintree connects.",
        );
      } else {
        lines.push(`Action context: ${actionContext ? "available" : "unavailable"}`);
        lines.push(`Worktrees: ${worktrees ? "available" : "unavailable"}`);
        lines.push(`Terminals: ${terminals ? "available" : "unavailable"}`);
      }
      lines.push(
        `Inbox (attention+): ${inbox.length} open event${inbox.length === 1 ? "" : "s"}`,
      );
      if (inbox.length > 0) lines.push(inboxText);

      return ok(lines.join("\n"), {
        mcp,
        actionContext: actionContext
          ? { text: actionContext.text, structuredContent: actionContext.structuredContent }
          : undefined,
        worktrees: worktrees
          ? { text: worktrees.text, structuredContent: worktrees.structuredContent }
          : undefined,
        terminals: terminals
          ? { text: terminals.text, structuredContent: terminals.structuredContent }
          : undefined,
        inbox,
      });
    },
  },
  {
    name: "terminal.summarize",
    description:
      "Read a bounded tail of a Daintree terminal's output and summarize it with the small model. Use this instead of dumping raw scrollback into context. Read-only; requires Daintree MCP.",
    risk: "read",
    readOnly: true,
    schema: SummarizeArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalId: { type: "string", description: "Daintree terminal id to summarize." },
        purpose: {
          type: "string",
          description: "What this summary is for (focuses the model).",
        },
        tailBytes: {
          type: "number",
          description: "Max characters of terminal tail to summarize.",
        },
      },
      required: ["terminalId"],
    },
    async handler(args: SummarizeArgs, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected, so terminal output cannot be read.",
          { recoverable: true },
        );
      }

      let tail: string;
      try {
        const out = await ctx.mcp.callTool("terminal.getOutput", {
          terminalId: args.terminalId,
          maxLines: 200,
        });
        if (out.isError) {
          return fail(
            "TERMINAL_OUTPUT",
            `Could not read output for terminal ${args.terminalId}: ${out.text || "terminal returned an error"}`,
          );
        }
        // Scrollback is in structuredContent.content; text is JSON-serialized.
        const sc = (out.structuredContent ?? {}) as Record<string, unknown>;
        tail = typeof sc.content === "string" ? sc.content : out.text;
      } catch (e) {
        return fail(
          "TERMINAL_OUTPUT",
          `Could not read output for terminal ${args.terminalId}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }

      if (args.tailBytes && tail.length > args.tailBytes) {
        tail = tail.slice(-args.tailBytes);
      }

      const purpose = args.purpose ?? `Summarize terminal ${args.terminalId} for the supervisor.`;

      try {
        const res = await ctx.router.chat("small", {
          messages: [
            { role: "system", content: SUMMARIZER_SYSTEM_PROMPT },
            { role: "user", content: buildSummarizerUserPrompt({ purpose, tail }) },
          ],
          maxTokens: 300,
        });
        const summary = res.content.trim() || "(no summary produced)";
        return ok(summary, {
          terminalId: args.terminalId,
          purpose,
          summary,
        });
      } catch (e) {
        return fail(
          "SUMMARIZE",
          `Failed to summarize terminal ${args.terminalId}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
