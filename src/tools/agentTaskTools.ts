/**
 * agentTaskTools — the no-file-edit escape hatch.
 *
 * The CLI never edits files itself, and it never spawns agents via a raw
 * `agent.launch` either. When a task needs code changes (mode "edit") OR a
 * read-only investigation delegated to a visible agent (mode "explore"), it
 * spawns a Daintree agent in a worktree (via the `agent.launch` MCP tool) and,
 * optionally, attaches a terminal watcher to supervise it. The agent prompt is
 * composed from the caller's task plus a mode-specific constraints block: edit
 * mode keeps the agent scoped to its worktree and reporting changed files/tests/
 * risks; explore mode forbids file changes and asks for findings only.
 */
import { z } from "zod";
import { randomUUID } from "node:crypto";
import { ok, fail, type ToolDef } from "./types.js";
import { SUPERVISOR_DEFAULT_CADENCE_MS } from "../watcherCadence.js";
import { logDebug } from "../debugLog.js";

/** Max length for the human-readable name passed to agent.launch (terminal/tab label). */
const AGENT_LAUNCH_NAME_MAX_LEN = 60;

/** Default agent id; used as the name prefix when the caller omits one. */
const DEFAULT_AGENT_ID = "claude";

/** Constraints appended to an edit-mode spawned-agent prompt (docs §18). */
const EDIT_CONSTRAINTS_BLOCK = [
  "Make changes only in this worktree. Do not modify unrelated files.",
  "Run relevant tests if practical.",
  "Report back changed files, tests run, remaining risks.",
  "If you need clarification, stop and ask.",
].join(" ");

/**
 * Constraints appended to an explore-mode spawned-agent prompt. The agent is
 * supervising a read-only investigation, so it must NOT touch files — only
 * report findings. This is what lets a "spawn an agent to explore X" request go
 * through this wrapper instead of a hand-rolled raw agent.launch.
 */
const EXPLORE_CONSTRAINTS_BLOCK = [
  "This is a READ-ONLY exploration: do not create, modify, or delete any files, and do not run commands that mutate state.",
  "Investigate and report back: the project's structure, key components, how the pieces fit together, and anything notable (risks, tech debt, surprises).",
  "If the task is ambiguous, state your assumptions and proceed; only stop to ask if you are genuinely blocked.",
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
  const constraints =
    args.mode === "explore" ? EXPLORE_CONSTRAINTS_BLOCK : EDIT_CONSTRAINTS_BLOCK;
  lines.push(`\n${constraints}`);
  return lines.join("\n");
}

/**
 * Derive a short, human-readable name for the spawned agent's terminal/tab in the
 * canonical "<Agent>: <task>" format (e.g. "Claude: auth refactor") so parallel
 * agents stay distinguishable at a glance in Daintree's UI. The prefix is always
 * the launching agent id with its first letter capitalized — including the default
 * "claude" — and the task half is the caller's title with whitespace collapsed,
 * falling back to "task" when blank. The whole label is hard-capped at
 * AGENT_LAUNCH_NAME_MAX_LEN, truncating the task half so the "<Agent>: " prefix
 * always survives.
 */
function buildAgentLaunchName(title: string, agentId: string): string {
  const id = agentId.trim() || DEFAULT_AGENT_ID;
  const prefix = `${id.charAt(0).toUpperCase()}${id.slice(1)}: `;
  const task = title.trim().replace(/\s+/g, " ") || "task";
  const room = Math.max(0, AGENT_LAUNCH_NAME_MAX_LEN - prefix.length);
  const head = task.length > room ? task.slice(0, room) : task;
  // Final hard cap so the invariant holds even for a pathologically long agentId.
  return `${prefix}${head}`.slice(0, AGENT_LAUNCH_NAME_MAX_LEN);
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
  mode: z
    .enum(["edit", "explore"])
    .optional()
    .describe(
      'Spawn intent (default "edit"). "edit" tells the agent to make code changes; "explore" tells it to investigate read-only and not touch any files.',
    ),
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
      "Spawn a visible Daintree agent in a worktree. Use mode:\"edit\" (default) to make code changes, or mode:\"explore\" for a read-only investigation (the agent is told not to touch files). This is the ONLY way to spawn an agent — never hand-roll a raw agent.launch via daintree.call. The CLI never edits files itself. Optionally attaches a terminal watcher.",
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
        mode: {
          type: "string",
          enum: ["edit", "explore"],
          description:
            'Spawn intent (default "edit"). "edit" tells the agent to make code changes; "explore" tells it to investigate read-only and not touch any files.',
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

      const agentId = args.agentId?.trim() || DEFAULT_AGENT_ID;
      const name = buildAgentLaunchName(args.title, agentId);
      const prompt = buildAgentPrompt(args);
      const requestKey = randomUUID();

      try {
        const res = await ctx.mcp.callTool("agent.launch", {
          agentId,
          name,
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
        // Daintree gap: agent.launch returns only { terminalId, location } — it
        // never carries worktreeId/taskId, so these reads degrade gracefully to
        // the caller-supplied worktreeId / undefined. Tracked in docs/DAINTREE_MCP.md
        // ("Known Daintree-side gaps") and issue #9; revisit if Daintree adds them.
        const worktreeId = extractField(res, "worktreeId") ?? args.worktreeId;
        const taskId = extractField(res, "taskId");

        logDebug(ctx.config, "spawn.launched", {
          via: "agentTask.spawnForEdits",
          agentId,
          mode: args.mode ?? "edit",
          name,
          title: args.title,
          terminalId,
          worktreeId,
          taskId,
          requestKey,
          watcherRequested: Boolean(args.watcher?.create),
        });

        let watcherId: string | undefined;
        let watcherWarning: string | undefined;
        if (args.watcher?.create) {
          if (terminalId) {
            const watcher = ctx.db.insertWatcher({
              kind: "terminal",
              title: `watch ${args.title}`,
              goal: args.watcher.goal ?? `Supervise: ${args.title}`,
              targetsJson: JSON.stringify([terminalId]),
              cadenceMs: args.watcher.cadenceMs ?? SUPERVISOR_DEFAULT_CADENCE_MS,
              isSupervisor: true,
              modelTier: "small",
              nextCheckAt: Date.now(),
              // Record the spawn mode so the watcher can tell a one-shot explore
              // agent idling at the prompt (end-of-turn, = completion) from an edit
              // agent genuinely waiting for input. Always set — a watcher created
              // without a known worktreeId still needs the mode. Scope the
              // post-completion git verification pass to this agent's worktree (when
              // known) so the cleanliness check reads the right tree.
              optionsJson: JSON.stringify({
                ...(worktreeId ? { verificationScope: { worktreeId } } : {}),
                spawnMode: args.mode ?? "edit",
              }),
            });
            watcherId = watcher.id;
            logDebug(ctx.config, "watcher.created", {
              watcherId: watcher.id,
              kind: "terminal",
              isSupervisor: true,
              via: "agentTask.spawnForEdits",
              agentId,
              mode: args.mode ?? "edit",
              title: watcher.title,
              goal: watcher.goal,
              targets: [terminalId],
              worktreeId,
              cadenceMs: watcher.cadenceMs,
              modelTier: watcher.modelTier,
              nextCheckAt: watcher.nextCheckAt,
            });
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
            logDebug(ctx.config, "watcher.create_skipped", {
              via: "agentTask.spawnForEdits",
              reason: "no terminalId from agent.launch",
              agentId,
              title: args.title,
            });
          }
        }

        // When a supervising watcher was created, surface the same
        // foreground-only lifecycle caveat the watcher tools emit: it pauses
        // when the assistant is closed and resumes on the next launch.
        const lifecycleNote = watcherId
          ? (ctx.daemonActive ? ctx.daemonActive() : true)
            ? " NOTE: supervision runs only while this assistant is open; this watcher pauses when you close the assistant and resumes on the next launch."
            : " NOTE: no scheduler is running in this session, so this watcher will not check until the assistant runs interactively."
          : "";

        return ok(
          `Spawned ${agentId} for "${args.title}"${
            terminalId ? ` (terminal ${terminalId})` : ""
          }${watcherId ? `; watcher ${watcherId}` : ""}${
            watcherWarning ? ` — ${watcherWarning}` : ""
          }.${lifecycleNote}`,
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
