/**
 * Structured JSONL renderer for one-shot mode, expressed as an AgentEventSink so
 * it slots in wherever the legacy console sink does. It writes one JSON object
 * per line to stdout as the run streams, then a final `result` envelope summarising
 * the outcome (see the schemas in src/schemas.ts). This is the scriptable/CI
 * surface: the interactive Ink UI and the human console renderer are untouched.
 *
 * The event-buffering mirrors {@link RunEventSink} exactly: streamed tokens are
 * accumulated and flushed as a single `assistant:content` line when a round ends
 * (a tool call begins, an error fires, or a new round starts), so *intermediate*
 * prose — text the model emits in the same round as a tool call, which never
 * reaches `assistantEnd` — is captured rather than lost. The final round's content
 * arrives via `assistantEnd` as `assistant:end`, and that round's streamed buffer
 * is dropped to avoid duplicating it. Keeping the two sinks consistent means the
 * live JSONL stream and the durable DB log describe a run identically.
 */
import type { AgentEventSink } from "../agent/events.js";
import {
  JSON_OUTPUT_SCHEMA_VERSION,
  ONE_SHOT_EXIT_CODE,
  type JsonOutputStatus,
} from "../schemas.js";

export interface JsonSinkHandle {
  /** The sink to hand to the agent loop / App.setHooks. */
  sink: AgentEventSink;
  /**
   * Emit the terminal `result` envelope and return the process exit code. Safe to
   * call more than once — only the first call writes (the loop's normal path calls
   * it once after `send()` resolves; a caller may also call it from a catch).
   */
  finish(): { exitCode: number };
}

export interface JsonSinkOptions {
  /** Sink for each JSONL line (includes the trailing newline). Defaults to stdout. */
  write?: (line: string) => void;
  /** Clock for the `ts` field. Injected so tests get a deterministic stamp. */
  now?: () => number;
}

/**
 * Build a JSONL sink plus its `finish()` terminator. State is closed over rather
 * than held on a class — same shape as `createConsoleSink()`. `send()` is
 * single-flight, so a single shared terminal-status slot is sufficient.
 */
export function createJsonSink(opts: JsonSinkOptions = {}): JsonSinkHandle {
  const write = opts.write ?? ((line: string) => void process.stdout.write(line));
  const now = opts.now ?? Date.now;

  let seq = 0;
  /** Tokens streamed in the current round, flushed as one `assistant:content` line. */
  let contentBuffer = "";
  /** Final assistant text, set by assistant:end / assistant:cancelled. */
  let content = "";
  // Default to error: if a turn somehow ends without any terminal event, that is
  // itself a failure, and exiting non-zero is the safe signal for a script.
  let status: JsonOutputStatus = "error";
  let exitCode: number = ONE_SHOT_EXIT_CODE.error;
  let errorMessage: string | null = null;
  let finished = false;

  function emit(type: string, payload?: Record<string, unknown>): void {
    // Nothing may follow the terminal `result` envelope: once finished, drop any
    // late event (e.g. an info emitted after shutdown) so the stream stays well-formed.
    if (finished) return;
    const line = { type, ts: now(), seq: seq++, ...(payload ?? {}) };
    let serialized: string;
    try {
      serialized = JSON.stringify(line);
    } catch {
      // A payload field (notably a ToolError's `details: unknown`) can be circular
      // or otherwise unserializable. Never drop the line — that would leave a seq
      // gap; emit a valid degraded line keeping the envelope fields intact.
      serialized = JSON.stringify({ type, ts: line.ts, seq: line.seq, serializationError: true });
    }
    write(serialized + "\n");
  }

  /** Write buffered round prose as one line, if any, then clear the buffer. */
  function flushContent(): void {
    if (contentBuffer.length === 0) return;
    const buffered = contentBuffer;
    contentBuffer = "";
    emit("assistant:content", { content: buffered });
  }

  const sink: AgentEventSink = {
    assistantStart() {
      // Defensive: rounds normally flush via toolCall/assistantEnd, but record any
      // leftover prose rather than lose it (matches RunEventSink).
      flushContent();
      emit("assistant:start");
    },
    assistantToken(token) {
      contentBuffer += token;
    },
    assistantEnd(text) {
      // `text` is authoritative for this final round — drop the streamed buffer
      // instead of flushing it as a duplicate assistant:content line.
      contentBuffer = "";
      content = text;
      status = "success";
      exitCode = ONE_SHOT_EXIT_CODE.success;
      errorMessage = null;
      emit("assistant:end", { content: text });
    },
    assistantCancelled(text) {
      contentBuffer = "";
      content = text;
      status = "cancelled";
      exitCode = ONE_SHOT_EXIT_CODE.cancelled;
      emit("assistant:cancelled", { content: text });
    },
    toolCall(event) {
      // Any prose streamed in this round precedes the tool call — record it first.
      flushContent();
      emit("tool:call", { id: event.id, name: event.name, args: event.args });
    },
    toolResult(event) {
      emit("tool:result", {
        id: event.id,
        name: event.name,
        ok: event.result.ok,
        summary: event.result.summary,
        // Surface the structured tool error so a consumer can react to *why* a
        // call failed without re-parsing the summary. Null on success.
        error: event.result.error ?? null,
        // Cross-reference into audit_log; absent if the call never reached dispatch.
        auditId: event.result.auditId,
      });
    },
    error(message) {
      // Preserve any partial prose streamed before the failure.
      flushContent();
      status = "error";
      exitCode = ONE_SHOT_EXIT_CODE.error;
      errorMessage = message;
      emit("error", { message });
    },
    info(message) {
      emit("info", { message });
    },
  };

  function finish(): { exitCode: number } {
    if (finished) return { exitCode };
    // Flush + emit the envelope while `finished` is still false (emit() drops lines
    // once it flips). A turn may end with prose still buffered if no terminal event
    // flushed it (e.g. a degenerate stream) — don't lose it.
    flushContent();
    emit("result", {
      schemaVersion: JSON_OUTPUT_SCHEMA_VERSION,
      status,
      exitCode,
      content,
      error: errorMessage === null ? null : { message: errorMessage },
    });
    finished = true;
    return { exitCode };
  }

  return { sink, finish };
}
