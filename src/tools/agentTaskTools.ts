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
      const prompt = `${args.taskPrompt}\n\n${CONSTRAINTS_BLOCK}`;
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

        const sc = res.structuredContent as { terminalId?: string } | undefined;
        const terminalId = sc?.terminalId;

        let watcherId: string | undefined;
        if (args.watcher?.create && terminalId) {
          const watcher = ctx.db.insertWatcher({
            kind: "terminal",
            title: `watch ${args.title}`,
            goal: args.watcher.goal ?? `Supervise: ${args.title}`,
            targetsJson: JSON.stringify([terminalId]),
            cadenceMs: args.watcher.cadenceMs ?? 120_000,
            modelTier: "small",
            nextCheckAt: Date.now(),
          });
          watcherId = watcher.id;
        }

        return ok(
          `Spawned ${agentId} for "${args.title}"${
            terminalId ? ` (terminal ${terminalId})` : ""
          }${watcherId ? `; watcher ${watcherId}` : ""}.`,
          {
            terminalId,
            worktreeId: args.worktreeId,
            ...(watcherId ? { watcherId } : {}),
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
