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
    const res = await ctx.mcp.callTool(name, args, ctx.signal);
    if (res.isError) return undefined;
    return { text: res.text, structuredContent: res.structuredContent };
  } catch {
    // Best-effort: any failure (including a user abort tearing the request down)
    // degrades this read to "unavailable". context.snapshot must never throw; the
    // agent loop catches the abort after dispatch returns and ends the turn cleanly.
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

const ReadArgs = z.object({
  terminalId: z.string().describe("Daintree terminal id to read."),
  maxLines: z
    .number()
    .int()
    .positive()
    .max(1000)
    .default(200)
    .describe("Max trailing lines of scrollback to return (1–1000)."),
  tailBytes: z
    .number()
    .int()
    .positive()
    .max(100_000)
    .optional()
    .describe("Further cap the returned text to the last N characters."),
});
type ReadArgs = z.infer<typeof ReadArgs>;

/** Read a terminal's scrollback tail via terminal.getOutput, no model involved. */
async function readTerminalTail(
  ctx: Parameters<ToolDef["handler"]>[1],
  terminalId: string,
  maxLines: number,
): Promise<{ ok: true; content: string } | { ok: false; error: string }> {
  try {
    const out = await ctx.mcp.callTool(
      "terminal.getOutput",
      { terminalId, maxLines },
      ctx.signal,
    );
    if (out.isError) {
      return { ok: false, error: out.text || "terminal returned an error" };
    }
    // Scrollback may arrive in structuredContent.content OR the raw text body
    // (Daintree uses the latter) — read both, falling back to raw text.
    const sc = (out.structuredContent ?? {}) as Record<string, unknown>;
    const content = typeof sc.content === "string" ? sc.content : out.text;
    return { ok: true, content };
  } catch (e) {
    return {
      ok: false,
      error: e instanceof Error ? e.message : String(e),
    };
  }
}

export const contextTools: ToolDef[] = [
  {
    name: "context.snapshot",
    description:
      "Build a compact snapshot of the current workspace: Daintree MCP status, and (when connected) action context, worktrees, terminals, plus the open attention queue. Best-effort and read-only; degrades gracefully when Daintree is offline.",
    risk: "read",
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

      if (ctx.signal?.aborted) {
        return fail("CANCELLED", "Turn cancelled while reading terminal output.", {
          recoverable: false,
        });
      }
      const read = await readTerminalTail(ctx, args.terminalId, 200);
      if (!read.ok) {
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", "Turn cancelled while reading terminal output.", {
            recoverable: false,
          });
        }
        return fail(
          "TERMINAL_OUTPUT",
          `Could not read output for terminal ${args.terminalId}: ${read.error}`,
        );
      }
      let tail = read.content;

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
          maxTokens: 512,
          signal: ctx.signal,
        });
        // Same truncation-legibility contract as terminal.extract: a "length"
        // finishReason means the small model hit its cap mid-summary, so the text
        // is cut off. Lead with the warning (the serializer head-truncates an
        // oversized summary) and flag it, so the caller doesn't read a partial
        // summary as complete — and knows terminal.read gives the full raw text.
        const truncated = res.finishReason === "length";
        const body = res.content.trim() || "(no summary produced)";
        const note = truncated
          ? "⚠ This summary is cut off: the summarizer hit its token cap. For the complete text use terminal.read (raw scrollback, no model, no cap).\n\n"
          : "";
        const summary = note + body;
        return ok(summary, {
          terminalId: args.terminalId,
          purpose,
          truncated,
          summary,
        });
      } catch (e) {
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", "Turn cancelled while summarizing terminal.", {
            recoverable: false,
          });
        }
        return fail(
          "SUMMARIZE",
          `Failed to summarize terminal ${args.terminalId}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "terminal.read",
    description:
      "Read a terminal's raw scrollback tail VERBATIM — no model, no summarization, no token cap. Use this to relay exactly what an agent said, or when you need the literal text. Prefer this over terminal.summarize/terminal.extract whenever you want the output reproduced rather than interpreted: those route through a small model that paraphrases and can truncate. Read-only; requires Daintree MCP.",
    risk: "read",
    schema: ReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalId: { type: "string", description: "Daintree terminal id to read." },
        maxLines: {
          type: "number",
          description: "Max trailing lines of scrollback to return (1–1000, default 200).",
        },
        tailBytes: {
          type: "number",
          description: "Further cap the returned text to the last N characters.",
        },
      },
      required: ["terminalId"],
    },
    async handler(args: ReadArgs, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected, so terminal output cannot be read.",
          { recoverable: true },
        );
      }
      if (ctx.signal?.aborted) {
        return fail("CANCELLED", "Turn cancelled while reading terminal output.", {
          recoverable: false,
        });
      }
      const read = await readTerminalTail(ctx, args.terminalId, args.maxLines);
      if (!read.ok) {
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", "Turn cancelled while reading terminal output.", {
            recoverable: false,
          });
        }
        return fail(
          "TERMINAL_OUTPUT",
          `Could not read output for terminal ${args.terminalId}: ${read.error}`,
        );
      }
      let content = read.content;
      if (args.tailBytes && content.length > args.tailBytes) {
        content = content.slice(-args.tailBytes);
      }
      return ok(content || "(no output captured)", {
        terminalId: args.terminalId,
        content,
        lineCount: content ? content.split("\n").length : 0,
      });
    },
  },
];
