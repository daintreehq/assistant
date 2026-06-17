/**
 * agentTaskTools — the no-file-edit escape hatch.
 *
 * The CLI never edits files itself. When a task needs code changes, it spawns a
 * visible Daintree agent in a worktree (via the `agent.launch` MCP tool) and,
 * optionally, attaches a terminal watcher to supervise it. The agent prompt is
 * composed from the caller's task plus a standard constraints block so the agent
 * stays scoped to its worktree and reports back changed files, tests, and risks.
 */
import { z } from "zod";
import { randomUUID } from "node:crypto";
import { ok, fail, type ToolDef } from "./types.js";

/** Standard constraints appended to every spawned-agent prompt (docs §18). */
const CONSTRAINTS_BLOCK = [
  "Make changes only in this worktree. Do not modify unrelated files.",
  "Run relevant tests if practical.",
  "Report back changed files, tests run, remaining risks.",
  "If you need clarification, stop and ask.",
].join(" ");

/**
 * Compose the agent prompt from the task, any caller-supplied context hints
 * (relevant file paths, whether to include a diff, the target worktree), and the
 * standard constraints block. The schema accepts `context`, the recipe tells the
 * model to pass file paths — so we must actually fold them into the prompt.
 */
function buildAgentPrompt(args: SpawnForEditsArgs): string {
  const lines: string[] = [args.taskPrompt.trim()];
  const ctxLines: string[] = [];
  if (args.worktreeId) ctxLines.push(`Work in worktree: ${args.worktreeId}`);
  const files = args.context?.filePaths?.filter((f) => f.trim());
  if (files && files.length) {
    ctxLines.push(`Relevant files:\n${files.map((f) => `  - ${f}`).join("\n")}`);
  }
  if (args.context?.includeDiff) {
    ctxLines.push(
      "Review the current working-tree diff in this worktree before changing anything.",
    );
  }
  if (ctxLines.length) lines.push(`\nContext:\n${ctxLines.join("\n")}`);
  lines.push(`\n${CONSTRAINTS_BLOCK}`);
  return lines.join("\n");
}

/**
 * Robustly pull a named field from an MCP launch result. Daintree may return it
 * under structuredContent, nested under a `task`/`agent` object, or only in the
 * text body (e.g. "terminalId: term_3a") — check each so a watcher isn't dropped
 * just because the field wasn't where we first looked.
 */
function extractField(
  res: { structuredContent?: unknown; text?: string },
  key: string,
): string | undefined {
  const sc = res.structuredContent;
  if (sc && typeof sc === "object") {
    const obj = sc as Record<string, unknown>;
    const direct = obj[key];
    if (typeof direct === "string" && direct) return direct;
    for (const nestedKey of ["task", "agent", "result", "data"]) {
      const nested = obj[nestedKey];
      if (nested && typeof nested === "object") {
        const v = (nested as Record<string, unknown>)[key];
        if (typeof v === "string" && v) return v;
      }
    }
  }
  if (typeof res.text === "string") {
    const m = res.text.match(new RegExp(`"?${key}"?\\s*[:=]\\s*"?([\\w.-]+)"?`));
    if (m) return m[1];
  }
  return undefined;
}

const SpawnForEditsArgs = z.object({
  worktreeId: z
    .string()
    .optional()
    .describe("Worktree to run the agent in. Omit to let Daintree choose."),
  agentId: z
    .string()
    .optional()
    .describe('Agent to launch (default "claude").'),
  title: z.string().describe("Short title for the task and any watcher."),
  taskPrompt: z
    .string()
    .describe("The instructions for the agent. Constraints are appended automatically."),
  context: z
    .object({
      filePaths: z.array(z.string()).optional(),
      includeDiff: z.boolean().optional(),
    })
    .optional()
    .describe("Optional context hints for the agent."),
  watcher: z
    .object({
      create: z.boolean(),
      goal: z.string().optional(),
      cadenceMs: z.number().int().positive().optional(),
    })
    .optional()
    .describe("Optionally attach a terminal watcher to supervise the agent."),
});
type SpawnForEditsArgs = z.infer<typeof SpawnForEditsArgs>;

export const agentTaskTools: ToolDef[] = [
  {
    name: "agentTask.spawnForEdits",
    description:
      "Spawn a visible Daintree agent in a worktree to make code changes. The CLI never edits files itself — it delegates edits to a supervised agent. Optionally attaches a terminal watcher.",
    risk: "project",
    schema: SpawnForEditsArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        worktreeId: {
          type: "string",
          description: "Worktree to run the agent in. Omit to let Daintree choose.",
        },
        agentId: {
          type: "string",
          description: 'Agent to launch (default "claude").',
        },
        title: {
          type: "string",
          description: "Short title for the task and any watcher.",
        },
        taskPrompt: {
          type: "string",
          description:
            "The instructions for the agent. Constraints are appended automatically.",
        },
        context: {
          type: "object",
          additionalProperties: false,
          properties: {
            filePaths: { type: "array", items: { type: "string" } },
            includeDiff: { type: "boolean" },
          },
        },
        watcher: {
          type: "object",
          additionalProperties: false,
          properties: {
            create: { type: "boolean" },
            goal: { type: "string" },
            cadenceMs: { type: "number" },
          },
          required: ["create"],
        },
      },
      required: ["title", "taskPrompt"],
    },
    async handler(args: SpawnForEditsArgs, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected, so no agent can be spawned to make edits. Connect Daintree (set DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN) and retry.",
        );
      }

      const agentId = args.agentId ?? "claude";
      const prompt = buildAgentPrompt(args);
      const requestKey = randomUUID();

      try {
        const res = await ctx.mcp.callTool("agent.launch", {
          agentId,
          ...(args.worktreeId ? { worktreeId: args.worktreeId } : {}),
          prompt,
          requestKey,
        });
        if (res.isError) {
          return fail(
            "AGENT_LAUNCH_FAILED",
            `agent.launch reported an error: ${res.text || "(no detail)"}`,
            { details: res.structuredContent },
          );
        }

        const terminalId = extractField(res, "terminalId");
        const worktreeId = extractField(res, "worktreeId") ?? args.worktreeId;
        const taskId = extractField(res, "taskId");

        let watcherId: string | undefined;
        let watcherWarning: string | undefined;
        if (args.watcher?.create) {
          if (terminalId) {
            const watcher = ctx.db.insertWatcher({
              kind: "terminal",
              title: `watch ${args.title}`,
              goal: args.watcher.goal ?? `Supervise: ${args.title}`,
              targetsJson: JSON.stringify([terminalId]),
              cadenceMs: args.watcher.cadenceMs ?? 120_000,
              modelTier: "small",
              nextCheckAt: Date.now(),
              // Scope the post-completion git verification pass to this agent's
              // worktree so the cleanliness check reads the right tree.
              ...(worktreeId
                ? {
                    optionsJson: JSON.stringify({
                      verificationScope: { worktreeId },
                    }),
                  }
                : {}),
            });
            watcherId = watcher.id;
            if (!worktreeId) {
              // agent.launch doesn't return a worktreeId; without one the
              // post-completion git check falls back to the active worktree, which
              // may not be the agent's. Flag it so completion isn't silently
              // verified against the wrong tree.
              watcherWarning =
                "watcher created without a known worktreeId, so post-completion verification will use the active worktree context";
            }
          } else {
            // A watcher was requested but the launch response carried no terminal
            // id — surface this instead of silently dropping the supervision.
            watcherWarning =
              "watcher requested but agent.launch returned no terminalId, so no watcher was created";
          }
        }

        return ok(
          `Spawned ${agentId} for "${args.title}"${
            terminalId ? ` (terminal ${terminalId})` : ""
          }${watcherId ? `; watcher ${watcherId}` : ""}${
            watcherWarning ? ` — ${watcherWarning}` : ""
          }.`,
          {
            terminalId,
            worktreeId,
            ...(taskId ? { taskId } : {}),
            ...(watcherId ? { watcherId } : {}),
            ...(watcherWarning ? { watcherWarning } : {}),
          },
        );
      } catch (e) {
        return fail(
          "AGENT_LAUNCH_FAILED",
          `Could not spawn agent for "${args.title}": ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
