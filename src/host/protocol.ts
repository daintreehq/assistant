/**
 * Native assistant-host protocol — the wire contract this package speaks to
 * Daintree when it is launched as an Electron `utilityProcess.fork()` child
 * (Daintree issue #10649).
 *
 * This is a hand-mirror of Daintree's source-of-truth contract at
 * `shared/types/ipc/assistantHost.ts` (+ the audit vocabularies in
 * `shared/types/ipc/mcpServer.ts`). The two repos have no shared package, so
 * the shapes are duplicated and kept in lockstep by {@link PROTOCOL_VERSION}:
 * Daintree validates every inbound message with Zod and refuses a version it
 * does not recognise, so a drift surfaces immediately at the boundary rather
 * than silently corrupting the timeline. Bump this in lockstep with the
 * Daintree constant on any breaking change.
 */

/** Wire-format version. Must equal Daintree's `ASSISTANT_HOST_PROTOCOL_VERSION`. */
export const PROTOCOL_VERSION = 1;

// Audit-aligned vocabularies — mirror of `mcpServer.ts`.
export type AuditResult =
  | "success"
  | "error"
  | "confirmation-pending"
  | "unauthorized"
  | "dedup"
  | "collision"
  | "rate_limited";

export type AuditSeverity = "info" | "notice" | "warning" | "error" | "critical";

export type ConfirmationDecision = "approved" | "rejected" | "timeout";

export type TurnOutcomeClass =
  | "answered"
  | "hedged"
  | "refused"
  | "docs-empty"
  | "tier-rejected"
  | "mcp-not-ready"
  | "agent-stuck"
  | "tool-error"
  | "reasoning-loop"
  | "hibernate-resume-stale"
  | "cancelled"
  | "unknown";

export type TurnRole = "user" | "assistant";

/**
 * Non-secret descriptor Daintree hands the host once, as the first `parentPort`
 * message. Carries no bearer token or MCP URL — those arrive via env
 * (`DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` / `DAINTREE_WINDOW_ID`), so a
 * leaked port message can never carry the secret.
 */
export interface SessionDescriptor {
  sessionId: string;
  windowId: number;
  projectId: string;
  cwd: string;
  tier: string;
  protocolVersion: number;
  resumeSessionId?: string;
}

// ---------------------------------------------------------------------------
// Host → Daintree events
// ---------------------------------------------------------------------------

export interface HostReadyEvent {
  type: "host:ready";
  sessionId: string;
  protocolVersion: number;
  resumedSessionId?: string;
}

export interface TurnStartEvent {
  type: "turn:start";
  sessionId: string;
  turnId: string;
  role: TurnRole;
  startedAt: number;
}

export interface TurnTokenEvent {
  type: "turn:token";
  sessionId: string;
  turnId: string;
  chunk: string;
}

export interface TurnEndEvent {
  type: "turn:end";
  sessionId: string;
  turnId: string;
  endedAt: number;
  outcome?: TurnOutcomeClass;
}

export interface ToolStartedEvent {
  type: "tool:started";
  sessionId: string;
  toolCallId: string;
  toolId: string;
  argsSummary: string;
  startedAt: number;
  turnId?: string;
  danger: boolean;
}

export interface ToolSettledEvent {
  type: "tool:settled";
  sessionId: string;
  toolCallId: string;
  toolId: string;
  durationMs: number;
  result: AuditResult;
  severity: AuditSeverity;
  errorCode?: string;
  turnId?: string;
}

export interface ApprovalRequestedEvent {
  type: "approval:requested";
  sessionId: string;
  approvalId: string;
  toolId: string;
  summary: string;
  requestedAt: number;
  turnId?: string;
}

export interface ApprovalDecidedEvent {
  type: "approval:decided";
  sessionId: string;
  approvalId: string;
  decision: ConfirmationDecision;
  decidedAt: number;
}

export interface HostErrorEvent {
  type: "host:error";
  sessionId: string;
  code: string;
  message: string;
}

export type HostShutdownReason = "hibernate" | "revoke" | "error" | "exit";

export interface HostShutdownEvent {
  type: "host:shutdown";
  sessionId: string;
  reason: HostShutdownReason;
  resumeSessionId?: string;
}

export type HostEvent =
  | HostReadyEvent
  | TurnStartEvent
  | TurnTokenEvent
  | TurnEndEvent
  | ToolStartedEvent
  | ToolSettledEvent
  | ApprovalRequestedEvent
  | ApprovalDecidedEvent
  | HostErrorEvent
  | HostShutdownEvent;

// ---------------------------------------------------------------------------
// Daintree → host commands
// ---------------------------------------------------------------------------

export interface PromptCommand {
  type: "prompt";
  sessionId: string;
  text: string;
}

export interface ApprovalDecideCommand {
  type: "approval:decide";
  sessionId: string;
  approvalId: string;
  decision: ConfirmationDecision;
}

export interface InterruptCommand {
  type: "interrupt";
  sessionId: string;
}

export interface HibernateCommand {
  type: "hibernate";
  sessionId: string;
}

export interface ShutdownCommand {
  type: "shutdown";
  sessionId: string;
}

export type HostCommand =
  | PromptCommand
  | ApprovalDecideCommand
  | InterruptCommand
  | HibernateCommand
  | ShutdownCommand;

export type HostCommandType = HostCommand["type"];

/** Severity for an audit result — mirror of `SEVERITY_BY_RESULT` in `mcpServer.ts`. */
const SEVERITY_BY_RESULT: Record<AuditResult, AuditSeverity> = {
  success: "info",
  dedup: "info",
  "confirmation-pending": "notice",
  unauthorized: "warning",
  rate_limited: "warning",
  collision: "warning",
  error: "error",
};

export function severityForResult(result: AuditResult): AuditSeverity {
  return SEVERITY_BY_RESULT[result];
}

/** Narrow an unknown `parentPort` payload to a {@link SessionDescriptor}. */
export function isSessionDescriptor(value: unknown): value is SessionDescriptor {
  if (typeof value !== "object" || value === null) return false;
  const d = value as Record<string, unknown>;
  return (
    typeof d.sessionId === "string" &&
    typeof d.windowId === "number" &&
    typeof d.projectId === "string" &&
    typeof d.cwd === "string" &&
    typeof d.tier === "string" &&
    typeof d.protocolVersion === "number"
  );
}

/** Narrow an unknown `parentPort` payload to a {@link HostCommand}. */
export function isHostCommand(value: unknown): value is HostCommand {
  if (typeof value !== "object" || value === null) return false;
  const c = value as Record<string, unknown>;
  if (typeof c.sessionId !== "string") return false;
  switch (c.type) {
    case "prompt":
      return typeof c.text === "string";
    case "approval:decide":
      return typeof c.approvalId === "string" && typeof c.decision === "string";
    case "interrupt":
    case "hibernate":
    case "shutdown":
      return true;
    default:
      return false;
  }
}
