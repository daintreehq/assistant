/**
 * UI-facing types. The Ink layer holds ephemeral presentation state (the
 * run-grouped transcript, pending confirm) while the runtime keeps durable
 * operational state in SQLite.
 *
 * The transcript is RUN-ORIENTED: a flat event stream is folded into turns. A
 * turn is one request → decision → delegated work → outcome, so the tree of
 * orchestration relationships is the primary visual structure.
 */
import type { ConfirmRequest } from "../tools/types.js";
import type {
  AuditRecord,
  QueueEvent,
  TimerRecord,
  WatcherRecord,
  WorkflowRunRecord,
} from "../schemas.js";
import type { McpStatus } from "../mcp/client.js";

export type ActivityState =
  | "queued"
  | "active"
  | "done"
  | "failed"
  | "waiting";

/** One delegated/tool action inside a turn — a branch of the run tree. */
export interface ActivityItem {
  /** The model's tool-call id; results match against this. */
  id: string;
  /** Internal tool name, e.g. `fs.read`. */
  name: string;
  /** Human verb, e.g. "Read", "Delegated". */
  label: string;
  /** Target of the verb, e.g. a relative path. */
  detail?: string;
  /** Raw args, kept for the expanded detail view only. */
  args?: unknown;
  /** Result summary once resolved. */
  summary?: string;
  state: ActivityState;
  startedAt: number;
  endedAt?: number;
}

export type TurnState = "active" | "complete" | "failed" | "cancelled";

export interface SystemNote {
  id: string;
  level: "info" | "warn" | "error";
  text: string;
  ts: number;
}

/** A user turn and everything Daintree did in response. */
export interface TurnCell {
  kind: "turn";
  id: string;
  /** Empty for system-origin turns (e.g. a scheduled run with no user prompt). */
  userText: string;
  assistantText: string;
  streaming: boolean;
  activities: ActivityItem[];
  notes: SystemNote[];
  state: TurnState;
  ts: number;
}

/** A standalone operational note (MCP connect, attention) outside any turn. */
export interface NoteCell {
  kind: "note";
  id: string;
  level: "info" | "warn" | "error";
  text: string;
  ts: number;
}

/** The result of a slash command rendered into the transcript. */
export interface CommandCell {
  kind: "command";
  id: string;
  title: string;
  text: string;
  ts: number;
}

export type TranscriptCell = TurnCell | NoteCell | CommandCell;

export interface PendingConfirm {
  id: string;
  request: ConfirmRequest;
  resolve: (approved: boolean) => void;
}

/**
 * Ephemeral per-session token accounting shown in the status line. Lives in React
 * state (not {@link DashboardState}, which is SQLite-backed) because it is reset
 * each session and accumulated from the agent's `usage` events. `costUsd` stays
 * undefined until a priced model reports usage; `contextTokens` /
 * `contextThreshold` carry the latest context-pressure reading (threshold 0 means
 * "no reading yet").
 */
export interface SessionUsage {
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
  /** Accumulated USD across priced calls; undefined until one is priced. */
  costUsd: number | undefined;
  /** Latest estimated conversation size — the context-pressure numerator. */
  contextTokens: number;
  /** Auto-compact threshold — the context-pressure denominator (0 = no reading). */
  contextThreshold: number;
  /** Tier of the most recent call, e.g. "large". */
  lastTier?: string;
  /** Concrete model id of the most recent call. */
  lastModel?: string;
}

export interface DashboardState {
  mcp: McpStatus;
  /** Durable issue/PR/work ledger rows. Optional for older tests/callers. */
  workflowRuns?: WorkflowRunRecord[];
  watchers: WatcherRecord[];
  timers: TimerRecord[];
  inbox: QueueEvent[];
  audit: AuditRecord[];
}
