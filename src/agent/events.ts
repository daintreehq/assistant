/**
 * Structured event sink for the main-thread agent loop. The loop emits these
 * instead of writing to stdout directly, so either the legacy console renderer or
 * the Ink UI can present the same run. This keeps the runtime free of any
 * terminal-rendering dependency.
 */
import type { ToolResult } from "../schemas.js";
import type { Db } from "../storage/db.js";

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
  /**
   * The assistant turn was cancelled by the user mid-flight. `content` is whatever
   * was streamed before the abort (often empty); the UI keeps the partial text but
   * stops the caret and marks the turn cancelled rather than failed.
   */
  assistantCancelled(content: string): void;
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
  assistantCancelled() {},
  toolCall() {},
  toolResult() {},
  error() {},
  info() {},
};

/**
 * A mutable holder for the id of the run currently streaming. `AgentSession.send()`
 * sets `current` at the top of each turn and clears it when the turn ends, so the
 * durable {@link RunEventSink} can stamp the right run id without the session
 * needing to know that sink exists.
 *
 * This is a plain object, not `AsyncLocalStorage`, on purpose: the architecture is
 * single-flight (one Ink app drives one `AgentSession`, and `send()` is not called
 * re-entrantly), so a single shared slot is sufficient. If concurrent `send()`
 * calls are ever introduced, this ref must become per-call (or async-local).
 */
export type RunIdRef = { current: string | undefined };

/**
 * Fan a single event stream out to several sinks, isolating each so one sink's
 * failure can never starve the others. Mirrors the best-effort dispatch already
 * used elsewhere in the loop — a durable sink hitting a disk error must not break
 * the live UI sink, and vice versa.
 */
export function multiSink(...sinks: AgentEventSink[]): AgentEventSink {
  const fan =
    <K extends keyof AgentEventSink>(method: K) =>
    (...args: Parameters<AgentEventSink[K]>): void => {
      for (const sink of sinks) {
        try {
          (sink[method] as (...a: unknown[]) => void)(...args);
        } catch {
          /* one sink's failure must not affect the others */
        }
      }
    };
  return {
    assistantStart: fan("assistantStart"),
    assistantToken: fan("assistantToken"),
    assistantEnd: fan("assistantEnd"),
    toolCall: fan("toolCall"),
    toolResult: fan("toolResult"),
    error: fan("error"),
    info: fan("info"),
  };
}

/**
 * Persists the agent event stream to the append-only `run_events` table, giving
 * each run (one `AgentSession.send()` turn) a typed, ordered log that can be
 * replayed after the fact. The current run id comes from the shared {@link RunIdRef}.
 *
 * Token-level events are deliberately not persisted — `assistant:end` already
 * carries the full think-stripped content, so logging every streamed token would
 * add hundreds of rows per turn with no extra replay fidelity.
 *
 * The `seq` counter is monotonic within a run and resets whenever the ref points
 * at a new run id. All writes are best-effort: a DB failure is swallowed so it can
 * never break the live turn.
 */
export class RunEventSink implements AgentEventSink {
  private seq = 0;
  private seqRunId: string | undefined = undefined;

  constructor(
    private readonly db: Db,
    private readonly ref: RunIdRef,
  ) {}

  assistantStart(): void {
    this.write("assistant:start");
  }

  assistantToken(): void {
    /* not persisted — assistant:end carries the full content (see class doc) */
  }

  assistantEnd(content: string): void {
    this.write("assistant:end", { content });
  }

  toolCall(event: ToolCallEvent): void {
    this.write("tool:call", { id: event.id, name: event.name, args: event.args });
  }

  toolResult(event: ToolResultEvent): void {
    this.write("tool:result", {
      id: event.id,
      name: event.name,
      ok: event.result.ok,
      summary: event.result.summary,
    });
  }

  error(message: string): void {
    this.write("error", { message });
  }

  info(message: string): void {
    this.write("info", { message });
  }

  private write(type: string, payload?: unknown): void {
    const runId = this.ref.current;
    if (!runId) return; // emitted outside a run — nothing to scope it to
    // A new run resets the monotonic seq counter.
    if (runId !== this.seqRunId) {
      this.seqRunId = runId;
      this.seq = 0;
    }
    try {
      this.db.insertRunEvent({
        runId,
        seq: this.seq++,
        type,
        payload: payload === undefined ? undefined : serializePayload(payload),
      });
    } catch {
      /* durable logging must never break a live turn */
    }
  }
}

/** Serialize a payload defensively — never throw on a cyclic/unserializable value. */
function serializePayload(payload: unknown): string {
  try {
    return JSON.stringify(payload) ?? "null";
  } catch {
    return JSON.stringify({ error: "unserializable" });
  }
}
