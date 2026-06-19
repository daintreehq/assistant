/**
 * Tool framework contracts. Every tool module exports one or more ToolDef objects
 * and a register function. The agent loop, watchers, and timers all dispatch
 * through the ToolRegistry, which applies the safety policy and writes audit rows.
 */
import type { z } from "zod";
import type { AppConfig } from "../config.js";
import type { DaintreeMcpClient } from "../mcp/client.js";
import type { Db } from "../storage/db.js";
import type { Queue } from "../queue.js";
import type { ModelRouter } from "../models/router.js";
import type { RiskClass, ToolResult } from "../schemas.js";

export type ToolActor = "main" | "watcher" | "timer" | "workflow" | "system";

export interface ConfirmRequest {
  toolName: string;
  risk: RiskClass;
  summary: string;
  args: unknown;
  /**
   * Plain-English statement of what approving will actually do — the *consequence*
   * (what's touched, whether it's reversible, any network/secret exposure), not the
   * raw risk class. The approval sheet leads with this; when absent it falls back to
   * a per-risk-class phrase. Keep it to one short, user-facing line (it's truncated).
   */
  consequence?: string;
}

/** Everything a tool handler can reach. Built once at startup. */
export interface ToolContext {
  config: AppConfig;
  mcp: DaintreeMcpClient;
  db: Db;
  queue: Queue;
  router: ModelRouter;
  projectPath: string;
  /**
   * Id of the conversation session this dispatch belongs to. Used by recipe
   * step-progress tools to key durable checkpoints to the live session. Absent
   * in stripped-down test contexts; tools that need it fail gracefully.
   */
  sessionId?: string;
  actor: ToolActor;
  /**
   * Entity id of the non-interactive actor behind this dispatch — the watcher
   * (`wch_…`) or timer (`tmr_…`) record id. Set by the scheduler so the registry
   * can look up a scoped automation grant tied to that specific actor. Absent for
   * the main (interactive) actor.
   */
  actorId?: string;
  /**
   * Id of the run (one `AgentSession.send()` turn) this dispatch belongs to.
   * Stamped onto each audit row so every tool call in a turn can be grouped with
   * the run's event log. Set per-turn via a derived context in `AgentSession.send()`;
   * absent for the base context and for scheduler-built (watcher/timer) contexts.
   */
  runId?: string;
  /**
   * Abort signal for the turn this dispatch belongs to (the UI's Escape-to-cancel).
   * Stamped per-turn by `AgentSession.send()`; absent for non-interactive actors
   * (watcher/timer/workflow build their own context and never set it). Long-running
   * handlers — MCP calls, terminal-extraction polls, agent launches — thread it into
   * their blocking work and, when it fires, stop early and return `fail("CANCELLED", …)`
   * rather than running to completion in the background after the user has moved on.
   */
  signal?: AbortSignal;
  /**
   * Tool names offered to the model in the current turn's tool spec — the core ∪
   * active-recipe projection (or the read-only inspection set). Discovery tools
   * (`tool.search`, `daintree.listTools`) cross-reference this to mark whether each
   * tool they surface is actually `callable` this turn, so they never advertise a
   * tool the model can't invoke. `undefined` ⇒ unconstrained (all tools callable).
   * Set per-turn by `AgentSession.runTurn`; absent for watcher/timer contexts.
   */
  activeToolNames?: string[];
  /** Ask the user to approve a mutating action. Returns true if approved. */
  confirm: (req: ConfirmRequest) => Promise<boolean>;
  /** Print an out-of-band line to the user (e.g. "spawned watcher wch_..."). */
  log: (msg: string) => void;
  /**
   * Whether the in-process scheduler/daemon is running. When false (e.g. a
   * one-shot non-interactive run), timers and watchers are persisted but will
   * not fire until the assistant runs interactively — tools say so rather than
   * implying they'll be monitored. Absent ⇒ assume active.
   */
  daemonActive?: () => boolean;
  /**
   * Session-scoped store for oversized tool results. When a serialized tool result
   * exceeds the inline size limit, the agent loop stashes the full JSON envelope
   * here under a generated `artifact_…` id and hands the model a compact, valid-JSON
   * stub instead of a string sliced mid-structure. The `artifact.read` tool pages
   * back through it. In-memory and session-lived by design — the ids are only
   * meaningful within the turn sequence that produced them. Absent in stripped-down
   * test/scheduler contexts; `artifact.read` fails gracefully when it is missing.
   */
  artifactStore?: Map<string, string>;
}

export interface ToolDef<A = any> {
  name: string;
  description: string;
  risk: RiskClass;
  /**
   * Optional one-line, user-facing consequence shown on the approval sheet (what
   * this action does to the user's resources, and whether it's reversible). Unlike
   * `description` (which is written for the model and can be long/instructional),
   * this is short prose for a human deciding Y/N. Worth setting for any tool whose
   * risk class always confirms; the UI falls back to a per-risk phrase otherwise.
   */
  consequence?: string;
  /** JSON Schema for the OpenAI function `parameters` field. */
  parameters: Record<string, unknown>;
  /** Optional Zod schema for runtime validation of parsed args. */
  schema?: z.ZodType<A>;
  /** Hint for the model + a sanity flag enforced by the registry. */
  readOnly?: boolean;
  handler: (args: A, ctx: ToolContext) => Promise<ToolResult>;
}

/* ----------------------------- result helpers ---------------------------- */

export function ok<T>(summary: string, result?: T): ToolResult<T> {
  return { ok: true, summary, result };
}

export function fail(
  code: string,
  message: string,
  opts: { recoverable?: boolean; details?: unknown } = {},
): ToolResult {
  return {
    ok: false,
    summary: message,
    error: {
      code,
      message,
      recoverable: opts.recoverable ?? true,
      details: opts.details,
    },
  };
}

/** Standard empty-object JSON schema for tools with no args. */
export const NO_ARGS: Record<string, unknown> = {
  type: "object",
  properties: {},
  additionalProperties: false,
};
