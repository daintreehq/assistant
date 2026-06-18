/**
 * Structured event sink for the main-thread agent loop. The loop emits these
 * instead of writing to stdout directly, so either the legacy console renderer or
 * the Ink UI can present the same run. This keeps the runtime free of any
 * terminal-rendering dependency.
 */
import type { ToolResult } from "../schemas.js";

/** The model requested a tool call (args already parsed, or raw on parse failure). */
export interface ToolCallEvent {
  /** The model's tool-call id — stable across the call/result pair. */
  id: string;
  name: string;
  args: unknown;
  startedAt: number;
}

/** A tool call completed. */
export interface ToolResultEvent {
  /** Matches the originating {@link ToolCallEvent.id}. */
  id: string;
  name: string;
  result: ToolResult;
  endedAt: number;
}

export interface AgentEventSink {
  /** A new assistant turn is about to stream. */
  assistantStart(): void;
  /** A token of streamed assistant text. */
  assistantToken(token: string): void;
  /** The assistant turn finished with this final (think-stripped) content. */
  assistantEnd(content: string): void;
  /** The model requested a tool call. Carries the call id so results match by id. */
  toolCall(event: ToolCallEvent): void;
  /** A tool call completed. Resolve the activity by {@link ToolResultEvent.id}. */
  toolResult(event: ToolResultEvent): void;
  /** A fatal-for-this-turn error message. */
  error(message: string): void;
  /** An informational note from the loop. */
  info(message: string): void;
}

export const noopAgentEvents: AgentEventSink = {
  assistantStart() {},
  assistantToken() {},
  assistantEnd() {},
  toolCall() {},
  toolResult() {},
  error() {},
  info() {},
};
