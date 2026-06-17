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
    z.object({ contains: z.string() }).strict(),
    z.object({ regex: z.string() }).strict(),
    z.object({ noOutputForMs: z.number() }).strict(),
    z.object({ modelJudge: z.string() }).strict(),
    z.object({ all: z.array(WatchCondition) }).strict(),
    z.object({ any: z.array(WatchCondition) }).strict(),
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
 * Deterministic verdict of a post-completion reconciliation pass.
 *   - "clean"   — the worktree has no uncommitted changes; completion is trustworthy.
 *   - "dirty"   — uncommitted changes remain; the agent's work needs review first.
 *   - "unknown" — the git state could not be read (MCP down / unrecognized shape);
 *                 treat as not-yet-verified, never as clean.
 */
export const VerificationVerdict = z.enum(["clean", "dirty", "unknown"]);
export type VerificationVerdict = z.infer<typeof VerificationVerdict>;

/**
 * Structured result of the read-only post-completion verification pass. Daintree
 * exposes no exit code and no test/lint runner, so the verdict is derived solely
 * from the worktree's git cleanliness (via git.getProjectPulse). This is attached
 * as queue-event evidence so the conductor can require a clean result before ever
 * suggesting irreversible git operations.
 */
export const VerificationResult = z
  .object({
    verdict: VerificationVerdict,
    /** True when the worktree has uncommitted changes. */
    hasGitChanges: z.boolean(),
    /** Count of changed files when derivable from the pulse, else 0. */
    changedFiles: z.number().int().min(0).default(0),
    /** One-line human/LLM-facing description of the git state observed. */
    gitSummary: z.string(),
  })
  .strict();
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
  kind: "terminal" | "worktree";
  title: string;
  goal: string;
  targetsJson: string; // string[] of terminalIds / worktreeIds
  cadenceMs: number;
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
  outcome: "ok" | "error" | "denied" | "dedup";
  durationMs: number;
  summary: string;
  resultJson?: string;
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
/* Helpers                                                                     */
/* -------------------------------------------------------------------------- */

/** Evaluate the leaf/composite parts of a WatchCondition that are deterministic. */
export function isCompositeCondition(
  c: WatchCondition,
): c is { all: WatchCondition[] } | { any: WatchCondition[] } | { not: WatchCondition } {
  return "all" in c || "any" in c || "not" in c;
}
