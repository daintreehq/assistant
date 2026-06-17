/**
 * UI-facing types. The Ink layer holds ephemeral presentation state (timeline,
 * pending confirm) while the runtime keeps durable operational state in SQLite.
 */
import type { ConfirmRequest } from "../tools/types.js";
import type {
  AuditRecord,
  QueueEvent,
  TimerRecord,
  WatcherRecord,
} from "../schemas.js";
import type { McpStatus } from "../mcp/client.js";

export type TimelineItem =
  | { id: string; kind: "user"; text: string; ts: number }
  | {
      id: string;
      kind: "assistant";
      text: string;
      streaming?: boolean;
      ts: number;
    }
  | {
      id: string;
      kind: "tool";
      name: string;
      args?: unknown;
      ok?: boolean;
      summary?: string;
      ts: number;
    }
  | {
      id: string;
      kind: "system";
      level: "info" | "warn" | "error";
      text: string;
      ts: number;
    }
  | { id: string; kind: "command"; title: string; text: string; ts: number };

export interface PendingConfirm {
  id: string;
  request: ConfirmRequest;
  resolve: (approved: boolean) => void;
}

export interface DashboardState {
  mcp: McpStatus;
  watchers: WatcherRecord[];
  timers: TimerRecord[];
  inbox: QueueEvent[];
  audit: AuditRecord[];
}
