/**
 * The UI bridge decouples the runtime from Ink. The runtime emits structured
 * events here (via an AgentEventSink and the confirm hook); the Ink controller
 * subscribes and turns them into React state. Nothing in this file imports Ink,
 * and nothing writes to stdout — that's what keeps the frame from being corrupted.
 */
import { EventEmitter } from "node:events";
import type { AgentEventSink } from "../agent/events.js";
import type { ConfirmRequest } from "../tools/types.js";
import type { ToolResult } from "../schemas.js";
import type { PendingConfirm } from "./types.js";

export type UiBridgeEvent =
  | { type: "assistant:start" }
  | { type: "assistant:token"; token: string }
  | { type: "assistant:end"; content: string }
  | { type: "assistant:cancelled"; content: string }
  | { type: "tool:call"; id: string; name: string; args: unknown; startedAt: number }
  | { type: "tool:result"; id: string; name: string; result: ToolResult; endedAt: number }
  | { type: "log"; level: "info" | "warn" | "error"; message: string }
  | { type: "confirm"; pending: PendingConfirm }
  | { type: "attention"; events: unknown[] };

let confirmCounter = 0;

export class UiBridge {
  private emitter = new EventEmitter();
  /** Resolvers for confirms awaiting a UI answer, so we can settle them on teardown. */
  private pendingConfirms = new Set<(approved: boolean) => void>();

  constructor() {
    // The dashboard poll + multiple panels can subscribe; lift the default cap.
    this.emitter.setMaxListeners(50);
  }

  emit(event: UiBridgeEvent): void {
    this.emitter.emit("event", event);
  }

  subscribe(fn: (event: UiBridgeEvent) => void): () => void {
    this.emitter.on("event", fn);
    return () => this.emitter.off("event", fn);
  }

  /** An AgentEventSink that republishes the agent loop onto the bridge. */
  agentEvents(): AgentEventSink {
    return {
      assistantStart: () => this.emit({ type: "assistant:start" }),
      assistantToken: (token) => this.emit({ type: "assistant:token", token }),
      assistantEnd: (content) => this.emit({ type: "assistant:end", content }),
      assistantCancelled: (content) =>
        this.emit({ type: "assistant:cancelled", content }),
      toolCall: ({ id, name, args, startedAt }) =>
        this.emit({ type: "tool:call", id, name, args, startedAt }),
      toolResult: ({ id, name, result, endedAt }) =>
        this.emit({ type: "tool:result", id, name, result, endedAt }),
      error: (message) => this.emit({ type: "log", level: "error", message }),
      info: (message) => this.emit({ type: "log", level: "info", message }),
    };
  }

  /** Surface a confirmation request to the UI; resolves when the user answers. */
  requestConfirm(request: ConfirmRequest): Promise<boolean> {
    return new Promise((resolve) => {
      // Wrap so the promise resolves at most once and removes itself from the
      // pending set — guards double-answers and lets teardown settle it.
      const settle = (approved: boolean) => {
        if (this.pendingConfirms.delete(settle)) resolve(approved);
      };
      this.pendingConfirms.add(settle);
      const pending: PendingConfirm = {
        id: `cfm_${(confirmCounter++).toString(36)}_${Date.now().toString(36)}`,
        request,
        resolve: settle,
      };
      this.emit({ type: "confirm", pending });
    });
  }

  /**
   * Settle every outstanding confirm (default: decline). Called on UI teardown so
   * a `ToolRegistry.dispatch` blocked on `await ctx.confirm()` can't hang the
   * process during shutdown.
   */
  settlePendingConfirms(approved = false): void {
    for (const settle of [...this.pendingConfirms]) settle(approved);
  }
}
