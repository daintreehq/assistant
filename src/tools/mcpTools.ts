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
        const tools = await ctx.mcp.listTools(false, ctx.signal);
        const list = tools.map((t) => ({
          name: t.name,
          description: t.description ?? "",
        }));
        return ok(`Found ${list.length} Daintree MCP tool(s).`, { tools: list });
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
        const tools = await ctx.mcp.listTools(false, ctx.signal);
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
    schema: WorkflowMutationArgs,
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
      },
      required: ["arguments"],
    },
    async handler(args, ctx) {
      return passthrough(ctx, "workflow.startWorkOnIssue", args.arguments, args.requestKey);
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
