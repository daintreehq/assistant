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

/**
 * Token usage, cost, and context pressure for one completed main-thread model
 * call. The cockpit accumulates these into a live session rollup (tokens + cost)
 * and a context-pressure gauge; nothing in the runtime depends on them.
 */
export interface AgentUsageEvent {
  /** Prompt (input) tokens billed for this call. */
  promptTokens: number;
  /** Completion (output) tokens billed for this call. */
  completionTokens: number;
  /** Total tokens as reported by the provider (prompt + completion). */
  totalTokens: number;
  /** Cached prompt tokens (billed at a discount), when the provider reports them. */
  cachedTokens?: number;
  /** Estimated tokens in the conversation now — the context-pressure numerator. */
  contextTokens: number;
  /** Auto-compact threshold — the context-pressure denominator. */
  contextThreshold: number;
  /** Estimated USD cost of this call, or undefined when the model has no rate. */
  costUsd: number | undefined;
  /** Routed tier for the call, e.g. "large". */
  tier: string;
  /** Concrete model id the tier resolved to. */
  model: string;
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
  /**
   * Token usage / cost / context pressure for a completed main-thread model call.
   * Optional: most sinks ignore it, and making it required would force a no-op
   * onto every existing adapter. Emitted once per streamed round, after the model
   * call returns and before the assistant message is appended.
   */
  usage?(event: AgentUsageEvent): void;
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
  usage() {},
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
  // The optional `usage` method is fanned out separately (below); restrict the
  // generic helper to the required methods so `Parameters<…>` stays a function type.
  type RequiredSinkMethod = Exclude<keyof AgentEventSink, "usage">;
  const fan =
    <K extends RequiredSinkMethod>(method: K) =>
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
    assistantCancelled: fan("assistantCancelled"),
    toolCall: fan("toolCall"),
    toolResult: fan("toolResult"),
    error: fan("error"),
    info: fan("info"),
    // `usage` is optional on the interface, so a sink may not implement it —
    // guard each call rather than blindly invoking it.
    usage: (event: AgentUsageEvent) => {
      for (const sink of sinks) {
        try {
          sink.usage?.(event);
        } catch {
          /* one sink's failure must not affect the others */
        }
      }
    },
  };
}

/**
 * Persists the agent event stream to the append-only `run_events` table, giving
 * each run (one `AgentSession.send()` turn) a typed, ordered log that can be
 * replayed after the fact. The current run id comes from the shared {@link RunIdRef}.
 *
 * Individual streamed tokens are not written one-per-row — that would add hundreds
 * of rows per turn. Instead they are buffered and flushed as a single
 * `assistant:content` row when the round ends (a tool call begins, an error fires,
 * or a new round starts). This captures *intermediate* assistant prose — text the
 * model emits in the same round as a tool call — which never reaches
 * `assistantEnd` (that only fires on the final, tool-free round) and would
 * otherwise be lost from the log. The final round's content still arrives via
 * `assistantEnd` as `assistant:end`; the streamed buffer for that round is dropped
 * to avoid duplicating it.
 *
 * The `seq` counter is monotonic within a run and resets whenever the ref points
 * at a new run id. All writes are best-effort: a DB failure is swallowed so it can
 * never break the live turn.
 */
export class RunEventSink implements AgentEventSink {
  private seq = 0;
  private seqRunId: string | undefined = undefined;
  /** Streamed tokens for the current round, flushed as one `assistant:content` row. */
  private contentBuffer = "";

  constructor(
    private readonly db: Db,
    private readonly ref: RunIdRef,
  ) {}

  assistantStart(): void {
    // Defensive: a round normally flushes via toolCall/assistantEnd before the
    // next start, but if any buffered prose remains, record it rather than lose it.
    this.flushContent();
    this.write("assistant:start");
  }

  assistantToken(token: string): void {
    this.contentBuffer += token;
  }

  assistantEnd(content: string): void {
    // `content` is authoritative for this (final) round, so drop the streamed
    // buffer instead of flushing it as a duplicate assistant:content row.
    this.contentBuffer = "";
    this.write("assistant:end", { content });
  }

  assistantCancelled(content: string): void {
    // A user abort ends the run mid-flight. `content` is authoritative for whatever
    // was streamed before the cancel, so drop the buffer (same reasoning as
    // assistantEnd) and record the cancellation so the replayed log shows the run
    // stopped on purpose rather than simply trailing off.
    this.contentBuffer = "";
    this.write("assistant:cancelled", { content });
  }

  toolCall(event: ToolCallEvent): void {
    // Any prose streamed in this round precedes the tool call — record it first.
    this.flushContent();
    this.write("tool:call", { id: event.id, name: event.name, args: event.args });
  }

  toolResult(event: ToolResultEvent): void {
    this.write("tool:result", {
      id: event.id,
      name: event.name,
      ok: event.result.ok,
      summary: event.result.summary,
      // The audit row id is the precise cross-reference into audit_log; it is set
      // on the result by the registry before this event fires (absent if the call
      // never reached dispatch, e.g. a refused or unparsable call).
      auditId: event.result.auditId,
    });
  }

  error(message: string): void {
    // Preserve any partial prose streamed before the failure.
    this.flushContent();
    this.write("error", { message });
  }

  info(message: string): void {
    this.write("info", { message });
  }

  usage(event: AgentUsageEvent): void {
    // Any prose streamed this round precedes the usage row — flush it first so the
    // replayed log keeps content/usage in the order they actually occurred.
    this.flushContent();
    // Token accounting for this round; persisted so a replayed log carries the
    // same cost/context-pressure signal the live cockpit showed.
    this.write("usage", {
      promptTokens: event.promptTokens,
      completionTokens: event.completionTokens,
      totalTokens: event.totalTokens,
      cachedTokens: event.cachedTokens,
      contextTokens: event.contextTokens,
      contextThreshold: event.contextThreshold,
      costUsd: event.costUsd,
      tier: event.tier,
      model: event.model,
    });
  }

  /** Write buffered round prose as one row, if any, then clear the buffer. */
  private flushContent(): void {
    if (this.contentBuffer.length === 0) return;
    const content = this.contentBuffer;
    this.contentBuffer = "";
    this.write("assistant:content", { content });
  }

  private write(type: string, payload?: unknown): void {
    const runId = this.ref.current;
    if (!runId) return; // emitted outside a run — nothing to scope it to
    // A new run resets the monotonic seq counter (and any stale buffered prose).
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

/** Largest serialized run-event payload we persist, mirroring the audit-log cap. */
const MAX_RUN_EVENT_PAYLOAD = 8000;

/**
 * Serialize a payload defensively: never throw on a cyclic/unserializable value,
 * and bound the size so a large tool result or long assistant turn can't write an
 * unbounded BLOB on the live turn's synchronous path. Oversized payloads are
 * replaced with a still-valid-JSON truncation marker carrying a preview.
 */
function serializePayload(payload: unknown): string {
  let json: string;
  try {
    json = JSON.stringify(payload) ?? "null";
  } catch {
    return JSON.stringify({ error: "unserializable" });
  }
  if (json.length <= MAX_RUN_EVENT_PAYLOAD) return json;
  return JSON.stringify({
    truncated: true,
    bytes: Buffer.byteLength(json, "utf8"),
    // Leave headroom for the wrapper + JSON escaping so the row stays near the cap.
    preview: json.slice(0, MAX_RUN_EVENT_PAYLOAD - 200),
  });
}
