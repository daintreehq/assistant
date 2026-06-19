/**
 * Shared Zod schemas and domain types for the Daintree Assistant CLI.
 *
 * These are the canonical contracts used across the model router, tools, daemon,
 * and storage layers. Keep them small and stable — tool modules and tests import
 * from here.
 */
import { z } from "zod";

/* -------------------------------------------------------------------------- */
/* Risk + permissions                                                          */
/* -------------------------------------------------------------------------- */

/** Risk class for a tool. Drives the confirmation matrix in safety/policy. */
export const RiskClass = z.enum([
  "read", // no state mutation
  "local", // mutates only CLI daemon state (timers/watchers/queue)
  "ui", // focuses/opens panels, changes Daintree UI state
  "terminal", // sends input / spawns visible terminals
  "project", // creates/deletes worktrees, runs recipes
  "git", // stages/commits/pushes/reverts
  "external", // forge / network / provider actions
  "system", // destructive or broad actions
]);
export type RiskClass = z.infer<typeof RiskClass>;

export const Tier = z.enum(["supervisor", "operator", "system"]);
export type Tier = z.infer<typeof Tier>;

export const ModelTier = z.enum(["small", "medium", "large"]);
export type ModelTier = z.infer<typeof ModelTier>;

/* -------------------------------------------------------------------------- */
/* Daintree state mirrors                                                      */
/* -------------------------------------------------------------------------- */

export const AgentState = z.enum([
  "idle",
  "working",
  "waiting",
  "directing",
  "completed",
  "exited",
]);
export type AgentState = z.infer<typeof AgentState>;

/* -------------------------------------------------------------------------- */
/* Tool result envelope                                                        */
/* -------------------------------------------------------------------------- */

export interface ToolError {
  code: string;
  message: string;
  recoverable: boolean;
  details?: unknown;
}

export interface ToolResult<T = unknown> {
  ok: boolean;
  result?: T;
  error?: ToolError;
  /** One-line, human/LLM-facing summary of what happened. */
  summary: string;
  /** Id of the audit_log row written for this call. */
  auditId?: string;
}

/* -------------------------------------------------------------------------- */
/* Queue events                                                                */
/* -------------------------------------------------------------------------- */

export const Severity = z.enum([
  "debug",
  "info",
  "attention",
  "urgent",
  "blocked",
  "done",
  "error",
]);
export type Severity = z.infer<typeof Severity>;

export const EventSource = z.enum([
  "timer",
  "terminal_watcher",
  "worktree_watcher",
  "workflow",
  "model_worker",
  "system",
  "user",
]);
export type EventSource = z.infer<typeof EventSource>;

export const EventTarget = z
  .object({
    projectId: z.string().optional(),
    worktreeId: z.string().optional(),
    terminalId: z.string().optional(),
    workflowRunId: z.string().optional(),
  })
  .strict();
export type EventTarget = z.infer<typeof EventTarget>;

export const RecommendedAction = z
  .object({
    label: z.string(),
    toolName: z.string(),
    args: z.unknown().optional(),
    risk: RiskClass.optional(),
    requiresConfirmation: z.boolean().optional(),
  })
  .strict();
export type RecommendedAction = z.infer<typeof RecommendedAction>;

export const QueuePublishArgs = z
  .object({
    source: EventSource,
    severity: Severity,
    title: z.string(),
    summary: z.string(),
    target: EventTarget.optional(),
    evidence: z.array(z.string()).optional(),
    recommendedActions: z.array(RecommendedAction).optional(),
    dedupeKey: z.string().optional(),
    ttlMs: z.number().optional(),
  })
  .strict();
export type QueuePublishArgs = z.infer<typeof QueuePublishArgs>;

export interface QueueEvent {
  id: string;
  source: EventSource;
  severity: Severity;
  title: string;
  summary: string;
  target?: EventTarget;
  evidence?: string[];
  recommendedActions?: RecommendedAction[];
  dedupeKey?: string;
  createdAt: number;
  /** Advances on each dedupe bump; createdAt stays fixed. Used for recency. */
  updatedAt?: number;
  expiresAt?: number;
  resolvedAt?: number;
  /** How many times this dedupeKey collapsed into the same event. */
  count: number;
}

/* -------------------------------------------------------------------------- */
/* Watch condition DSL                                                         */
/* -------------------------------------------------------------------------- */

export type WatchCondition =
  | { stateIs: AgentState }
  | { runtimeStatusIs: "running" | "exited" }
  | { contains: string }
  | { regex: string }
  | { noOutputForMs: number }
  | { modelJudge: string }
  | { all: WatchCondition[] }
  | { any: WatchCondition[] }
  | { not: WatchCondition };

export const WatchCondition: z.ZodType<WatchCondition> = z.lazy(() =>
  z.union([
    z.object({ stateIs: AgentState }).strict(),
    z
      .object({
        runtimeStatusIs: z.enum(["running", "exited"]),
      })
      .strict(),
    // Reject degenerate conditions at creation time rather than letting them
    // fail silently at evaluation: an empty `contains` matches every frame, an
    // invalid regex is caught-and-false'd by the engine so it never fires, a
    // non-positive `noOutputForMs` is nonsensical, and an empty `all`/`any`
    // group is vacuously true/false. Each would persist a watcher that can
    // never do its job — the exact false-supervision this guards against.
    z
      .object({
        contains: z
          .string()
          .refine((v) => v.trim().length > 0, "contains must be a non-empty, non-whitespace string"),
      })
      .strict(),
    z
      .object({
        // Reject both an empty pattern (matches every frame, like an empty
        // `contains`) and a syntactically invalid one (the engine catches-and-
        // false's it, so it would never fire).
        regex: z
          .string()
          .min(1, "regex must be a non-empty pattern")
          .refine(
            (val) => {
              try {
                new RegExp(val);
                return true;
              } catch {
                return false;
              }
            },
            { message: "regex must be a valid regular expression" },
          ),
      })
      .strict(),
    // `.int().positive()` also rejects Infinity (not an integer): without it,
    // `JSON.stringify(Infinity)` persists `null`, which the engine compares as 0
    // and fires the condition on the first check.
    z.object({ noOutputForMs: z.number().int().positive("noOutputForMs must be a positive integer (ms)") }).strict(),
    z
      .object({
        modelJudge: z
          .string()
          .refine((v) => v.trim().length > 0, "modelJudge must be a non-empty, non-whitespace string"),
      })
      .strict(),
    z.object({ all: z.array(WatchCondition).min(1, "all must contain at least one condition") }).strict(),
    z.object({ any: z.array(WatchCondition).min(1, "any must contain at least one condition") }).strict(),
    z.object({ not: WatchCondition }).strict(),
  ]),
);

/* -------------------------------------------------------------------------- */
/* Watcher classification (small-model output)                                 */
/* -------------------------------------------------------------------------- */

export const WatcherClassification = z.enum([
  "no_change",
  "still_working",
  "waiting_for_input",
  "permission_prompt",
  "command_failed",
  "tests_failed",
  "tests_passed",
  "merge_conflict",
  "completed_success",
  // The agent reports completion but a deterministic post-completion check found
  // uncommitted changes (or could not verify) — completion is NOT yet trustworthy,
  // so irreversible actions (commit/push/worktree.delete) must not be suggested.
  // Set ONLY by the engine's verification pass, never by the small model — it is
  // intentionally absent from the model-facing classification enum in prompts.
  "completed_unverified",
  "completed_unknown",
  "terminal_exited",
  "needs_large_model",
  "unknown",
]);
export type WatcherClassification = z.infer<typeof WatcherClassification>;

/* -------------------------------------------------------------------------- */
/* Post-completion verification                                                */
/* -------------------------------------------------------------------------- */

/**
 * Three-state verdict of a post-completion verification pass. Completion is judged
 * from evidence of correctness against a task's acceptance contract, NOT from git
 * cleanliness alone (a clean tree can mean the agent did nothing, never committed,
 * or never ran — see issue #83). Thin evidence is never silently upgraded to
 * success.
 *   - "verified" — the worktree is clean AND the acceptance contract is met
 *                  (or, absent a contract, the worktree is clean — legacy gate).
 *   - "failed"   — the acceptance contract was confidently NOT met.
 *   - "unknown"  — the evidence is inconclusive: uncommitted work remains, the
 *                  git state could not be read, or the acceptance judge was not
 *                  confident. A first-class, legitimate outcome — never treated as
 *                  success.
 */
export const VerificationVerdict = z.enum(["verified", "failed", "unknown"]);
export type VerificationVerdict = z.infer<typeof VerificationVerdict>;

/**
 * Structured evidence bundle from the read-only post-completion verification pass.
 * Beyond the deterministic git artifact state (via git.getProjectPulse) it can
 * carry the task's acceptance contract and the model judge's assessment of whether
 * the agent's terminal output satisfied it. Attached as queue-event evidence so the
 * conductor can require a `verified` result — not mere git cleanliness — before
 * ever suggesting irreversible git operations.
 *
 * `verdict` uses `.catch("unknown")` so a legacy persisted blob (old enum values
 * "clean"/"dirty") deserializes to the safe `unknown`, never a false `verified`.
 * New fields are optional so old blobs still parse.
 */
export const VerificationResult = z
  .object({
    verdict: VerificationVerdict.catch("unknown"),
    /** True when the worktree has uncommitted changes (artifact state). */
    hasGitChanges: z.boolean(),
    /** Count of changed files when derivable from the pulse, else 0. */
    changedFiles: z.number().int().min(0).default(0),
    /** Changed-file paths when the pulse exposes them, else empty. */
    changedFileList: z.array(z.string()).default([]),
    /** One-line human/LLM-facing description of the git state observed. */
    gitSummary: z.string(),
    /** The task-specific acceptance contract the verdict was judged against. */
    acceptanceCriteria: z.string().optional(),
    /** The model judge's one-line rationale for whether the contract was met. */
    criteriaMetSummary: z.string().optional(),
    /** Unresolved warnings surfaced during verification (non-fatal). */
    unresolvedWarnings: z.array(z.string()).default([]),
  })
  .strip();
export type VerificationResult = z.infer<typeof VerificationResult>;

/** Evidence-string prefix that carries a serialized VerificationResult. */
export const VERIFICATION_EVIDENCE_PREFIX = "verification:";

/** Schema the small watcher model must return as a JSON object. */
export const WatcherVerdict = z
  .object({
    classification: WatcherClassification,
    confidence: z.number().min(0).max(1),
    summary: z.string(),
    evidence: z.array(z.string()).default([]),
    recommendedAction: z
      .enum([
        "none",
        "focus_terminal",
        "ask_user",
        "send_input",
        "spawn_helper",
        "open_review",
      ])
      .default("none"),
  })
  .strict();
export type WatcherVerdict = z.infer<typeof WatcherVerdict>;

/**
 * Answer to a single `modelJudge` condition — a binary yes/no predicate about the
 * terminal, evaluated against the judge's own question. This is deliberately NOT
 * the WatcherVerdict classification enum: that enum carries operational terminal
 * semantics ("tests_failed", "waiting_for_input"), whereas a judge asks a specific
 * logical question ("did the migration finish?") whose answer is just true/false.
 * `reason` is placed BEFORE `matched` on purpose: emitting the rationale first
 * gives the small model an implicit chain-of-thought, which measurably improves the
 * accuracy of the boolean it then commits to.
 */
export const ModelJudgeAnswer = z
  .object({
    reason: z.string(),
    confidence: z.number().min(0).max(1),
    matched: z.boolean(),
  })
  .strict();
export type ModelJudgeAnswer = z.infer<typeof ModelJudgeAnswer>;

/* -------------------------------------------------------------------------- */
/* Persisted records (DB rows)                                                 */
/* -------------------------------------------------------------------------- */

export interface TimerRecord {
  id: string;
  title: string;
  fireAt: number; // wall-clock ms
  repeatEveryMs?: number;
  repeatUntil?: number;
  maxRuns?: number;
  runCount: number;
  payloadType: "enqueue" | "run_check" | "call_safe_tool";
  payloadJson: string;
  targetJson?: string;
  status: "scheduled" | "fired" | "cancelled" | "done";
  createdAt: number;
  lastFiredAt?: number;
}

export interface WatcherRecord {
  id: string;
  kind: "terminal";
  title: string;
  goal: string;
  targetsJson: string; // string[] of terminalIds
  cadenceMs: number;
  /** True for supervisor watchers attached to CLI-spawned worker terminals.
   * These default to a fast cadence and are floored at the scheduler tick so a
   * stalled worker surfaces quickly. User-created background watchers are false. */
  isSupervisor?: boolean;
  modelTier: ModelTier;
  startAfterMs?: number;
  stopAfterMs?: number;
  stopWhenJson?: string;
  alertWhenJson?: string;
  optionsJson?: string;
  status:
    | "created"
    | "active"
    | "paused"
    | "condition_met"
    | "timeout"
    | "cancelled"
    | "error";
  lastClassification?: string;
  lastCheckedAt?: number;
  nextCheckAt: number;
  createdAt: number;
}

export interface AuditRecord {
  id: string;
  ts: number;
  actor: "main" | "watcher" | "timer" | "workflow" | "system";
  toolName: string;
  argsJson: string;
  // "grant_ok": a non-interactive actor ran a confirm-required tool because a
  // valid scoped automation grant authorized (and consumed) the use.
  outcome: "ok" | "error" | "denied" | "dedup" | "grant_ok";
  durationMs: number;
  summary: string;
  resultJson?: string;
  // Provenance of the grant that authorized a "grant_ok" outcome, so audit
  // summaries can distinguish a purely local grant from a (future) Daintree
  // session grant. Absent on every non-grant_ok row. See AutomationGrantSource.
  grantSource?: AutomationGrantSource;
  /** Id of the grant consumed for a "grant_ok" outcome. Absent otherwise. */
  grantId?: string;
  /**
   * Id of the run (one `AgentSession.send()` turn) that triggered this dispatch,
   * so every tool call in a turn can be grouped with the run's event log in
   * `run_events`. Absent for dispatches outside a run (e.g. the scheduler-driven
   * watcher/timer paths that build their own context).
   */
  runId?: string;
}

/**
 * One append-only entry in a run's event log. A "run" is a single
 * `AgentSession.send()` turn; its events (assistant start/end, tool call/result,
 * errors, info) are recorded in `seq` order so a finished run can be replayed
 * faithfully. Cross-references `AuditRecord.runId` for the tool-dispatch detail.
 */
export interface RunEventRecord {
  /** `rne_<uuid8>` — unique per event. */
  id: string;
  /** The run this event belongs to (`AuditRecord.runId`). */
  runId: string;
  /** Monotonic position within the run, starting at 0. */
  seq: number;
  ts: number;
  /** Event kind, e.g. "assistant:start" | "tool:call" | "tool:result". */
  type: string;
  /** JSON-serialized payload, or absent for zero-payload events. */
  payload?: string;
}

/**
 * One row of the run index — an aggregate over `run_events` grouped by run. Backs
 * the no-argument `/explain` listing so a user can discover recent run ids without
 * already knowing one. Not persisted directly; computed on demand by `Db.listRuns`.
 */
export interface RunSummaryRecord {
  /** The run id (`AgentSession.send()` turn). */
  runId: string;
  /** Timestamp of the run's first event (its start). */
  firstTs: number;
  /** Timestamp of the run's last event (its end). */
  lastTs: number;
  /** How many events the run recorded. */
  eventCount: number;
}

/* -------------------------------------------------------------------------- */
/* Automation grants                                                           */
/* -------------------------------------------------------------------------- */

/** Which kind of non-interactive actor a grant is scoped to. */
export type AutomationGrantActorType = "watcher" | "timer";

/**
 * Where a grant's authority is anchored.
 *   - "local"    — authority lives entirely in this CLI's SQLite store (today's
 *                  behaviour, and the only kind mintable now).
 *   - "daintree" — the grant mirrors a Daintree native session-scoped grant, so
 *                  it is visible/revocable from the Daintree app. Reserved for
 *                  when Daintree exposes an external grants API; no code path
 *                  mints one yet. The column exists so the two lifecycles never
 *                  diverge silently once the bridge lands.
 */
export type AutomationGrantSource = "local" | "daintree";

/**
 * A scoped, expiring authorization that lets a specific watcher/timer perform a
 * bounded number of confirm-required follow-up mutations without an interactive
 * prompt. Minted by the main actor; consumed atomically at dispatch time.
 *
 * The allowlist is two nullable JSON-array columns rather than a SQL
 * discriminated union: a call is authorized when the tool name is in
 * `allowedToolNamesJson` OR its risk class is in `allowedRiskClassesJson` (union
 * semantics). At least one list must be non-empty — enforced in the TypeScript
 * layer, not the schema.
 *
 * `revokedAt` means explicit revocation only. Use-exhaustion is implicit via
 * `usesRemaining = 0`; it does NOT stamp `revokedAt`.
 */
export interface AutomationGrantRecord {
  id: string;
  actorId: string; // wch_… or tmr_…
  actorType: AutomationGrantActorType;
  allowedRiskClassesJson: string | null; // JSON array of RiskClass, or null
  allowedToolNamesJson: string | null; // JSON array of tool names, or null
  expiresAt: number; // wall-clock ms
  maxUses: number;
  usesRemaining: number;
  revokedAt: number | null;
  createdAt: number;
  // Provenance of the grant's authority. Always "local" today; the column lets
  // a future Daintree-backed grant be told apart in listings and audit.
  source: AutomationGrantSource;
}

export interface ConversationMessageRecord {
  id: string;
  sessionId: string;
  seq: number;
  role: "system" | "user" | "assistant" | "tool";
  content: string;
  toolCallsJson?: string;
  toolCallId?: string;
  createdAt: number;
}

/** One small-model recipe-selection decision; the dataset for improving recipes. */
export interface RecipeSelectionLogRecord {
  id: string;
  ts: number;
  sessionId: string;
  userInput: string;
  selectedRecipeIdsJson: string;
  confidence: number;
  taskType?: string;
  reason?: string;
}

/* -------------------------------------------------------------------------- */
/* Workflow ledger                                                             */
/* -------------------------------------------------------------------------- */

/**
 * Lifecycle of a single unit of orchestrated work (one issue/PR). Mirrors the
 * status conventions already used by timers and watchers:
 *   - pending   — created, work not yet started
 *   - active    — work in progress (a terminal/worker is running)
 *   - blocked   — stalled on a human decision or external dependency
 *   - done      — completed successfully (terminal)
 *   - cancelled — abandoned on purpose (terminal)
 *   - failed    — ended unsuccessfully (terminal)
 */
export type WorkflowRunStatus =
  | "pending"
  | "active"
  | "blocked"
  | "done"
  | "cancelled"
  | "failed";

/**
 * A durable ledger row tying together the terminals, watchers, and queue events
 * that make up one unit of issue/PR work, plus the single next required action.
 * Lets the assistant answer "what is this terminal working on and what is the
 * next required action" without re-inferring state from scratch after a restart.
 *
 * The `*Json` columns hold serialized data (matching the store-wide convention,
 * e.g. WatcherRecord.targetsJson):
 *   - terminalIdsJson / watcherIdsJson / queueEventIdsJson — JSON `string[]` of
 *     the linked Daintree/daemon ids
 *   - notesJson — JSON `string[]` of freeform context notes
 *   - nextActionJson — a single serialized {@link RecommendedAction}
 */
export interface WorkflowRunRecord {
  id: string; // wfr_<uuid8>
  issueNumber?: number;
  issueUrl?: string;
  issueTitle?: string;
  branch?: string;
  worktreeId?: string;
  prNumber?: number;
  prUrl?: string;
  terminalIdsJson?: string; // JSON string[]
  watcherIdsJson?: string; // JSON string[]
  queueEventIdsJson?: string; // JSON string[]
  status: WorkflowRunStatus;
  nextActionJson?: string; // serialized RecommendedAction
  notesJson?: string; // JSON string[]
  createdAt: number;
  /** Advances on every update; createdAt stays fixed. Used for recency ordering. */
  updatedAt: number;
  /** Stamped once when status first reaches a terminal state. */
  completedAt?: number;
}

/**
 * Where a memory came from:
 *   - user      — the human stated it explicitly ("always deploy from main").
 *   - assistant — the assistant derived/confirmed it during a session.
 *   - compact   — reserved for facts distilled by auto-compaction. Not yet
 *                 written by any tool; held open so a later issue can wire
 *                 `compact()` to persist key facts without an API change.
 */
export type MemorySource = "user" | "assistant" | "compact";

/**
 * A single durable, project-scoped fact / decision / procedure the assistant can
 * recall across sessions. The DB file is already per-project (config.ts diverges
 * stateDir on DAINTREE_PROJECT_ID), so there is no projectId column — one DB is
 * one project's memory.
 *
 * Recall is backed by an FTS5 external-content virtual table (`memories_fts`)
 * shadowing this row's `content`, kept in sync by triggers. `forget` is a soft
 * delete (stamps `deletedAt`) so a fact the assistant drops isn't immediately
 * re-derived and re-saved; recall/list filter `deletedAt IS NULL`. `pinnedAt`
 * marks a memory the user wants kept regardless of any future pruning.
 */
export interface MemoryRecord {
  id: string; // mem_<uuid8>
  content: string;
  /** Optional free tag for filtered recall, e.g. "convention", "decision", "fix". */
  category?: string;
  source: MemorySource;
  /** Non-null ⇒ pinned. */
  pinnedAt?: number;
  /** Non-null ⇒ soft-deleted ("forgotten"); excluded from recall/list. */
  deletedAt?: number;
  createdAt: number;
  updatedAt: number;
}

/* -------------------------------------------------------------------------- */
/* Recipe run state (step-level progress + checkpoints)                        */
/* -------------------------------------------------------------------------- */

/**
 * Lifecycle of one recipe's step-level execution within a session:
 *   - active    — the recipe is mid-run; `currentStep` points at the live step
 *   - completed — the model advanced past the final step (terminal)
 *   - abandoned — the run was explicitly closed without finishing (terminal)
 *
 * Recipes themselves stay stateless prompt injections; this record is the only
 * place runtime progress lives, so a multi-step recipe can be supervised as it
 * runs and resumed (via recipe.run.get) if a turn is interrupted.
 */
export type RecipeRunStatus = "active" | "completed" | "abandoned";

/** Outcome the model reports for a single numbered step. */
export type RecipeStepStatus = "done" | "skipped";

/**
 * One checkpoint entry: a numbered step the model has reported on. `index` is
 * 1-based (matching the runbook's numbered steps), `ts` is when the transition
 * was recorded, and `notes` is an optional freeform checkpoint note.
 */
export interface RecipeStepProgress {
  index: number;
  status: RecipeStepStatus;
  notes?: string;
  ts: number;
}

/**
 * Durable step-level progress for one (session, recipe) pair. The natural key is
 * `(sessionId, recipeId)` — the selector caps a session at three mutually
 * exclusive recipes, so a single active run per recipe is sufficient.
 *
 * `stepsJson` holds a serialized {@link RecipeStepProgress}`[]` (matching the
 * store-wide `*Json` convention); the tool layer deserializes it. `currentStep`
 * is denormalized for a fast "where did we leave off" read without parsing JSON
 * (0 = not started).
 */
export interface RecipeRunStateRecord {
  id: string; // rrs_<uuid8>
  sessionId: string;
  recipeId: string;
  /** The step now active. 0 = not started; stays at the final step once done. */
  currentStep: number;
  stepsJson: string; // JSON RecipeStepProgress[]
  status: RecipeRunStatus;
  startedAt: number;
  /** Advances on every transition; startedAt stays fixed. Used for recency. */
  updatedAt: number;
  /** Stamped once when status first reaches a terminal state. */
  completedAt?: number;
}

/* -------------------------------------------------------------------------- */
/* One-shot structured (JSONL) output                                          */
/* -------------------------------------------------------------------------- */

/**
 * Schema version for the one-shot `--json` output contract. A plain integer, not
 * semver: this is a pre-release tool whose only structured-output consumers are
 * scripts/CI, so one monotonic number is enough — bump it only on a *breaking*
 * change to the line shape (renamed/removed field, changed type), never on an
 * additive one.
 */
export const JSON_OUTPUT_SCHEMA_VERSION = 1;

/**
 * Exit-code contract for one-shot mode. Defined here (not buried in the CLI) so
 * scripts can depend on a documented, stable mapping:
 *   - 0 success   — the turn completed and the assistant replied.
 *   - 1 error     — a model/general error ended the turn (stream error, max
 *                   iterations, or an unexpected throw). This is also the
 *                   process-wide catch-all that already existed.
 *   - 2 cancelled — the turn was cancelled mid-flight.
 *   - 3 toolFailure — RESERVED. The agent loop has no terminal tool-failure
 *                   signal today: failed tool calls are fed back to the model as
 *                   recoverable context and the turn continues, so a turn that
 *                   ends after a tool error still exits 0 (the model chose to
 *                   stop). Kept in the contract so a future loop change can adopt
 *                   it without renumbering the codes scripts already rely on.
 */
export const ONE_SHOT_EXIT_CODE = {
  success: 0,
  error: 1,
  cancelled: 2,
  toolFailure: 3,
} as const;

/** Terminal status of a one-shot turn, mirrored in the `result` envelope. */
export const JsonOutputStatus = z.enum(["success", "error", "cancelled"]);
export type JsonOutputStatus = z.infer<typeof JsonOutputStatus>;

/**
 * Event `type` strings emitted on the one-shot JSONL stream. These deliberately
 * reuse the durable {@link RunEventRecord} type strings (`assistant:start`,
 * `tool:call`, …) so the live stream and the replayable DB log describe a run the
 * same way — one vocabulary, two transports. `result` is the extra terminal line
 * unique to this stream (the DB log ends a run differently).
 */
export const JsonlEventType = z.enum([
  "assistant:start",
  "assistant:content",
  "assistant:end",
  "assistant:cancelled",
  "tool:call",
  "tool:result",
  "error",
  "info",
  "result",
]);
export type JsonlEventType = z.infer<typeof JsonlEventType>;

/**
 * Fields every JSONL line carries. Per-type payload fields ride alongside these
 * (the schema is permissive on extras via `passthrough`), so a consumer can
 * always read `type`/`ts`/`seq` without knowing the specific event shape.
 * `seq` is monotonic within a single run, starting at 0.
 */
export const JsonlEventSchema = z
  .object({
    type: JsonlEventType,
    ts: z.number(),
    seq: z.number().int().min(0),
  })
  .passthrough();
export type JsonlEvent = z.infer<typeof JsonlEventSchema>;

/**
 * The final line of every one-shot `--json` run: a self-contained summary of the
 * outcome. Callers that only want the result can read this last line and ignore
 * the streamed events. `exitCode` mirrors the process exit code so a consumer
 * parsing stdout never has to also capture `$?`.
 */
export const JsonResultEnvelopeSchema = z
  .object({
    type: z.literal("result"),
    ts: z.number(),
    seq: z.number().int().min(0),
    schemaVersion: z.literal(JSON_OUTPUT_SCHEMA_VERSION),
    status: JsonOutputStatus,
    // 0 | 1 | 2 today; 3 (toolFailure) is reserved (see ONE_SHOT_EXIT_CODE).
    exitCode: z.union([z.literal(0), z.literal(1), z.literal(2)]),
    /** The final assistant text (empty string if the turn produced none). */
    content: z.string(),
    /** Present only when `status` is "error"; null otherwise. */
    error: z.object({ message: z.string() }).nullable(),
  })
  .strict();
export type JsonResultEnvelope = z.infer<typeof JsonResultEnvelopeSchema>;

/* -------------------------------------------------------------------------- */
/* Agent launch operations (idempotent spawn saga)                             */
/* -------------------------------------------------------------------------- */

/**
 * Stages a spawned-agent launch advances through. Spawning is a multi-step
 * external operation (MCP `agent.launch` → bind terminal → attach watcher) with
 * no transactional guarantee, so the durable record below tracks where a launch
 * got to. This lets a retry reconcile a partial failure instead of blindly
 * launching a second agent.
 *
 *   - launch_requested — row written *before* the MCP call (write-ahead); a crash
 *                        here leaves a recoverable record, not a ghost agent.
 *   - agent_started    — `agent.launch` returned without error.
 *   - terminal_bound   — a terminalId was extracted from the response.
 *   - watcher_attached — the supervising watcher was inserted.
 *   - confirmed        — full success (terminal).
 *   - failed           — `agent.launch` returned an explicit error, or the
 *                        session ended before confirmation (terminal).
 *   - ambiguous        — the launch outcome is unknown: the response carried no
 *                        terminalId, or the transport threw (the request may have
 *                        reached Daintree). Needs reconciliation before retry.
 *
 * Only `confirmed` and `failed` are terminal; an `ambiguous` record stays live so
 * a retry can reconcile it (via `terminal.list`) within the same session.
 */
export type AgentLaunchStage =
  | "launch_requested"
  | "agent_started"
  | "terminal_bound"
  | "watcher_attached"
  | "confirmed"
  | "failed"
  | "ambiguous";

/**
 * A durable record of one `agentTask.spawnForEdits` launch, keyed by a
 * deterministic `idempotencyKey` (a hash of the task's identity — taskPrompt,
 * worktreeId, agentId, mode). A retry of the same logical task finds the in-flight
 * record and reconciles instead of duplicating; a completed (`confirmed`/`failed`)
 * record never blocks a fresh run of the same task later.
 *
 * `name` is the deterministic launch name passed to `agent.launch` (e.g.
 * "Claude: auth refactor"); it is stored so an ambiguous launch can be
 * reconciled by matching it against `terminal.list`. Records are session-scoped:
 * `cancelStaleAgentLaunches()` marks any non-terminal row `failed` on DB open,
 * since the terminals/watchers they reference live only for the session.
 */
export interface AgentLaunchRecord {
  id: string; // agt_<uuid8>
  /** Deterministic content hash of the task identity; the dedup key. */
  idempotencyKey: string;
  agentId: string;
  worktreeId?: string;
  mode: string; // "edit" | "explore"
  title: string;
  /** Deterministic launch name, for terminal.list reconciliation. */
  name: string;
  terminalId?: string;
  watcherId?: string;
  stage: AgentLaunchStage;
  errorCode?: string;
  errorMessage?: string;
  createdAt: number;
  /** Advances on every stage transition; createdAt stays fixed. */
  updatedAt: number;
}

/* -------------------------------------------------------------------------- */
/* Helpers                                                                     */
/* -------------------------------------------------------------------------- */

/** Evaluate the leaf/composite parts of a WatchCondition that are deterministic. */
export function isCompositeCondition(
  c: WatchCondition,
): c is { all: WatchCondition[] } | { any: WatchCondition[] } | { not: WatchCondition } {
  return "all" in c || "any" in c || "not" in c;
}
