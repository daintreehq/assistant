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
import { createHash } from "node:crypto";
import { ok, fail, type ToolContext, type ToolDef } from "./types.js";
import type { AgentLaunchRecord, ToolResult } from "../schemas.js";
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
 * standard constraints block. The schema accepts `context`, the skill tells the
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
  // The acceptance contract is what completion is judged against (issue #83), so the
  // agent must see it up front — it states what "done" means for this task.
  const criteria = args.acceptanceCriteria?.trim();
  if (criteria) {
    lines.push(
      `\nAcceptance criteria (your work is verified against these — state clearly when each is met):\n${criteria}`,
    );
  }
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

/**
 * Deterministic idempotency key for a launch, derived from the task's *identity*
 * — not a per-call random UUID. A retry of the same logical task therefore hashes
 * to the same key, so it reconciles with the in-flight record instead of spawning
 * a second agent. We hash only the fields that make a launch distinct:
 * `taskPrompt`, `worktreeId`, `agentId`, `mode`. The `title` is display-only and
 * `context` (file hints / includeDiff) is guidance for the agent's environment,
 * not what makes this a different launch — both are excluded so a retry that
 * tweaks them still dedupes. Keys are sorted for a canonical JSON before hashing
 * (the input is a flat object, so a key-sort is sufficient — no recursive
 * stable-stringify needed). 16 hex chars (64 bits) is ample per session.
 */
function computeIdempotencyKey(parts: {
  taskPrompt: string;
  worktreeId: string;
  agentId: string;
  mode: string;
}): string {
  const canonical = JSON.stringify(
    Object.fromEntries(Object.entries(parts).sort(([a], [b]) => a.localeCompare(b))),
  );
  return createHash("sha256").update(canonical, "utf8").digest("hex").slice(0, 16);
}

/** One terminal as reported by `terminal.list`, with the fields reconciliation
 * needs. Unlike watcherEngine's `readTerminalList` (which keeps only liveness),
 * this preserves `name`/`agentId`/`worktreeId` so an ambiguous launch can be
 * matched back to its terminal. */
interface ReconcileTerminal {
  id: string;
  name?: string;
  agentId?: string;
  worktreeId?: string;
}

/** Parse `terminal.list` entries from either the structured payload or a JSON
 * `text` body (Daintree may populate either), preserving the reconciliation
 * fields. Never throws — returns [] on any unreadable shape. */
function parseTerminalList(res: {
  structuredContent?: unknown;
  text?: string;
}): ReconcileTerminal[] {
  const entries: unknown[] = [];
  const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
  if (Array.isArray(sc.terminals)) entries.push(...sc.terminals);
  if (typeof res.text === "string" && res.text.trim()) {
    try {
      const parsed = JSON.parse(res.text) as { terminals?: unknown };
      if (Array.isArray(parsed?.terminals)) entries.push(...parsed.terminals);
    } catch {
      /* not JSON — ignore this source */
    }
  }
  const out: ReconcileTerminal[] = [];
  for (const t of entries) {
    if (!t || typeof t !== "object") continue;
    const e = t as Record<string, unknown>;
    const id =
      typeof e.id === "string"
        ? e.id
        : typeof e.terminalId === "string"
          ? e.terminalId
          : undefined;
    if (!id) continue;
    out.push({
      id,
      name: typeof e.name === "string" ? e.name : undefined,
      agentId: typeof e.agentId === "string" ? e.agentId : undefined,
      worktreeId: typeof e.worktreeId === "string" ? e.worktreeId : undefined,
    });
  }
  return out;
}

/**
 * Answer "did the previous attempt actually start an agent?" for an ambiguous
 * launch. We read the authoritative `terminal.list` inventory and look for a
 * terminal whose `name` exactly matches the deterministic launch name we sent.
 * Name alone can in theory collide, so when the inventory carries `agentId` /
 * `worktreeId` we additionally require those to match (a free, stricter check —
 * the launch name is capped at 60 chars so we deliberately do NOT mutate it with
 * a key suffix). Returns the bound terminalId, or undefined when no confident
 * match exists (the launch stays ambiguous). Never throws.
 */
async function reconcileViaTerminalList(
  ctx: ToolContext,
  name: string,
  agentId: string,
  worktreeId: string | undefined,
): Promise<string | undefined> {
  try {
    const res = await ctx.mcp.callTool("terminal.list", {});
    if (res.isError) return undefined;
    const terminals = parseTerminalList(res);
    const matches = terminals.filter(
      (t) =>
        t.name === name &&
        (t.agentId == null || t.agentId === agentId) &&
        (worktreeId == null || t.worktreeId == null || t.worktreeId === worktreeId),
    );
    // Bind only on an UNAMBIGUOUS match. Two terminals sharing the deterministic
    // launch name (same title+agent for genuinely different tasks) can't be told
    // apart, so a multi-match is itself ambiguous — don't risk binding the wrong one.
    return matches.length === 1 ? matches[0].id : undefined;
  } catch {
    return undefined;
  }
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
  acceptanceCriteria: z
    .string()
    .optional()
    .describe(
      "Task-specific contract that defines 'done'. When set on an edit-mode task, a supervising watcher verifies completion against these criteria (not git cleanliness alone) before reporting success — so thin evidence is never upgraded to success. Provide it whenever there is a concrete, checkable definition of done. (Ignored for mode:\"explore\", which is read-only and ends at the prompt.)",
    ),
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

/**
 * Finish a launch that is bound to a terminal: attach the supervising watcher (if
 * one was requested and not already attached), advance the saga record to its
 * terminal stage, and build the success result. Shared by the fresh-launch path,
 * the idempotent-retry path (an in-flight record already has a terminal), and the
 * ambiguous-reconciliation path — so all three return an identical `ok()` shape.
 *
 * Watcher attachment is best-effort: if `insertWatcher` throws, the agent is still
 * running, so we do NOT fail the launch — we return ok() with a `watcherWarning`
 * and leave the record at `terminal_bound` (non-terminal) so a later retry can
 * re-attach the watcher. A successful (or not-requested) watcher advances the
 * record to `confirmed`.
 */
function finishBoundLaunch(
  ctx: ToolContext,
  args: SpawnForEditsArgs,
  record: AgentLaunchRecord,
  terminalId: string,
  worktreeId: string | undefined,
  taskId: string | undefined,
  kind: "fresh" | "idempotent" | "reconciled",
): ToolResult {
  const agentId = record.agentId;
  let watcherId = record.watcherId;
  let watcherWarning: string | undefined;

  if (args.watcher?.create && !watcherId) {
    try {
      const watcher = ctx.db.insertWatcher({
        kind: "terminal",
        title: `watch ${args.title}`,
        goal: args.watcher.goal ?? `Supervise: ${args.title}`,
        targetsJson: JSON.stringify([terminalId]),
        cadenceMs: args.watcher.cadenceMs ?? SUPERVISOR_DEFAULT_CADENCE_MS,
        isSupervisor: true,
        modelTier: "small",
        nextCheckAt: Date.now(),
        // Record the spawn mode so the watcher can tell a one-shot explore agent
        // idling at the prompt (end-of-turn, = completion) from an edit agent
        // genuinely waiting for input. Always set. Scope the post-completion git
        // verification pass to this agent's worktree (when known).
        optionsJson: JSON.stringify({
          ...(worktreeId ? { verificationScope: { worktreeId } } : {}),
          spawnMode: args.mode ?? "edit",
          // Persist the acceptance contract so the supervisor gates completion on
          // evidence the work was actually done, not git cleanliness alone.
          ...(args.acceptanceCriteria?.trim()
            ? { acceptanceCriteria: args.acceptanceCriteria.trim() }
            : {}),
        }),
      });
      watcherId = watcher.id;
      ctx.db.updateAgentLaunch(record.id, { stage: "watcher_attached", watcherId });
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
        // post-completion git check falls back to the active worktree, which may
        // not be the agent's. Flag it so completion isn't silently verified
        // against the wrong tree.
        watcherWarning =
          "watcher created without a known worktreeId, so post-completion verification will use the active worktree context";
      }
    } catch (e) {
      // Watcher bookkeeping failed, but the agent IS running — surface the gap
      // instead of failing a successful launch. The record stays terminal_bound
      // so a retry can re-attach.
      watcherWarning = `watcher could not be attached: ${e instanceof Error ? e.message : String(e)}`;
      logDebug(ctx.config, "watcher.create_failed", {
        via: "agentTask.spawnForEdits",
        agentId,
        title: args.title,
        error: watcherWarning,
      });
    }
  }

  // A watcher that was requested but couldn't be attached leaves the saga
  // recoverable (terminal_bound); everything else is fully settled.
  const settled = !args.watcher?.create || Boolean(watcherId);
  ctx.db.updateAgentLaunch(record.id, { stage: settled ? "confirmed" : "terminal_bound" });

  // When a supervising watcher exists, surface the same foreground-only lifecycle
  // caveat the watcher tools emit: it is discarded when the assistant is closed
  // and does not resume on the next launch (watchers are session-scoped).
  const lifecycleNote = watcherId
    ? (ctx.daemonActive ? ctx.daemonActive() : true)
      ? " NOTE: supervision runs only while this assistant is open; this watcher is discarded when you close the assistant and does not resume on the next launch (watchers are session-scoped)."
      : " NOTE: no scheduler is running in this session, so this watcher will not check until the assistant runs interactively."
    : "";

  const verb =
    kind === "idempotent"
      ? "Reused running"
      : kind === "reconciled"
        ? "Recovered"
        : "Spawned";

  return ok(
    `${verb} ${agentId} for "${args.title}" (terminal ${terminalId})${
      watcherId ? `; watcher ${watcherId}` : ""
    }${watcherWarning ? ` — ${watcherWarning}` : ""}.${lifecycleNote}`,
    {
      launchId: record.id,
      terminalId,
      worktreeId,
      ...(taskId ? { taskId } : {}),
      ...(watcherId ? { watcherId } : {}),
      ...(watcherWarning ? { watcherWarning } : {}),
    },
  );
}

export const agentTaskTools: ToolDef[] = [
  {
    name: "agentTask.spawnForEdits",
    description:
      "Spawn a visible Daintree agent in a worktree. Use mode:\"edit\" (default) to make code changes, or mode:\"explore\" for a read-only investigation (the agent is told not to touch files). This is the ONLY way to spawn an agent — never hand-roll a raw agent.launch via daintree.call. The CLI never edits files itself. Optionally attaches a terminal watcher.",
    consequence:
      "Opens a visible agent terminal in a worktree that can edit project files. Changes stay in the worktree until you commit them.",
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
        acceptanceCriteria: {
          type: "string",
          description:
            "Task-specific contract that defines 'done'. When set on an edit-mode task, a supervising watcher verifies completion against these criteria (not git cleanliness alone) before reporting success. Provide it whenever there is a concrete, checkable definition of done. Ignored for mode:\"explore\".",
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

      // The user already cancelled before we issued the launch — don't spawn an
      // agent the turn no longer wants.
      if (ctx.signal?.aborted) {
        return fail("CANCELLED", "Turn cancelled before the agent was launched.", {
          recoverable: false,
        });
      }

      const agentId = args.agentId?.trim() || DEFAULT_AGENT_ID;
      const mode = args.mode ?? "edit";
      const name = buildAgentLaunchName(args.title, agentId);
      const prompt = buildAgentPrompt(args);
      // Normalize the worktree once: an explicit empty string must be treated like
      // an omitted worktree so it doesn't hash to a different idempotency key (and
      // isn't forwarded as a bogus worktreeId to agent.launch).
      const worktreeId = args.worktreeId?.trim() || undefined;
      // Deterministic key over the task's identity, so a retry reconciles with the
      // in-flight saga record instead of launching a second agent. Reused as the
      // agent.launch requestKey so Daintree can dedupe on its side too.
      const idempotencyKey = computeIdempotencyKey({
        taskPrompt: args.taskPrompt,
        worktreeId: worktreeId ?? "",
        agentId,
        mode,
      });

      // --- Idempotent retry: is there a live launch saga for this exact task? ---
      // Only non-terminal records match (cancelStaleAgentLaunches retires prior
      // sessions' rows on DB open), so a completed task can still be re-run later.
      const existing = ctx.db.findActiveAgentLaunch(idempotencyKey);
      if (existing) {
        if (existing.terminalId) {
          // A prior attempt already bound a terminal — the agent is running. Don't
          // launch again; finish any outstanding watcher step and return success.
          logDebug(ctx.config, "spawn.idempotent_hit", {
            via: "agentTask.spawnForEdits",
            launchId: existing.id,
            idempotencyKey,
            stage: existing.stage,
            terminalId: existing.terminalId,
          });
          return finishBoundLaunch(
            ctx,
            args,
            existing,
            existing.terminalId,
            existing.worktreeId ?? worktreeId,
            undefined,
            "idempotent",
          );
        }
        // In-flight but unbound (ambiguous, or launch_requested/agent_started left
        // by a crash): we don't know if an agent started. Reconcile before retrying.
        const reconciled = await reconcileViaTerminalList(
          ctx,
          existing.name,
          agentId,
          existing.worktreeId ?? worktreeId,
        );
        if (reconciled) {
          ctx.db.updateAgentLaunch(existing.id, {
            stage: "terminal_bound",
            terminalId: reconciled,
            errorCode: undefined,
            errorMessage: undefined,
          });
          logDebug(ctx.config, "spawn.reconciled", {
            via: "agentTask.spawnForEdits",
            launchId: existing.id,
            idempotencyKey,
            terminalId: reconciled,
          });
          return finishBoundLaunch(
            ctx,
            args,
            existing,
            reconciled,
            existing.worktreeId ?? worktreeId,
            undefined,
            "reconciled",
          );
        }
        // Reconciliation found no matching terminal — this explicit retry is the
        // caller's signal to try again, and we just confirmed no agent is running
        // under this identity. Retire the dead-end record as failed (so it stops
        // blocking) and fall through to a fresh launch, rather than deadlocking on
        // `ambiguous` until the session restarts.
        ctx.db.updateAgentLaunch(existing.id, {
          stage: "failed",
          errorCode: "LAUNCH_NOT_FOUND",
          errorMessage:
            "retry found no matching terminal; retired so a fresh launch can proceed",
        });
        logDebug(ctx.config, "spawn.retire_unresolved", {
          via: "agentTask.spawnForEdits",
          launchId: existing.id,
          idempotencyKey,
          priorStage: existing.stage,
        });
      }

      // --- Fresh launch: write the saga record BEFORE the side-effecting call ---
      // (write-ahead) so a crash mid-launch leaves a recoverable record, not a
      // ghost agent.
      const record = ctx.db.insertAgentLaunch({
        idempotencyKey,
        agentId,
        worktreeId,
        mode,
        title: args.title,
        name,
        stage: "launch_requested",
      });

      let res: Awaited<ReturnType<ToolContext["mcp"]["callTool"]>>;
      try {
        res = await ctx.mcp.callTool(
          "agent.launch",
          {
            agentId,
            name,
            ...(worktreeId ? { worktreeId } : {}),
            prompt,
            requestKey: idempotencyKey,
          },
          // Forward the turn's signal so pressing Escape aborts the in-flight
          // agent.launch MCP call rather than letting it run to completion.
          ctx.signal,
        );
      } catch (e) {
        // If the user cancelled mid-launch, the signal aborts and callTool rejects.
        // That is a cancellation, not an ambiguous launch — map it to CANCELLED
        // (matching the pre-launch abort check above) instead of treating an aborted
        // request as a transport failure that needs reconciliation.
        if (ctx.signal?.aborted) {
          ctx.db.updateAgentLaunch(record.id, {
            stage: "failed",
            errorCode: "CANCELLED",
            errorMessage: "Turn cancelled during agent launch.",
          });
          return fail("CANCELLED", "Turn cancelled during agent launch.", {
            details: { launchId: record.id },
          });
        }
        // The transport threw — the request MAY have reached Daintree, so this is
        // ambiguous, not a clean failure. Mark it and try to reconcile.
        const msg = e instanceof Error ? e.message : String(e);
        ctx.db.updateAgentLaunch(record.id, {
          stage: "ambiguous",
          errorCode: "AGENT_LAUNCH_THREW",
          errorMessage: msg,
        });
        const reconciled = await reconcileViaTerminalList(ctx, name, agentId, worktreeId);
        if (reconciled) {
          ctx.db.updateAgentLaunch(record.id, {
            stage: "terminal_bound",
            terminalId: reconciled,
            errorCode: undefined,
            errorMessage: undefined,
          });
          return finishBoundLaunch(ctx, args, record, reconciled, worktreeId, undefined, "reconciled");
        }
        return fail(
          "AGENT_LAUNCH_AMBIGUOUS",
          `Could not confirm whether an agent for "${args.title}" started (transport error: ${msg}). Check Daintree's terminals before retrying.`,
          { recoverable: true, details: { launchId: record.id } },
        );
      }

      if (res.isError) {
        // An explicit error response — the launch genuinely failed (terminal).
        const detail = res.text || "(no detail)";
        ctx.db.updateAgentLaunch(record.id, {
          stage: "failed",
          errorCode: "AGENT_LAUNCH_FAILED",
          errorMessage: detail,
        });
        return fail(
          "AGENT_LAUNCH_FAILED",
          `agent.launch reported an error: ${detail}`,
          { details: res.structuredContent },
        );
      }

      ctx.db.updateAgentLaunch(record.id, { stage: "agent_started" });

      const terminalId = extractField(res, "terminalId");
      // Daintree gap: agent.launch returns only { terminalId, location } — it
      // never carries worktreeId/taskId, so these reads degrade gracefully to the
      // caller-supplied worktreeId / undefined. Tracked in docs/DAINTREE_MCP.md
      // ("Known Daintree-side gaps") and issue #9; revisit if Daintree adds them.
      const resolvedWorktreeId = extractField(res, "worktreeId") ?? worktreeId;
      const taskId = extractField(res, "taskId");

      logDebug(ctx.config, "spawn.launched", {
        via: "agentTask.spawnForEdits",
        agentId,
        mode,
        name,
        title: args.title,
        terminalId,
        worktreeId: resolvedWorktreeId,
        taskId,
        idempotencyKey,
        launchId: record.id,
        watcherRequested: Boolean(args.watcher?.create),
      });

      if (!terminalId) {
        // No terminalId means we DON'T KNOW whether an agent started — ambiguous,
        // not a success. Mark it, attempt reconciliation, and only return a clean
        // result if we can confidently bind a terminal; otherwise fail recoverably
        // so the caller can reconcile/retry rather than assume success.
        ctx.db.updateAgentLaunch(record.id, {
          stage: "ambiguous",
          errorCode: "NO_TERMINAL_ID",
          errorMessage: "agent.launch returned no terminalId",
        });
        const reconciled = await reconcileViaTerminalList(ctx, name, agentId, resolvedWorktreeId);
        if (reconciled) {
          ctx.db.updateAgentLaunch(record.id, {
            stage: "terminal_bound",
            terminalId: reconciled,
            errorCode: undefined,
            errorMessage: undefined,
          });
          return finishBoundLaunch(ctx, args, record, reconciled, resolvedWorktreeId, taskId, "reconciled");
        }
        logDebug(ctx.config, "spawn.ambiguous", {
          via: "agentTask.spawnForEdits",
          launchId: record.id,
          idempotencyKey,
          reason: "no terminalId and no reconciling terminal",
        });
        return fail(
          "AGENT_LAUNCH_AMBIGUOUS",
          `agent.launch for "${args.title}" returned no terminalId, so it is unknown whether an agent started. Check Daintree's terminals before retrying.`,
          { recoverable: true, details: { launchId: record.id } },
        );
      }

      ctx.db.updateAgentLaunch(record.id, { stage: "terminal_bound", terminalId });
      return finishBoundLaunch(ctx, args, record, terminalId, resolvedWorktreeId, taskId, "fresh");
    },
  },
];
