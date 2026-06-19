/**
 * Translation layer between the assistant runtime and the native-host wire
 * protocol. Adapts the in-process {@link AgentEventSink} and the tool confirm
 * hook into {@link HostEvent}s, and turns inbound approval decisions back into
 * the boolean the tool dispatcher awaits.
 *
 * Kept free of any `parentPort` dependency: it emits through an injected `post`
 * callback so it can be unit-tested without an Electron utility process. The
 * host entry (`index.ts`) owns the transport and the command loop.
 *
 * Turn model: one assistant turn spans a whole `AgentSession.send()` call, even
 * when that call streams across several model iterations punctuated by tool
 * calls. The agent loop fires `assistantStart()` once per iteration, so only
 * the first opens the turn; later ones continue streaming into it, and tool
 * calls nest under it via `turnId`. The host brackets each exchange with
 * {@link startExchange} (user turn) and {@link settleTurn} (close any dangling
 * assistant turn after `send()` returns).
 */
import { randomUUID } from "node:crypto";
import type { AgentEventSink } from "../agent/events.js";
import type { RiskClass, ToolResult } from "../schemas.js";
import {
  type AuditResult,
  type AuditSeverity,
  type ConfirmationDecision,
  type HostEvent,
  type TurnOutcomeClass,
  severityForResult,
} from "./protocol.js";

export interface HostBridgeOptions {
  sessionId: string;
  post: (event: HostEvent) => void;
  /** Look up a tool's declared risk to set the `danger` display hint. */
  riskOf?: (toolName: string) => RiskClass | undefined;
  /** Injectable clock (tests). */
  now?: () => number;
  /** A confirm request unanswered past this many ms resolves as `timeout`. */
  approvalTimeoutMs?: number;
}

interface PendingApproval {
  resolve: (decision: ConfirmationDecision) => void;
  timer: ReturnType<typeof setTimeout> | null;
}

const DEFAULT_APPROVAL_TIMEOUT_MS = 5 * 60_000;
const ARGS_SUMMARY_MAX_STRING = 80;

export class HostBridge {
  private readonly sessionId: string;
  private readonly post: (event: HostEvent) => void;
  private readonly riskOf: (toolName: string) => RiskClass | undefined;
  private readonly now: () => number;
  private readonly approvalTimeoutMs: number;

  private activeTurnId: string | null = null;
  private interrupted = false;
  private readonly pendingApprovals = new Map<string, PendingApproval>();
  /** `startedAt` by tool-call id, set on `tool:started`, drained on settle. */
  private readonly toolStartedAt = new Map<string, number>();

  constructor(opts: HostBridgeOptions) {
    this.sessionId = opts.sessionId;
    this.post = opts.post;
    this.riskOf = opts.riskOf ?? (() => undefined);
    this.now = opts.now ?? Date.now;
    this.approvalTimeoutMs = opts.approvalTimeoutMs ?? DEFAULT_APPROVAL_TIMEOUT_MS;
  }

  /** The sink to hand to `App.setHooks({ agentEvents })`. */
  readonly sink: AgentEventSink = {
    assistantStart: () => {
      if (this.interrupted || this.activeTurnId) return;
      this.activeTurnId = this.genId("turn");
      this.post({
        type: "turn:start",
        sessionId: this.sessionId,
        turnId: this.activeTurnId,
        role: "assistant",
        startedAt: this.now(),
      });
    },
    assistantToken: (chunk: string) => {
      if (this.interrupted || !this.activeTurnId) return;
      this.post({ type: "turn:token", sessionId: this.sessionId, turnId: this.activeTurnId, chunk });
    },
    assistantEnd: (content: string) => {
      if (!this.activeTurnId) return;
      this.closeTurn(content.trim().length > 0 ? "answered" : "unknown");
    },
    assistantCancelled: () => {
      // User aborted the turn — close it as a clean cancellation, not a failure.
      if (this.activeTurnId) this.closeTurn("cancelled");
    },
    toolCall: (event) => {
      if (this.interrupted) return;
      this.toolStartedAt.set(event.id, event.startedAt);
      this.post({
        type: "tool:started",
        sessionId: this.sessionId,
        toolCallId: event.id,
        toolId: event.name,
        argsSummary: redactArgs(event.args),
        startedAt: event.startedAt,
        turnId: this.activeTurnId ?? undefined,
        danger: this.isDanger(event.name),
      });
    },
    toolResult: (event) => {
      if (this.interrupted) return;
      const startedAt = this.toolStartedAt.get(event.id);
      this.toolStartedAt.delete(event.id);
      const audit = resultToAudit(event.result);
      this.post({
        type: "tool:settled",
        sessionId: this.sessionId,
        toolCallId: event.id,
        toolId: event.name,
        durationMs: startedAt !== undefined ? Math.max(0, event.endedAt - startedAt) : 0,
        result: audit.result,
        severity: audit.severity,
        errorCode: audit.errorCode,
        turnId: this.activeTurnId ?? undefined,
      });
    },
    error: (message: string) => {
      this.post({ type: "host:error", sessionId: this.sessionId, code: "turn-error", message });
      if (this.activeTurnId) this.closeTurn("unknown");
    },
    // Out-of-band informational lines have no protocol channel; intentionally
    // dropped (drift warnings etc. still reach the audit log via MCP).
    info: () => {},
  };

  /**
   * Bracket the start of a user exchange: reset per-turn state and emit the
   * instantaneous user turn. The prompt text itself is not carried — Daintree
   * originated the `prompt` command and already has it.
   */
  startExchange(): void {
    this.interrupted = false;
    this.activeTurnId = null;
    const turnId = this.genId("turn");
    const at = this.now();
    this.post({ type: "turn:start", sessionId: this.sessionId, turnId, role: "user", startedAt: at });
    this.post({ type: "turn:end", sessionId: this.sessionId, turnId, endedAt: at });
  }

  /** Close any assistant turn still open after `send()` returns (or was interrupted). */
  settleTurn(outcome: TurnOutcomeClass = "unknown"): void {
    if (this.activeTurnId) this.closeTurn(outcome);
  }

  /** Mark the in-flight turn interrupted: stop forwarding its output. */
  interrupt(): void {
    if (!this.activeTurnId) return;
    this.interrupted = true;
    this.closeTurn("agent-stuck");
  }

  /**
   * Bridge the tool confirm hook. Emits `approval:requested` and resolves the
   * boolean the dispatcher awaits when a matching `approval:decide` arrives (or
   * the request times out).
   */
  confirm(req: { toolName: string; summary: string }): Promise<boolean> {
    const approvalId = this.genId("apr");
    this.post({
      type: "approval:requested",
      sessionId: this.sessionId,
      approvalId,
      toolId: req.toolName,
      summary: req.summary,
      requestedAt: this.now(),
      turnId: this.activeTurnId ?? undefined,
    });

    return new Promise<boolean>((resolve) => {
      const timer =
        this.approvalTimeoutMs > 0
          ? setTimeout(() => this.resolveApproval(approvalId, "timeout"), this.approvalTimeoutMs)
          : null;
      // Don't keep the event loop alive on the approval timer alone.
      timer?.unref?.();
      this.pendingApprovals.set(approvalId, {
        resolve: (decision) => resolve(decision === "approved"),
        timer,
      });
    });
  }

  /** Resolve an outstanding approval (from an `approval:decide` command or timeout). */
  resolveApproval(approvalId: string, decision: ConfirmationDecision): void {
    const pending = this.pendingApprovals.get(approvalId);
    if (!pending) return;
    this.pendingApprovals.delete(approvalId);
    if (pending.timer) clearTimeout(pending.timer);
    this.post({
      type: "approval:decided",
      sessionId: this.sessionId,
      approvalId,
      decision,
      decidedAt: this.now(),
    });
    pending.resolve(decision);
  }

  /** Settle every outstanding approval (shutdown / hibernate drain). */
  settlePendingApprovals(decision: ConfirmationDecision = "rejected"): void {
    for (const approvalId of [...this.pendingApprovals.keys()]) {
      this.resolveApproval(approvalId, decision);
    }
  }

  private closeTurn(outcome: TurnOutcomeClass): void {
    const turnId = this.activeTurnId;
    if (!turnId) return;
    this.activeTurnId = null;
    this.post({ type: "turn:end", sessionId: this.sessionId, turnId, endedAt: this.now(), outcome });
  }

  private isDanger(toolName: string): boolean {
    const risk = this.riskOf(toolName);
    return risk !== undefined && risk !== "read";
  }

  private genId(prefix: string): string {
    return `${prefix}_${randomUUID().slice(0, 8)}`;
  }
}

const ERROR_CODE_TO_RESULT: Record<string, AuditResult> = {
  CONFIRMATION_REQUIRED: "confirmation-pending",
  UNAUTHORIZED: "unauthorized",
  TIER_REJECTED: "unauthorized",
  FORBIDDEN: "unauthorized",
  RATE_LIMITED: "rate_limited",
  DEDUP: "dedup",
  DUPLICATE: "dedup",
  COLLISION: "collision",
};

export function resultToAudit(res: ToolResult): {
  result: AuditResult;
  severity: AuditSeverity;
  errorCode?: string;
} {
  if (res.ok) return { result: "success", severity: severityForResult("success") };
  const code = res.error?.code;
  const result = (code && ERROR_CODE_TO_RESULT[code]) || "error";
  return { result, severity: severityForResult(result), errorCode: code };
}

/**
 * Single-level, redacted JSON view of tool args for the timeline. Long strings
 * collapse to `<string: N chars>` and nested objects/arrays to `<object>` /
 * `<array>` — raw arg values may carry file content, terminal output, or prompt
 * text and must never cross the boundary verbatim. Mirrors the redaction
 * Daintree's MCP audit applies to `argsSummary`.
 */
export function redactArgs(args: unknown): string {
  if (args === null || args === undefined) return "";
  if (typeof args === "string") {
    return args.length > ARGS_SUMMARY_MAX_STRING ? `<string: ${args.length} chars>` : JSON.stringify(args);
  }
  if (typeof args !== "object") return JSON.stringify(args);
  const redactValue = (v: unknown): unknown => {
    if (typeof v === "string") return v.length > ARGS_SUMMARY_MAX_STRING ? `<string: ${v.length} chars>` : v;
    if (Array.isArray(v)) return "<array>";
    if (v !== null && typeof v === "object") return "<object>";
    return v;
  };
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(args as Record<string, unknown>)) out[k] = redactValue(v);
  try {
    return JSON.stringify(out);
  } catch {
    return "<unserializable>";
  }
}
