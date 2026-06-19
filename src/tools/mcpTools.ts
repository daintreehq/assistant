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
import { isForbiddenToolName } from "../safety/policy.js";
import { SUPERVISOR_DEFAULT_CADENCE_MS } from "../watcherCadence.js";
import { logDebug } from "../debugLog.js";

/**
 * Shared `note` for the discovery tools (`tool.search`, `daintree.listTools`),
 * explaining what the per-result `callable` flag means so the model treats a
 * `callable: false` entry as "known but not offered this turn", not "broken".
 */
const CALLABLE_NOTE =
  "`callable: false` means the tool exists but is not offered in this turn's tool spec (e.g. an active recipe narrowed the toolset) — only `callable: true` tools can be invoked directly now. An unwrapped tool may still be reachable via `daintree.call` when that escape hatch is offered.";

/**
 * Build a predicate that reports whether a discovered MCP tool is actually offered
 * in the current turn's tool projection. `activeToolNames` is the core ∪ active-recipe
 * (or read-only) tool set stamped onto the context per-turn by `AgentSession.runTurn`;
 * `undefined` means the turn is unconstrained, so every tool is callable. Uses a Set
 * for O(1) membership across all results.
 */
function makeCallablePredicate(
  activeToolNames: string[] | undefined,
): (name: string) => boolean {
  if (activeToolNames === undefined) return () => true;
  const offered = new Set(activeToolNames);
  return (name) => offered.has(name);
}

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
    const res = await ctx.mcp.callTool(mcpName, callArgs, ctx.signal);
    if (res.isError) {
      // Carry Daintree's own refusal text into the failure summary so a denied
      // grant-authorized mutation surfaces *why* it was refused (e.g. a revoked
      // session grant) rather than a generic error, all the way to the queue.
      return fail(
        "MCP_TOOL_ERROR",
        res.text
          ? `Daintree refused ${mcpName}: ${res.text}`
          : `Daintree tool ${mcpName} returned an error.`,
        { details: { structuredContent: res.structuredContent, rawText: res.text } },
      );
    }
    return ok(`Called ${mcpName}.`, {
      text: res.text,
      structuredContent: res.structuredContent,
    });
  } catch (e) {
    // A user abort surfaces as a timeout-shaped MCP error; report it as a clean
    // cancellation rather than a tool failure.
    if (ctx.signal?.aborted) {
      return fail("CANCELLED", `Turn cancelled during ${mcpName}.`, {
        recoverable: false,
      });
    }
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

/* ---- copyTree / terminal-input / agent-focus / git-snapshot wrappers (#120) ---- */

/**
 * copyTree options are an opaque bag Daintree owns — we forward them verbatim and
 * deliberately do NOT model the keys here, so the assistant never invents option
 * names. A `z.record` keeps validation permissive while still rejecting non-objects.
 */
const CopyTreeOptionsSchema = z.record(z.string(), z.unknown());

const CopyTreeGenerateArgs = z.object({
  worktreeId: z
    .string()
    .optional()
    .describe("Worktree to generate the copy tree for; Daintree uses the active worktree when omitted."),
  options: CopyTreeOptionsSchema.optional().describe(
    "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys).",
  ),
});

const CopyTreeInjectToTerminalArgs = z.object({
  terminalId: z.string().trim().min(1).describe("Terminal to inject the generated copy tree into."),
  worktreeId: z
    .string()
    .optional()
    .describe("Worktree to generate the copy tree from; Daintree uses the active worktree when omitted."),
  options: CopyTreeOptionsSchema.optional().describe(
    "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys).",
  ),
});

const TerminalSendCommandArgs = z.object({
  terminalId: z.string().trim().min(1).describe("Terminal to send the command to."),
  command: z.string().trim().min(1).describe("Shell command text to type into the terminal and run."),
});

const SnapshotWorktreeArgs = z.object({
  worktreeId: z.string().trim().min(1).describe("Worktree whose pre-agent git snapshot to act on."),
});

/**
 * MCP tools that MUST go through a typed local wrapper instead of the raw
 * daintree.call escape hatch. Each maps the raw MCP tool name to the wrapper(s)
 * that cover it, with named, validated parameters. The escape hatch invites two
 * recurring failure modes — reaching for it when a wrapper exists, then sending
 * an empty `arguments: {}` and retrying the identical broken call — so for these
 * tools daintree.call fails fast and redirects rather than forwarding a call the
 * model already keeps fumbling. Keep this in sync with the wrappers and with the
 * verified surface in daintreeMcp.ts.
 */
const WRAPPED_MCP_TOOLS: Record<string, string> = {
  "agent.launch":
    'agentTask.spawnForEdits (set mode:"explore" for a read-only investigation, mode:"edit" to change files)',
  "terminal.getOutput":
    "terminal.summarize (model summary of the tail) or terminal.extract (pull specific text/JSON, optionally waiting for a condition)",
  "panel.focus": "terminal.focus",
  "terminal.sendCommand":
    "terminal.sendCommand (typed wrapper — pass terminalId and command)",
  "copyTree.injectToTerminal":
    "copyTree.injectToTerminal (typed wrapper — pass terminalId)",
  "copyTree.generateAndCopyFile":
    "copyTree.generateAndCopyFile (typed wrapper — pass an optional worktreeId)",
  "git.snapshotRevert": "git.snapshotRevert (typed wrapper — pass worktreeId)",
  "git.snapshotDelete": "git.snapshotDelete (typed wrapper — pass worktreeId)",
};

const ForgeReadArgs = z
  .object({
    arguments: z
      .record(z.string(), z.unknown())
      .optional()
      .describe("Optional filters forwarded to Daintree verbatim."),
  })
  .strict();

const ForgeGetIssueArgs = z
  .object({
    arguments: z
      .record(z.string(), z.unknown())
      .optional()
      .describe("Forwarded verbatim; expects the issue identifier (e.g. { issueId })."),
  })
  .strict();

const WorkflowMutationArgs = z
  .object({
    arguments: z
      .record(z.string(), z.unknown())
      .describe("Arguments forwarded to the Daintree workflow action (e.g. issueId)."),
    requestKey: z.string().optional(),
  })
  .strict();

/**
 * workflow.startWorkOnIssue has its own schema (not the shared WorkflowMutationArgs)
 * because it carries `attachWatcher` — a knob meaningless to the other workflow
 * mutations like prepBranchForReview, which return no terminal to supervise. The
 * field is assistant-side only: the handler never forwards it to Daintree.
 */
const WorkflowStartWorkArgs = z
  .object({
    arguments: z
      .record(z.string(), z.unknown())
      .describe("Arguments forwarded to the Daintree workflow action (e.g. issueId)."),
    requestKey: z.string().optional(),
    attachWatcher: z
      .boolean()
      .optional()
      .describe(
        "When true (the default), atomically attach a supervising watcher to the terminal the workflow launches. Set false to skip supervision.",
      ),
  })
  .strict();

/**
 * Best-effort: atomically attach a supervising watcher to the terminal that
 * workflow.startWorkOnIssue just launched, so the spawned agent is never left
 * running unsupervised through a separate, racy follow-up call (issue #126). The
 * Daintree action returns terminalId (nullable — null when the agent launch
 * failed on a partial-success path), worktreeId, issueNumber and issueTitle at
 * the top level of structuredContent. When no terminal was launched we silently
 * skip: the worktree/branch setup itself still succeeded, so the overall call
 * stays ok. A watcher-insert failure degrades to a warning, never a failed call,
 * mirroring finishBoundLaunch in agentTaskTools.ts.
 */
function attachSupervisorWatcher(ctx: ToolContext, res: ToolResult): ToolResult {
  const sc = (res.result as { structuredContent?: unknown } | undefined)?.structuredContent;
  const obj = sc && typeof sc === "object" ? (sc as Record<string, unknown>) : {};
  // Trim before the falsy guard so a whitespace-only id (never expected from
  // Daintree, but cheap to harden against) doesn't spawn a watcher targeting a
  // terminal that can't exist.
  const terminalId =
    typeof obj.terminalId === "string" && obj.terminalId.trim() ? obj.terminalId.trim() : undefined;
  // No terminal launched (terminalId null/absent) → nothing to supervise. The
  // workflow setup still succeeded, so return its result untouched.
  if (!terminalId) return res;

  const worktreeId =
    typeof obj.worktreeId === "string" && obj.worktreeId.trim() ? obj.worktreeId.trim() : undefined;
  const issueTitle = typeof obj.issueTitle === "string" && obj.issueTitle ? obj.issueTitle : undefined;
  const issueLabel =
    typeof obj.issueNumber === "number"
      ? `issue #${obj.issueNumber}`
      : typeof obj.issueNumber === "string" && obj.issueNumber
        ? `issue #${obj.issueNumber}`
        : "issue";

  // Retry safety: if an active supervisor already targets this terminal (e.g. the
  // tool call was retried after a transient error), don't create a duplicate.
  // This scan-then-insert is intentionally non-atomic — adequate for a local
  // SQLite write with no concurrent writers in a single-session daemon.
  const existing = ctx.db.listWatchers("active").find((w) => {
    if (!w.isSupervisor) return false;
    try {
      return (JSON.parse(w.targetsJson) as unknown[]).includes(terminalId);
    } catch {
      return false;
    }
  });
  if (existing) {
    return ok(`${res.summary} Supervisor watcher ${existing.id} already attached to terminal ${terminalId}.`, {
      ...(res.result as object),
      watcherId: existing.id,
    });
  }

  const title = issueTitle ? `watch ${issueTitle}` : `watch ${issueLabel}`;
  const goal = `Supervise work on ${issueLabel}${issueTitle ? `: ${issueTitle}` : ""}`;

  let watcherId: string | undefined;
  let watcherWarning: string | undefined;
  try {
    const watcher = ctx.db.insertWatcher({
      kind: "terminal",
      title,
      goal,
      targetsJson: JSON.stringify([terminalId]),
      cadenceMs: SUPERVISOR_DEFAULT_CADENCE_MS,
      isSupervisor: true,
      modelTier: "small",
      nextCheckAt: Date.now(),
      // workflow.startWorkOnIssue always launches an edit agent; record the mode
      // so the watcher reads an idle prompt as "waiting for input", not "done",
      // and scope the post-completion git check to the agent's worktree when known.
      optionsJson: JSON.stringify({
        ...(worktreeId ? { verificationScope: { worktreeId } } : {}),
        spawnMode: "edit",
      }),
    });
    watcherId = watcher.id;
    logDebug(ctx.config, "watcher.created", {
      watcherId: watcher.id,
      kind: "terminal",
      isSupervisor: true,
      via: "workflow.startWorkOnIssue",
      title,
      goal,
      targets: [terminalId],
      worktreeId,
      cadenceMs: watcher.cadenceMs,
      modelTier: watcher.modelTier,
      nextCheckAt: watcher.nextCheckAt,
    });
  } catch (e) {
    // The agent IS running; surface the supervision gap instead of failing a
    // successful workflow launch.
    watcherWarning = `supervising watcher could not be attached: ${e instanceof Error ? e.message : String(e)}`;
    logDebug(ctx.config, "watcher.create_failed", {
      via: "workflow.startWorkOnIssue",
      title,
      error: watcherWarning,
    });
  }

  // Mirror the foreground-only lifecycle caveat the watcher/spawn tools emit.
  const lifecycleNote = watcherId
    ? (ctx.daemonActive ? ctx.daemonActive() : true)
      ? " NOTE: supervision runs only while this assistant is open; this watcher is discarded when you close the assistant and does not resume on the next launch (watchers are session-scoped)."
      : " NOTE: no scheduler is running in this session, so this watcher will not check until the assistant runs interactively."
    : "";

  return ok(
    `${res.summary}${
      watcherId ? ` Attached supervisor watcher ${watcherId} to terminal ${terminalId}.` : ""
    }${watcherWarning ? ` — ${watcherWarning}` : ""}${lifecycleNote}`,
    {
      ...(res.result as object),
      ...(watcherId ? { watcherId } : {}),
      ...(watcherWarning ? { watcherWarning } : {}),
    },
  );
}

/* ----------------------------- forge wrappers ---------------------------- */

/*
 * The forge.* write tools below give the model a typed, field-level surface for
 * issue/PR/review mutations instead of routing through the generic daintree.call
 * escape hatch. Each is risk "external" (in ALWAYS_CONFIRM, so it always prompts)
 * and forwards its parsed args to the same-named Daintree MCP action. They stay
 * provider-agnostic — no GitHub/GitLab-specific logic lives here.
 *
 * The shapes mirror Daintree's forge action schemas: issue/PR numbers and review
 * ids are positive integers, never strings.
 */
const cwdField = z
  .string()
  .optional()
  .describe("Working directory / worktree path; Daintree resolves the active worktree when omitted.");
const requestKeyField = z
  .string()
  .optional()
  .describe("Optional idempotency key; Daintree strips it before validation and dedupes on it where supported.");
const issueNumberField = z.number().int().positive().describe("Forge issue number.");
const prNumberField = z.number().int().positive().describe("Forge pull/merge request number.");

/** Reusable JSON-schema property fragments for the model-facing `parameters`. */
const P_CWD = {
  type: "string",
  description: "Working directory / worktree path; Daintree resolves the active worktree when omitted.",
};
const P_REQUEST_KEY = { type: "string", description: "Optional idempotency key forwarded to Daintree." };
const P_ISSUE_NUMBER = { type: "integer", minimum: 1, description: "Forge issue number." };
const P_PR_NUMBER = { type: "integer", minimum: 1, description: "Forge pull/merge request number." };

/** Build a flat object JSON-schema for the model from a property map + required list. */
function forgeObjSchema(
  properties: Record<string, Record<string, unknown>>,
  required: string[],
): Record<string, unknown> {
  return { type: "object", additionalProperties: false, properties, required };
}

/**
 * Per-tool, user-facing consequence lines for the forge writes. The approval
 * sheet leads with these instead of the raw "external" risk class. Kept short
 * (one truncatable line) and phrased around what happens to the user's resources
 * — what's published and whether it's reversible — not how the registry gates it.
 */
const FORGE_WRITE_CONSEQUENCES: Record<string, string> = {
  "forge.createIssue":
    "Opens a new issue on the remote forge (GitHub/GitLab). Publicly visible to everyone with repo access.",
  "forge.closeIssue": "Closes an issue on the remote forge. Reversible — it can be reopened.",
  "forge.reopenIssue": "Reopens a closed issue on the remote forge.",
  "forge.editIssue": "Edits an issue's title or body on the remote forge.",
  "forge.addIssueComment":
    "Posts a comment on an issue on the remote forge. Visible to everyone with repo access.",
  "forge.addIssueLabel": "Adds a label to an issue on the remote forge.",
  "forge.removeIssueLabel": "Removes a label from an issue on the remote forge.",
  "forge.assignIssue": "Assigns a user to an issue on the remote forge.",
  "forge.unassignIssue": "Removes an assignee from an issue on the remote forge.",
  "forge.createPR":
    "Opens a pull request on the remote forge. Visible to everyone with repo access.",
  "forge.closePR": "Closes a pull request without merging. Reversible — it can be reopened.",
  "forge.reopenPR": "Reopens a closed pull request on the remote forge.",
  "forge.mergePR":
    "Merges a pull request into its base branch on the remote forge. Hard to undo once merged.",
  "forge.convertPRToDraft": "Converts a pull request back to draft on the remote forge.",
  "forge.markPRReadyForReview": "Marks a draft pull request ready for review on the remote forge.",
  "forge.commentOnPR":
    "Posts a comment on a pull request on the remote forge. Visible to everyone with repo access.",
  "forge.editPR": "Edits a pull request's title or body on the remote forge.",
  "forge.approvePR": "Submits an approving review on a pull request on the remote forge.",
  "forge.requestChanges":
    "Submits a changes-requested review on a pull request on the remote forge.",
  "forge.dismissReview": "Dismisses an existing review on a pull request on the remote forge.",
  "forge.requestReviewers": "Requests reviewers on a pull request on the remote forge.",
};

/**
 * Build a forge WRITE wrapper. Every forge mutation shares one shape: risk
 * "external" (always confirmed) and a handler that forwards the parsed args to
 * the same-named Daintree MCP action — lifting requestKey out so it travels as
 * the dedicated idempotency parameter rather than a forwarded field.
 */
function forgeWrite<A extends { requestKey?: string }>(
  name: string,
  description: string,
  schema: z.ZodType<A>,
  parameters: Record<string, unknown>,
): ToolDef<A> {
  return {
    name,
    description,
    consequence: FORGE_WRITE_CONSEQUENCES[name],
    risk: "external",
    schema,
    parameters,
    async handler(args, ctx) {
      const { requestKey, ...rest } = args;
      return passthrough(ctx, name, rest as Record<string, unknown>, requestKey);
    },
  };
}

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
      "List the Daintree MCP tools, with their names and descriptions. Each entry carries a `callable` flag: tools marked `callable: false` are known to exist but are not offered in this turn's tool spec (e.g. an active recipe narrowed the toolset), so calling them would do nothing.",
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
        const tools = await ctx.mcp.listTools(false, ctx.signal);
        // `activeToolNames` is the turn's tool projection; `undefined` ⇒ unconstrained
        // (everything callable). Annotate rather than filter so the model still learns
        // a tool exists even when the current recipe doesn't offer it.
        const callableOf = makeCallablePredicate(ctx.activeToolNames);
        const list = tools.map((t) => ({
          name: t.name,
          description: t.description ?? "",
          callable: callableOf(t.name),
        }));
        return ok(`Found ${list.length} Daintree MCP tool(s).`, {
          tools: list,
          note: CALLABLE_NOTE,
        });
      } catch (e) {
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", "Turn cancelled while listing MCP tools.", {
            recoverable: false,
          });
        }
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
      "Search Daintree MCP tools by keyword (substring match on name/description). Each match carries a `callable` flag: tools marked `callable: false` exist but are not offered in this turn's tool spec (e.g. an active recipe narrowed the toolset), so calling them would do nothing — only `callable: true` results can be invoked now.",
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
        const tools = await ctx.mcp.listTools(false, ctx.signal);
        // Annotate each match with whether it's actually offered in this turn's tool
        // spec (`ctx.activeToolNames`; `undefined` ⇒ unconstrained, all callable) so we
        // never advertise a tool the model can't invoke right now. Annotate, don't
        // filter — the model still needs to learn a tool exists to reason about it.
        const callableOf = makeCallablePredicate(ctx.activeToolNames);
        const matches = tools
          .filter(
            (t) =>
              t.name.toLowerCase().includes(q) ||
              (t.description ?? "").toLowerCase().includes(q),
          )
          .slice(0, max)
          .map((t) => ({
            name: t.name,
            description: t.description ?? "",
            callable: callableOf(t.name),
          }));
        return ok(
          `Found ${matches.length} Daintree MCP tool(s) matching "${args.query}".`,
          {
            query: args.query,
            matches,
            note: CALLABLE_NOTE,
          },
        );
      } catch (e) {
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", "Turn cancelled while searching MCP tools.", {
            recoverable: false,
          });
        }
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
      "Raw passthrough to ANY Daintree MCP tool. Escape hatch — highest risk ('system'), always confirmed, requires the 'system' tier. Prefer purpose-built tools; use this only when no wrapper exists. Tools that already have a wrapper (e.g. agent.launch, terminal.getOutput, panel.focus) are refused here and redirected to the wrapper.",
    consequence:
      "Runs an arbitrary Daintree MCP tool with the arguments shown. Effect depends entirely on the named tool — inspect the args before approving.",
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
      const wrapper = WRAPPED_MCP_TOOLS[args.name];
      if (wrapper) {
        return fail(
          "USE_TYPED_WRAPPER",
          `Do not call ${args.name} through daintree.call — use the typed wrapper instead: ${wrapper}. It takes named, validated parameters, so you can't drop a required argument. Switch tools; do not retry this raw call.`,
        );
      }
      // The no-file-edit invariant is enforced on local tool NAMES at registration,
      // but daintree.call forwards an arbitrary raw MCP name — apply the same guard
      // here so a file-mutating MCP tool can't be reached through the escape hatch.
      if (isForbiddenToolName(args.name)) {
        return fail(
          "FILE_EDIT_FORBIDDEN",
          `Refusing to call ${args.name} via daintree.call — the assistant never edits files directly. Spawn a visible agent (agentTask.spawnForEdits) to make changes.`,
          { recoverable: false },
        );
      }
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
        const res = await ctx.mcp.callTool(args.name, callArgs, ctx.signal);
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
        if (ctx.signal?.aborted) {
          return fail("CANCELLED", `Turn cancelled during ${args.name}.`, {
            recoverable: false,
          });
        }
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
    consequence:
      "Runs a workspace recipe that changes real project state (e.g. files, branches, or worktrees). May not be reversible.",
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
    consequence:
      "Creates a new git worktree on disk and runs its startup recipe. Adds a checkout you may later need to clean up.",
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
      "Focus/reveal a Daintree terminal in the UI (read-only side effect on the UI; no state mutation).",
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
      // Daintree has no `terminal.focus` MCP tool — terminals are panels, so the
      // correct call is `panel.focus` with the terminal id as the panelId.
      return passthrough(ctx, "panel.focus", { panelId: args.terminalId });
    },
  },
  /* ---- copyTree / terminal-input / agent-focus / git-snapshot wrappers (#120) ----
   * These cover Daintree actions that were previously reachable only through the raw
   * daintree.call escape hatch. Each carries the same risk class Daintree gates the
   * action at (verified against helpAssistantTierAllowlists.ts), so reads/UI focus run
   * without the system-tier confirmation the escape hatch always forces. The fleet.*
   * arming ops and terminal.armByState are intentionally NOT wrapped — they are
   * renderer-only UI gestures with no MCP surface. */
  {
    name: "copyTree.generate",
    description:
      "Generate a Daintree 'copy tree' — a concatenated digest of a worktree's files — and return it as text (read-only). Typed wrapper around the Daintree copyTree.generate MCP tool. 'options' is forwarded verbatim; don't invent keys.",
    risk: "read",
    readOnly: true,
    schema: CopyTreeGenerateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        worktreeId: {
          type: "string",
          description:
            "Worktree to generate the copy tree for; Daintree uses the active worktree when omitted.",
        },
        options: {
          type: "object",
          additionalProperties: true,
          description: "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "copyTree.generate", args);
    },
  },
  {
    name: "terminal.sendCommand",
    description:
      "Send a command line to a Daintree terminal — types it into the terminal's input and runs it. Mutating, so it always confirms. Typed wrapper around the Daintree terminal.sendCommand MCP tool.",
    consequence:
      "Runs a shell command in the named terminal as if you typed it. Effects depend on the command and may not be reversible.",
    risk: "terminal",
    schema: TerminalSendCommandArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalId: { type: "string", description: "Terminal to send the command to." },
        command: {
          type: "string",
          description: "Shell command text to type into the terminal and run.",
        },
      },
      required: ["terminalId", "command"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "terminal.sendCommand", args);
    },
  },
  {
    name: "copyTree.injectToTerminal",
    description:
      "Generate a worktree's copy tree and inject it into a Daintree terminal's input. Mutating (writes into a terminal), so it always confirms. Typed wrapper around the Daintree copyTree.injectToTerminal MCP tool. 'options' is forwarded verbatim.",
    consequence:
      "Pastes a generated file digest into the named terminal's input. May be large; review the target terminal before approving.",
    risk: "terminal",
    schema: CopyTreeInjectToTerminalArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        terminalId: {
          type: "string",
          description: "Terminal to inject the generated copy tree into.",
        },
        worktreeId: {
          type: "string",
          description:
            "Worktree to generate the copy tree from; Daintree uses the active worktree when omitted.",
        },
        options: {
          type: "object",
          additionalProperties: true,
          description: "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys).",
        },
      },
      required: ["terminalId"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "copyTree.injectToTerminal", args);
    },
  },
  {
    name: "agent.focusNextWaiting",
    description:
      "Focus the next agent terminal that is waiting for input or a decision (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextWaiting MCP tool.",
    risk: "ui",
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      return passthrough(ctx, "agent.focusNextWaiting", {});
    },
  },
  {
    name: "agent.focusNextWorking",
    description:
      "Focus the next agent terminal that is actively working (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextWorking MCP tool.",
    risk: "ui",
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      return passthrough(ctx, "agent.focusNextWorking", {});
    },
  },
  {
    name: "agent.focusNextAgent",
    description:
      "Focus the next agent terminal in order (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextAgent MCP tool.",
    risk: "ui",
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      return passthrough(ctx, "agent.focusNextAgent", {});
    },
  },
  {
    name: "agent.focusPreviousAgent",
    description:
      "Focus the previous agent terminal in order (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusPreviousAgent MCP tool.",
    risk: "ui",
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      return passthrough(ctx, "agent.focusPreviousAgent", {});
    },
  },
  {
    name: "workflow.focusNextAttention",
    description:
      "Focus the next agent needing attention (waiting agents before working ones) and report the queue counts (UI focus change; no state mutation). Typed wrapper around the Daintree workflow.focusNextAttention MCP tool — returns { focused, state, waitingCount, workingCount }.",
    risk: "ui",
    schema: ListToolsArgs,
    parameters: NO_ARGS,
    async handler(_args, ctx) {
      return passthrough(ctx, "workflow.focusNextAttention", {});
    },
  },
  {
    name: "copyTree.generateAndCopyFile",
    description:
      "Generate a worktree's copy tree and copy it to the OS clipboard as a file. System tier — always confirms. Typed wrapper around the Daintree copyTree.generateAndCopyFile MCP tool. 'options' is forwarded verbatim.",
    consequence:
      "Writes the generated file digest to the operating-system clipboard, replacing its current contents.",
    risk: "system",
    schema: CopyTreeGenerateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        worktreeId: {
          type: "string",
          description:
            "Worktree to generate the copy tree for; Daintree uses the active worktree when omitted.",
        },
        options: {
          type: "object",
          additionalProperties: true,
          description: "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "copyTree.generateAndCopyFile", args);
    },
  },
  {
    name: "git.snapshotRevert",
    description:
      "Revert a worktree to its pre-agent git snapshot via Daintree. System tier — always confirms. Typed wrapper around the Daintree git.snapshotRevert MCP tool.",
    consequence:
      "Resets the worktree to its pre-agent snapshot. Uncommitted changes are irrecoverable — this cannot be undone.",
    risk: "git",
    schema: SnapshotWorktreeArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        worktreeId: {
          type: "string",
          description: "Worktree whose pre-agent git snapshot to revert to.",
        },
      },
      required: ["worktreeId"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "git.snapshotRevert", args);
    },
  },
  {
    name: "git.snapshotDelete",
    description:
      "Permanently delete a worktree's pre-agent git snapshot via Daintree. System tier — always confirms. Typed wrapper around the Daintree git.snapshotDelete MCP tool.",
    consequence:
      "Permanently deletes the worktree's pre-agent snapshot. There is no recovery once deleted.",
    risk: "git",
    schema: SnapshotWorktreeArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        worktreeId: {
          type: "string",
          description: "Worktree whose pre-agent git snapshot to delete.",
        },
      },
      required: ["worktreeId"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "git.snapshotDelete", args);
    },
  },
  {
    name: "forge.listIssues",
    description:
      "List forge issues (GitHub/GitLab) via Daintree (read-only). Typed wrapper around the Daintree forge.listIssues MCP tool. Pass optional filters through 'arguments'.",
    risk: "read",
    readOnly: true,
    schema: ForgeReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Optional filters forwarded to Daintree (e.g. state, labels).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "forge.listIssues", args.arguments ?? {});
    },
  },
  {
    name: "forge.getIssue",
    description:
      "Fetch a single forge issue via Daintree (read-only). Typed wrapper around the Daintree forge.getIssue MCP tool. Pass the issue identifier (e.g. issueId) through 'arguments'.",
    risk: "read",
    readOnly: true,
    schema: ForgeGetIssueArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Forwarded verbatim; expects the issue identifier (e.g. { issueId }).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "forge.getIssue", args.arguments ?? {});
    },
  },
  {
    name: "forge.listPRs",
    description:
      "List forge pull/merge requests via Daintree (read-only). Typed wrapper around the Daintree forge.listPRs MCP tool. Pass optional filters through 'arguments'.",
    risk: "read",
    readOnly: true,
    schema: ForgeReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Optional filters forwarded to Daintree (e.g. state, base).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "forge.listPRs", args.arguments ?? {});
    },
  },
  // ---- forge issue writes (#29) ----
  forgeWrite(
    "forge.createIssue",
    "Create a forge issue (GitHub/GitLab) via Daintree. Mutates the forge, so it always confirms. Provider-agnostic.",
    z.object({
      cwd: cwdField,
      title: z.string().trim().min(1).describe("Issue title."),
      body: z.string().optional().describe("Issue body (markdown)."),
      labels: z.array(z.string()).optional().describe("Label names to apply on creation."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        title: { type: "string", description: "Issue title." },
        body: { type: "string", description: "Issue body (markdown)." },
        labels: {
          type: "array",
          items: { type: "string" },
          description: "Label names to apply on creation.",
        },
        requestKey: P_REQUEST_KEY,
      },
      ["title"],
    ),
  ),
  forgeWrite(
    "forge.closeIssue",
    "Close a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      stateReason: z
        .enum(["completed", "not_planned", "duplicate"])
        .optional()
        .describe("Why the issue is being closed."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        stateReason: {
          type: "string",
          enum: ["completed", "not_planned", "duplicate"],
          description: "Why the issue is being closed.",
        },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber"],
    ),
  ),
  forgeWrite(
    "forge.reopenIssue",
    "Reopen a closed forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({ cwd: cwdField, issueNumber: issueNumberField, requestKey: requestKeyField }),
    forgeObjSchema(
      { cwd: P_CWD, issueNumber: P_ISSUE_NUMBER, requestKey: P_REQUEST_KEY },
      ["issueNumber"],
    ),
  ),
  forgeWrite(
    "forge.editIssue",
    "Edit a forge issue's title and/or body via Daintree (provide at least one). Mutates the forge, so it always confirms.",
    z
      .object({
        cwd: cwdField,
        issueNumber: issueNumberField,
        title: z.string().optional().describe("New issue title."),
        body: z.string().optional().describe("New issue body (markdown)."),
        requestKey: requestKeyField,
      })
      .refine((v) => v.title != null || v.body != null, {
        message: "Provide at least one of title or body.",
      }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        title: { type: "string", description: "New issue title." },
        body: { type: "string", description: "New issue body (markdown)." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber"],
    ),
  ),
  forgeWrite(
    "forge.addIssueComment",
    "Add a comment to a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      body: z.string().trim().min(1).describe("Comment body (markdown)."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        body: { type: "string", description: "Comment body (markdown)." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber", "body"],
    ),
  ),
  forgeWrite(
    "forge.addIssueLabel",
    "Add a label to a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      label: z.string().trim().min(1).describe("Label name to add."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        label: { type: "string", description: "Label name to add." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber", "label"],
    ),
  ),
  forgeWrite(
    "forge.removeIssueLabel",
    "Remove a label from a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      label: z.string().trim().min(1).describe("Label name to remove."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        label: { type: "string", description: "Label name to remove." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber", "label"],
    ),
  ),
  forgeWrite(
    "forge.assignIssue",
    "Assign a user to a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      username: z.string().trim().min(1).describe("Username to assign."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        username: { type: "string", description: "Username to assign." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber", "username"],
    ),
  ),
  forgeWrite(
    "forge.unassignIssue",
    "Unassign a user from a forge issue via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      issueNumber: issueNumberField,
      username: z.string().trim().min(1).describe("Username to unassign."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        issueNumber: P_ISSUE_NUMBER,
        username: { type: "string", description: "Username to unassign." },
        requestKey: P_REQUEST_KEY,
      },
      ["issueNumber", "username"],
    ),
  ),
  // ---- forge PR read (#29) ----
  {
    name: "forge.getPR",
    description:
      "Fetch a single forge pull/merge request via Daintree (read-only). Typed wrapper around the Daintree forge.getPR MCP tool.",
    risk: "read",
    readOnly: true,
    schema: z.object({ cwd: cwdField, prNumber: prNumberField }),
    parameters: forgeObjSchema({ cwd: P_CWD, prNumber: P_PR_NUMBER }, ["prNumber"]),
    async handler(args, ctx) {
      return passthrough(ctx, "forge.getPR", args);
    },
  },
  // ---- forge PR writes (#29) ----
  forgeWrite(
    "forge.createPR",
    "Create a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      head: z.string().trim().min(1).describe("Source branch (the branch with changes)."),
      base: z.string().trim().min(1).describe("Target branch to merge into."),
      title: z.string().trim().min(1).describe("PR title."),
      body: z.string().optional().describe("PR body (markdown)."),
      draft: z.boolean().optional().describe("Open as a draft PR."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        head: { type: "string", description: "Source branch (the branch with changes)." },
        base: { type: "string", description: "Target branch to merge into." },
        title: { type: "string", description: "PR title." },
        body: { type: "string", description: "PR body (markdown)." },
        draft: { type: "boolean", description: "Open as a draft PR." },
        requestKey: P_REQUEST_KEY,
      },
      ["head", "base", "title"],
    ),
  ),
  forgeWrite(
    "forge.closePR",
    "Close a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({ cwd: cwdField, prNumber: prNumberField, requestKey: requestKeyField }),
    forgeObjSchema(
      { cwd: P_CWD, prNumber: P_PR_NUMBER, requestKey: P_REQUEST_KEY },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.reopenPR",
    "Reopen a closed forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({ cwd: cwdField, prNumber: prNumberField, requestKey: requestKeyField }),
    forgeObjSchema(
      { cwd: P_CWD, prNumber: P_PR_NUMBER, requestKey: P_REQUEST_KEY },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.mergePR",
    "Merge a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      prNumber: prNumberField,
      mergeMethod: z
        .enum(["merge", "squash", "rebase"])
        .optional()
        .describe("Merge strategy."),
      commitTitle: z.string().optional().describe("Override the merge commit title."),
      commitMessage: z.string().optional().describe("Override the merge commit message."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        mergeMethod: {
          type: "string",
          enum: ["merge", "squash", "rebase"],
          description: "Merge strategy.",
        },
        commitTitle: { type: "string", description: "Override the merge commit title." },
        commitMessage: { type: "string", description: "Override the merge commit message." },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.convertPRToDraft",
    "Convert a forge pull/merge request to draft via Daintree. Mutates the forge, so it always confirms.",
    z.object({ cwd: cwdField, prNumber: prNumberField, requestKey: requestKeyField }),
    forgeObjSchema(
      { cwd: P_CWD, prNumber: P_PR_NUMBER, requestKey: P_REQUEST_KEY },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.markPRReadyForReview",
    "Mark a draft forge pull/merge request ready for review via Daintree. Mutates the forge, so it always confirms.",
    z.object({ cwd: cwdField, prNumber: prNumberField, requestKey: requestKeyField }),
    forgeObjSchema(
      { cwd: P_CWD, prNumber: P_PR_NUMBER, requestKey: P_REQUEST_KEY },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.commentOnPR",
    "Add a comment to a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      prNumber: prNumberField,
      body: z.string().trim().min(1).describe("Comment body (markdown)."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        body: { type: "string", description: "Comment body (markdown)." },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber", "body"],
    ),
  ),
  forgeWrite(
    "forge.editPR",
    "Edit a forge pull/merge request's title and/or body via Daintree (provide at least one). Mutates the forge, so it always confirms.",
    z
      .object({
        cwd: cwdField,
        prNumber: prNumberField,
        title: z.string().optional().describe("New PR title."),
        body: z.string().optional().describe("New PR body (markdown)."),
        requestKey: requestKeyField,
      })
      .refine((v) => v.title != null || v.body != null, {
        message: "Provide at least one of title or body.",
      }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        title: { type: "string", description: "New PR title." },
        body: { type: "string", description: "New PR body (markdown)." },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber"],
    ),
  ),
  // ---- forge review writes (#29) ----
  forgeWrite(
    "forge.approvePR",
    "Approve a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      prNumber: prNumberField,
      body: z.string().optional().describe("Optional approval comment (markdown)."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        body: { type: "string", description: "Optional approval comment (markdown)." },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber"],
    ),
  ),
  forgeWrite(
    "forge.requestChanges",
    "Request changes on a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      prNumber: prNumberField,
      body: z.string().trim().min(1).describe("Review comment explaining the requested changes (markdown)."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        body: {
          type: "string",
          description: "Review comment explaining the requested changes (markdown).",
        },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber", "body"],
    ),
  ),
  forgeWrite(
    "forge.dismissReview",
    "Dismiss an existing review on a forge pull/merge request via Daintree. Mutates the forge, so it always confirms.",
    z.object({
      cwd: cwdField,
      prNumber: prNumberField,
      reviewId: z.number().int().positive().describe("Id of the review to dismiss."),
      message: z.string().trim().min(1).describe("Reason for dismissing the review."),
      requestKey: requestKeyField,
    }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        reviewId: { type: "integer", minimum: 1, description: "Id of the review to dismiss." },
        message: { type: "string", description: "Reason for dismissing the review." },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber", "reviewId", "message"],
    ),
  ),
  forgeWrite(
    "forge.requestReviewers",
    "Request reviewers on a forge pull/merge request via Daintree (provide at least one of users or teams). Mutates the forge, so it always confirms.",
    z
      .object({
        cwd: cwdField,
        prNumber: prNumberField,
        users: z
          .array(z.string().trim().min(1))
          .optional()
          .describe("Usernames to request review from."),
        teams: z
          .array(z.string().trim().min(1))
          .optional()
          .describe("Team slugs to request review from."),
        requestKey: requestKeyField,
      })
      .refine((v) => (v.users?.length ?? 0) > 0 || (v.teams?.length ?? 0) > 0, {
        message: "Provide at least one of users or teams.",
      }),
    forgeObjSchema(
      {
        cwd: P_CWD,
        prNumber: P_PR_NUMBER,
        users: {
          type: "array",
          items: { type: "string" },
          description: "Usernames to request review from.",
        },
        teams: {
          type: "array",
          items: { type: "string" },
          description: "Team slugs to request review from.",
        },
        requestKey: P_REQUEST_KEY,
      },
      ["prNumber"],
    ),
  ),
  {
    name: "workflow.startWorkOnIssue",
    description:
      "Start work on a forge issue via Daintree — sets up a worktree/branch for the issue. Mutates real workspace state and touches the forge, so it always confirms. Pass an idempotency requestKey when available.",
    consequence:
      "Sets up a worktree and branch for a forge issue, and touches the remote forge. Creates local checkout state.",
    risk: "external",
    schema: WorkflowStartWorkArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Arguments for workflow.startWorkOnIssue (e.g. issueId).",
        },
        requestKey: { type: "string", description: "Optional idempotency key." },
        attachWatcher: {
          type: "boolean",
          description:
            "When true (the default), atomically attach a supervising watcher to the launched terminal. Set false to skip supervision.",
        },
      },
      required: ["arguments"],
    },
    async handler(args, ctx) {
      const res = await passthrough(ctx, "workflow.startWorkOnIssue", args.arguments, args.requestKey);
      // Atomically supervise the launched terminal in the same call (issue #126),
      // so the agent is never left unsupervised through a separate follow-up step.
      // A failed passthrough is already shaped correctly; opt-out skips entirely.
      if (!res.ok || args.attachWatcher === false) return res;
      return attachSupervisorWatcher(ctx, res);
    },
  },
  {
    name: "workflow.prepBranchForReview",
    description:
      "Prepare the current branch for review via Daintree — readies the branch/PR for review. Mutates real workspace state and touches the forge, so it always confirms. Pass an idempotency requestKey when available.",
    consequence:
      "Readies the current branch and its pull request for review on the remote forge. Pushes branch state outward.",
    risk: "external",
    schema: WorkflowMutationArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        arguments: {
          type: "object",
          additionalProperties: true,
          description: "Arguments for workflow.prepBranchForReview (e.g. worktreeId).",
        },
        requestKey: { type: "string", description: "Optional idempotency key." },
      },
      required: ["arguments"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "workflow.prepBranchForReview", args.arguments, args.requestKey);
    },
  },
];
