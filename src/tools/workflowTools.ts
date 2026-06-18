/**
 * Workflow ledger tools. A workflow run is the durable, first-class object that
 * ties together the terminals, watchers, and queue events of one unit of issue/PR
 * work, plus its single next required action. These tools let the assistant open
 * a ledger record when it starts supervising an issue, read it back after a
 * restart, and patch its status / links / next action as the work progresses.
 *
 * All mutations touch only local daemon state (risk "local"); get/list are
 * read-only. No file mutation, no terminal input.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import {
  RecommendedAction,
  type WorkflowRunRecord,
  type WorkflowRunStatus,
} from "../schemas.js";

const STATUS_VALUES = [
  "pending",
  "active",
  "blocked",
  "done",
  "cancelled",
  "failed",
] as const;

const StatusEnum = z.enum(STATUS_VALUES);

/** Statuses that close out a run and so stamp completedAt. */
const TERMINAL_STATUSES: ReadonlySet<WorkflowRunStatus> = new Set([
  "done",
  "cancelled",
  "failed",
]);

const NextAction = RecommendedAction.describe(
  "The single next required action for this run (a RecommendedAction).",
);

const CreateArgs = z.object({
  issueNumber: z.number().int().optional().describe("GitHub issue number."),
  issueUrl: z.string().optional().describe("GitHub issue URL."),
  issueTitle: z.string().optional().describe("Short issue title."),
  branch: z.string().optional().describe("Working branch name."),
  worktreeId: z.string().optional().describe("Daintree worktree id."),
  prNumber: z.number().int().optional().describe("Pull request number, once opened."),
  prUrl: z.string().optional().describe("Pull request URL, once opened."),
  terminalIds: z
    .array(z.string())
    .optional()
    .describe("Daintree terminal ids working on this run."),
  watcherIds: z
    .array(z.string())
    .optional()
    .describe("Watcher ids supervising this run."),
  queueEventIds: z
    .array(z.string())
    .optional()
    .describe("Queue event ids associated with this run."),
  status: StatusEnum.optional().describe(
    "Initial status (default 'pending').",
  ),
  nextAction: NextAction.optional(),
  notes: z
    .array(z.string())
    .optional()
    .describe("Freeform context notes."),
});

const GetArgs = z.object({
  id: z.string().describe("Workflow run id (wfr_…)."),
});

const ListArgs = z.object({
  status: StatusEnum.optional().describe(
    "Only return runs with this status (default: all).",
  ),
});

const UpdateArgs = z.object({
  id: z.string().describe("Workflow run id to patch."),
  issueNumber: z.number().int().optional(),
  issueUrl: z.string().optional(),
  issueTitle: z.string().optional(),
  branch: z.string().optional(),
  worktreeId: z.string().optional(),
  prNumber: z.number().int().optional().describe("Pull request number, once opened."),
  prUrl: z.string().optional().describe("Pull request URL, once opened."),
  terminalIds: z
    .array(z.string())
    .optional()
    .describe("Replaces the linked terminal ids."),
  watcherIds: z
    .array(z.string())
    .optional()
    .describe("Replaces the linked watcher ids."),
  queueEventIds: z
    .array(z.string())
    .optional()
    .describe("Replaces the linked queue event ids."),
  status: StatusEnum.optional().describe(
    "New status. Reaching done/cancelled/failed stamps completedAt.",
  ),
  nextAction: NextAction.optional(),
  notes: z
    .array(z.string())
    .optional()
    .describe("Replaces the freeform context notes."),
});

/** Parse a stored JSON string-array column, tolerating null/garbage. */
function parseArr(s?: string): string[] {
  if (!s) return [];
  try {
    const v = JSON.parse(s);
    return Array.isArray(v) ? v.map(String) : [];
  } catch {
    return [];
  }
}

/** Parse a stored RecommendedAction, returning undefined on absent/garbage. */
function parseAction(s?: string): z.infer<typeof RecommendedAction> | undefined {
  if (!s) return undefined;
  try {
    const parsed = RecommendedAction.safeParse(JSON.parse(s));
    return parsed.success ? parsed.data : undefined;
  } catch {
    return undefined;
  }
}

/** Deserialize a stored record into a model-friendly view (arrays + object). */
function toView(rec: WorkflowRunRecord) {
  return {
    id: rec.id,
    issueNumber: rec.issueNumber,
    issueUrl: rec.issueUrl,
    issueTitle: rec.issueTitle,
    branch: rec.branch,
    worktreeId: rec.worktreeId,
    prNumber: rec.prNumber,
    prUrl: rec.prUrl,
    terminalIds: parseArr(rec.terminalIdsJson),
    watcherIds: parseArr(rec.watcherIdsJson),
    queueEventIds: parseArr(rec.queueEventIdsJson),
    status: rec.status,
    nextAction: parseAction(rec.nextActionJson),
    notes: parseArr(rec.notesJson),
    createdAt: rec.createdAt,
    updatedAt: rec.updatedAt,
    completedAt: rec.completedAt,
  };
}

function summarizeWorkflow(rec: WorkflowRunRecord): string {
  const issue = rec.issueNumber ? `#${rec.issueNumber}` : rec.id;
  const title = rec.issueTitle ? ` ${rec.issueTitle}` : "";
  const pr = rec.prNumber ? ` PR#${rec.prNumber}` : "";
  return `${rec.id} [${rec.status}] ${issue}${title}${pr}`;
}

/** Shared JSON-schema properties for the create/update tool surfaces. */
const RECORD_PROPS: Record<string, unknown> = {
  issueNumber: { type: "number", description: "GitHub issue number." },
  issueUrl: { type: "string", description: "GitHub issue URL." },
  issueTitle: { type: "string", description: "Short issue title." },
  branch: { type: "string", description: "Working branch name." },
  worktreeId: { type: "string", description: "Daintree worktree id." },
  prNumber: { type: "number", description: "Pull request number, once opened." },
  prUrl: { type: "string", description: "Pull request URL, once opened." },
  terminalIds: {
    type: "array",
    items: { type: "string" },
    description: "Daintree terminal ids working on this run.",
  },
  watcherIds: {
    type: "array",
    items: { type: "string" },
    description: "Watcher ids supervising this run.",
  },
  queueEventIds: {
    type: "array",
    items: { type: "string" },
    description: "Queue event ids associated with this run.",
  },
  status: {
    type: "string",
    enum: [...STATUS_VALUES],
    description:
      "Run status. Reaching done/cancelled/failed stamps completedAt.",
  },
  nextAction: {
    type: "object",
    description: "The single next required action (a RecommendedAction).",
    properties: {
      label: { type: "string" },
      toolName: { type: "string" },
      args: {},
      risk: { type: "string" },
      requiresConfirmation: { type: "boolean" },
    },
  },
  notes: {
    type: "array",
    items: { type: "string" },
    description: "Freeform context notes.",
  },
};

export const workflowTools: ToolDef[] = [
  {
    name: "workflow.create",
    description:
      "Open a durable workflow ledger record for one unit of issue/PR work, linking its terminals/watchers/queue events and recording the next required action. Local daemon state only; never edits files.",
    risk: "local",
    schema: CreateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: RECORD_PROPS,
      required: [],
    },
    async handler(args: z.infer<typeof CreateArgs>, ctx) {
      try {
        const rec = ctx.db.insertWorkflowRun({
          issueNumber: args.issueNumber,
          issueUrl: args.issueUrl,
          issueTitle: args.issueTitle,
          branch: args.branch,
          worktreeId: args.worktreeId,
          prNumber: args.prNumber,
          prUrl: args.prUrl,
          terminalIdsJson: args.terminalIds
            ? JSON.stringify(args.terminalIds)
            : undefined,
          watcherIdsJson: args.watcherIds
            ? JSON.stringify(args.watcherIds)
            : undefined,
          queueEventIdsJson: args.queueEventIds
            ? JSON.stringify(args.queueEventIds)
            : undefined,
          status: args.status,
          // A run created directly in a terminal status is born complete.
          completedAt:
            args.status && TERMINAL_STATUSES.has(args.status)
              ? Date.now()
              : undefined,
          nextActionJson: args.nextAction
            ? JSON.stringify(args.nextAction)
            : undefined,
          notesJson: args.notes ? JSON.stringify(args.notes) : undefined,
        });
        const forIssue = args.issueNumber ? ` for issue #${args.issueNumber}` : "";
        return ok(`Created workflow run ${rec.id}${forIssue} [${rec.status}].`, {
          id: rec.id,
          workflow: toView(rec),
        });
      } catch (e) {
        return fail(
          "WORKFLOW_CREATE",
          `Could not create workflow run: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "workflow.get",
    description: "Read one workflow ledger record by id (read-only).",
    risk: "read",
    readOnly: true,
    schema: GetArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Workflow run id (wfr_…)." },
      },
      required: ["id"],
    },
    async handler(args: z.infer<typeof GetArgs>, ctx) {
      const rec = ctx.db.getWorkflowRun(args.id);
      if (!rec) {
        return fail("WORKFLOW_NOT_FOUND", `No workflow run with id ${args.id}.`, {
          recoverable: false,
        });
      }
      return ok(summarizeWorkflow(rec), { workflow: toView(rec) });
    },
  },
  {
    name: "workflow.list",
    description:
      "List workflow ledger records, optionally filtered by status (read-only).",
    risk: "read",
    readOnly: true,
    schema: ListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        status: {
          type: "string",
          enum: [...STATUS_VALUES],
          description: "Only return runs with this status (default: all).",
        },
      },
    },
    async handler(args: z.infer<typeof ListArgs>, ctx) {
      const runs = ctx.db.listWorkflowRuns(args.status);
      const lines = runs.map(summarizeWorkflow);
      const scope = args.status ? ` ${args.status}` : "";
      return ok(
        runs.length
          ? `${runs.length}${scope} workflow run(s):\n${lines.join("\n")}`
          : `No${scope} workflow runs.`,
        { workflows: runs.map(toView) },
      );
    },
  },
  {
    name: "workflow.update",
    description:
      "Patch a workflow ledger record — status, next action, PR linkage, or terminal/watcher/queue links. Array fields replace the stored list. Local daemon state only.",
    risk: "local",
    schema: UpdateArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        id: { type: "string", description: "Workflow run id to patch." },
        ...RECORD_PROPS,
      },
      required: ["id"],
    },
    async handler(args: z.infer<typeof UpdateArgs>, ctx) {
      const existing = ctx.db.getWorkflowRun(args.id);
      if (!existing) {
        return fail("WORKFLOW_NOT_FOUND", `No workflow run with id ${args.id}.`, {
          recoverable: false,
        });
      }
      const patch: Partial<WorkflowRunRecord> = {};
      if (args.issueNumber !== undefined) patch.issueNumber = args.issueNumber;
      if (args.issueUrl !== undefined) patch.issueUrl = args.issueUrl;
      if (args.issueTitle !== undefined) patch.issueTitle = args.issueTitle;
      if (args.branch !== undefined) patch.branch = args.branch;
      if (args.worktreeId !== undefined) patch.worktreeId = args.worktreeId;
      if (args.prNumber !== undefined) patch.prNumber = args.prNumber;
      if (args.prUrl !== undefined) patch.prUrl = args.prUrl;
      if (args.terminalIds !== undefined)
        patch.terminalIdsJson = JSON.stringify(args.terminalIds);
      if (args.watcherIds !== undefined)
        patch.watcherIdsJson = JSON.stringify(args.watcherIds);
      if (args.queueEventIds !== undefined)
        patch.queueEventIdsJson = JSON.stringify(args.queueEventIds);
      if (args.nextAction !== undefined)
        patch.nextActionJson = JSON.stringify(args.nextAction);
      if (args.notes !== undefined) patch.notesJson = JSON.stringify(args.notes);
      if (args.status !== undefined) {
        patch.status = args.status;
        // Stamp completedAt the first time a run reaches a terminal state.
        if (TERMINAL_STATUSES.has(args.status) && existing.completedAt === undefined) {
          patch.completedAt = Date.now();
        }
      }
      ctx.db.updateWorkflowRun(args.id, patch);
      const updated = ctx.db.getWorkflowRun(args.id)!;
      return ok(`Updated workflow run ${args.id} [${updated.status}].`, {
        workflow: toView(updated),
      });
    },
  },
];
