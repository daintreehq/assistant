import { createJsonSink } from "../src/cli/jsonSink.js";
import {
  JsonlEventSchema,
  JsonResultEnvelopeSchema,
  JSON_OUTPUT_SCHEMA_VERSION,
} from "../src/schemas.js";
import type { ToolResult } from "../src/schemas.js";

// Unit tests for the one-shot JSONL sink. The sink takes injected `write` and
// `now` so the output is fully deterministic — we collect every line and assert
// on the parsed JSON, the terminal `result` envelope, and the exit code, without
// touching real stdout or the wall clock.
//
// NOTE: an end-to-end CLI smoke test (spawning `node dist/index.js --json …`) is
// intentionally omitted — the suite runs with no network and a faked model layer,
// so a subprocess test would need to stub the whole model stack at the shell
// level. The sink contract here plus the existing loop/event tests cover it.

function makeSink() {
  const lines: string[] = [];
  let clock = 1000;
  const handle = createJsonSink({
    write: (line) => lines.push(line),
    // Monotonic, deterministic clock — each call advances by 1ms.
    now: () => clock++,
  });
  const parsed = () => lines.map((l) => JSON.parse(l));
  return { ...handle, lines, parsed };
}

function toolResult(over: Partial<ToolResult> = {}): ToolResult {
  return { ok: true, summary: "ok", ...over };
}

describe("createJsonSink", () => {
  it("emits start, end, and a success result envelope for a clean turn", () => {
    const { sink, finish, lines, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantToken("hel");
    sink.assistantToken("lo");
    sink.assistantEnd("hello");
    const { exitCode } = finish();

    // Every line is valid JSON ending in a newline.
    for (const l of lines) expect(l.endsWith("\n")).toBe(true);
    const events = parsed();
    expect(events.map((e) => e.type)).toEqual([
      "assistant:start",
      "assistant:end",
      "result",
    ]);
    // Streamed tokens of the final round are dropped (no assistant:content line);
    // the authoritative text rides on assistant:end.
    expect(events[1].content).toBe("hello");

    const envelope = JsonResultEnvelopeSchema.parse(events[2]);
    expect(envelope.status).toBe("success");
    expect(envelope.exitCode).toBe(0);
    expect(envelope.content).toBe("hello");
    expect(envelope.error).toBeNull();
    expect(envelope.schemaVersion).toBe(JSON_OUTPUT_SCHEMA_VERSION);
    expect(exitCode).toBe(0);
  });

  it("flushes intermediate prose as assistant:content before a tool call", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantToken("let me check");
    sink.toolCall({ id: "c1", name: "fs.search", args: { q: "x" }, startedAt: 0 });
    sink.toolResult({ id: "c1", name: "fs.search", result: toolResult({ summary: "2 files" }), endedAt: 1 });
    sink.assistantStart();
    sink.assistantEnd("done");
    finish();

    const events = parsed();
    expect(events.map((e) => e.type)).toEqual([
      "assistant:start",
      "assistant:content",
      "tool:call",
      "tool:result",
      "assistant:start",
      "assistant:end",
      "result",
    ]);
    // The round's streamed prose was captured (not lost) ahead of the tool call.
    expect(events[1].content).toBe("let me check");
    const call = events[2];
    expect(call.name).toBe("fs.search");
    expect(call.args).toEqual({ q: "x" });
    const result = events[3];
    expect(result.ok).toBe(true);
    expect(result.summary).toBe("2 files");
    expect(result.error).toBeNull();
  });

  it("surfaces a structured tool error on the tool:result line but still exits 0", () => {
    // Failed tool calls are recoverable context for the model; the loop continues
    // and the turn ends normally, so the final exit reflects assistant:end (0).
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.toolCall({ id: "c1", name: "git.push", args: {}, startedAt: 0 });
    sink.toolResult({
      id: "c1",
      name: "git.push",
      result: toolResult({
        ok: false,
        summary: "push rejected",
        error: { code: "git_rejected", message: "non-fast-forward", recoverable: true },
      }),
      endedAt: 1,
    });
    sink.assistantStart();
    sink.assistantEnd("could not push, will retry");
    const { exitCode } = finish();

    const events = parsed();
    const result = events.find((e) => e.type === "tool:result");
    expect(result.ok).toBe(false);
    expect(result.error).toEqual({ code: "git_rejected", message: "non-fast-forward", recoverable: true });
    const envelope = JsonResultEnvelopeSchema.parse(events.at(-1));
    expect(envelope.status).toBe("success");
    expect(exitCode).toBe(0);
  });

  it("maps an error event to status=error and exit 1", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.error("Model error: boom");
    const { exitCode } = finish();

    const events = parsed();
    expect(events.map((e) => e.type)).toEqual(["assistant:start", "error", "result"]);
    const envelope = JsonResultEnvelopeSchema.parse(events.at(-1));
    expect(envelope.status).toBe("error");
    expect(envelope.exitCode).toBe(1);
    expect(envelope.error).toEqual({ message: "Model error: boom" });
    expect(exitCode).toBe(1);
  });

  it("flushes buffered prose before an error event", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantToken("partial thought");
    sink.error("stream died");
    finish();

    const events = parsed();
    expect(events.map((e) => e.type)).toEqual([
      "assistant:start",
      "assistant:content",
      "error",
      "result",
    ]);
    expect(events[1].content).toBe("partial thought");
  });

  it("maps a cancelled turn to status=cancelled and exit 2", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantToken("interrupt");
    sink.assistantCancelled("");
    const { exitCode } = finish();

    const events = parsed();
    // Cancelled drops the streamed buffer (content is authoritative) — no
    // assistant:content line is emitted for the aborted round.
    expect(events.map((e) => e.type)).toEqual([
      "assistant:start",
      "assistant:cancelled",
      "result",
    ]);
    const envelope = JsonResultEnvelopeSchema.parse(events.at(-1));
    expect(envelope.status).toBe("cancelled");
    expect(envelope.exitCode).toBe(2);
    expect(exitCode).toBe(2);
  });

  it("is idempotent: finish() writes exactly one result line", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantEnd("hi");
    const first = finish();
    const second = finish();

    const resultLines = parsed().filter((e) => e.type === "result");
    expect(resultLines).toHaveLength(1);
    expect(first.exitCode).toBe(0);
    expect(second.exitCode).toBe(0);
  });

  it("assigns monotonic seq across all lines and validates every line", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantToken("a");
    sink.toolCall({ id: "c1", name: "t", args: {}, startedAt: 0 });
    sink.toolResult({ id: "c1", name: "t", result: toolResult(), endedAt: 1 });
    sink.info("note");
    sink.assistantStart();
    sink.assistantEnd("final");
    finish();

    const events = parsed();
    // Each line carries seq 0..N-1 in order, and the timestamps come from the
    // injected clock (>= 1000), proving no real Date.now() leaked in.
    events.forEach((e, i) => {
      const v = JsonlEventSchema.parse(e);
      expect(v.seq).toBe(i);
      expect(v.ts).toBeGreaterThanOrEqual(1000);
    });
  });

  it("drops events emitted after finish() so nothing follows the result line", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.assistantEnd("hi");
    finish();
    // A late event from the loop (e.g. after shutdown) must not append past the
    // terminal envelope.
    sink.info("late");
    sink.toolCall({ id: "c9", name: "t", args: {}, startedAt: 0 });

    const events = parsed();
    expect(events.at(-1).type).toBe("result");
    expect(events.filter((e) => e.type === "result")).toHaveLength(1);
    expect(events.some((e) => e.type === "info")).toBe(false);
  });

  it("does not throw or drop a line on an unserializable tool error detail", () => {
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    sink.toolCall({ id: "c1", name: "t", args: {}, startedAt: 0 });
    // BigInt is not JSON-serializable; the sink must degrade the line, not throw.
    expect(() =>
      sink.toolResult({
        id: "c1",
        name: "t",
        result: toolResult({
          ok: false,
          summary: "boom",
          error: { code: "x", message: "m", recoverable: false, details: 1n },
        }),
        endedAt: 1,
      }),
    ).not.toThrow();
    sink.assistantStart();
    sink.assistantEnd("ok");
    finish();

    const events = parsed();
    // The stream is still well-formed JSONL with contiguous seq, and the degraded
    // tool:result line is marked rather than missing.
    events.forEach((e, i) => expect(e.seq).toBe(i));
    const degraded = events.find((e) => e.type === "tool:result");
    expect(degraded.serializationError).toBe(true);
    expect(JsonResultEnvelopeSchema.parse(events.at(-1)).status).toBe("success");
  });

  it("defaults to error/exit 1 when a turn ends with no terminal event", () => {
    // Defensive contract: a degenerate run that never reaches end/cancel/error
    // must still exit non-zero so a script treats it as failure.
    const { sink, finish, parsed } = makeSink();
    sink.assistantStart();
    const { exitCode } = finish();
    expect(exitCode).toBe(1);
    const envelope = JsonResultEnvelopeSchema.parse(parsed().at(-1));
    expect(envelope.status).toBe("error");
    expect(envelope.content).toBe("");
    expect(envelope.error).toBeNull();
  });
});
