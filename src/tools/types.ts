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
}

export interface ToolDef<A = any> {
  name: string;
  description: string;
  risk: RiskClass;
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
