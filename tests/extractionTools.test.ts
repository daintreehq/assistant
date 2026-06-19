import { describe, it, expect, vi } from "vitest";
import {
  extractionTools,
  ExtractArgs,
  ExtractAsyncArgs,
  runAsyncExtraction,
} from "../src/tools/extractionTools.js";
import type { ToolContext } from "../src/tools/types.js";

const extract = extractionTools.find((t) => t.name === "terminal.extract")!;
const extractAsync = extractionTools.find(
  (t) => t.name === "terminal.extract.async",
)!;

/** A per-terminal status entry, optionally carrying the inline recentOutput tail. */
type StatusEntry = {
  terminalId: string;
  agentState?: string;
  recentOutput?: string;
  exitCode?: number;
  spawnedAt?: number;
  lastTransitionAt?: number;
};
/**
 * A status result shaped like Daintree's terminal.getStatus. When `textOnly` the
 * payload lands ONLY in the text body with no structuredContent — Daintree's real
 * shape (#108) — exercising the parse-fallback path.
 */
function statusRes(entries: Array<StatusEntry>, textOnly = false) {
  return textOnly
    ? { isError: false, text: JSON.stringify({ terminals: entries }), structuredContent: undefined }
    : { isError: false, text: "", structuredContent: { terminals: entries } };
}
/** An output result shaped like Daintree's terminal.getOutput (text-only when set). */
function outputRes(content: string, textOnly = false) {
  return textOnly
    ? { isError: false, text: content, structuredContent: undefined }
    : { isError: false, text: "", structuredContent: { content } };
}

interface CtxParts {
  connected?: boolean;
  status?: (call: number) => Array<StatusEntry>;
  output?: (call: number) => string;
  chat?: ReturnType<typeof vi.fn>;
  json?: ReturnType<typeof vi.fn>;
  publish?: ReturnType<typeof vi.fn>;
  /** Deliver terminal payloads only in the text body (Daintree's real shape). */
  textOnly?: boolean;
}

function ctxWith(parts: CtxParts = {}): {
  ctx: ToolContext;
  chat: ReturnType<typeof vi.fn>;
  json: ReturnType<typeof vi.fn>;
  publish: ReturnType<typeof vi.fn>;
} {
  let statusCall = 0;
  let outputCall = 0;
  const chat = parts.chat ?? vi.fn().mockResolvedValue({ content: "extracted" });
  const json = parts.json ?? vi.fn().mockResolvedValue({ result: "x" });
  const publish = parts.publish ?? vi.fn().mockReturnValue({ id: "evt_1", count: 1 });
  const ctx = {
    mcp: {
      isConnected: () => parts.connected ?? true,
      callTool: vi.fn(async (name: string) => {
        if (name === "terminal.getStatus") {
          const entries = parts.status
            ? parts.status(statusCall++)
            : [{ terminalId: "t1", agentState: "working" }];
          return statusRes(entries, parts.textOnly);
        }
        if (name === "terminal.getOutput") {
          const content = parts.output ? parts.output(outputCall++) : "log line";
          return outputRes(content, parts.textOnly);
        }
        return { isError: true, text: "", structuredContent: {} };
      }),
    },
    router: { chat, json },
    queue: { publish },
    actor: "main",
  } as unknown as ToolContext;
  return { ctx, chat, json, publish };
}

describe("terminal.extract — inline", () => {
  it("extracts plain text via router.chat (not json) when format=text", async () => {
    const { ctx, chat, json } = ctxWith({
      chat: vi.fn().mockResolvedValue({ content: "  the answer  " }),
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "what is the answer", format: "text", pollIntervalMs: 0, maxAttempts: 30, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect(chat).toHaveBeenCalledTimes(1);
    expect(json).not.toHaveBeenCalled();
    expect((res.result as { result: string }).result).toBe("the answer");
  });

  it("extracts JSON via router.json (never router.chat) when format=json", async () => {
    const { ctx, chat, json } = ctxWith({
      json: vi.fn().mockResolvedValue({ result: { status: "passed" } }),
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "extract status", format: "json", jsonSchema: "{ status: string }", pollIntervalMs: 0, maxAttempts: 30, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect(json).toHaveBeenCalledTimes(1);
    expect(chat).not.toHaveBeenCalled();
    expect((res.result as { result: unknown }).result).toEqual({ status: "passed" });
  });

  it("waits until a `contains` condition is met, then extracts", async () => {
    // First read has no marker, second read does.
    const { ctx, chat } = ctxWith({
      output: (c) => (c === 0 ? "still building..." : "BUILD OK done"),
      chat: vi.fn().mockResolvedValue({ content: "ok" }),
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "did it pass", format: "text", wait: { contains: "BUILD OK" }, pollIntervalMs: 0, maxAttempts: 30, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { attempts: number }).attempts).toBe(2);
    expect(chat).toHaveBeenCalledTimes(1);
  });

  it("fails WAIT_TIMEOUT when the condition never resolves within maxAttempts", async () => {
    const { ctx, chat } = ctxWith({ output: () => "never matches" });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", wait: { contains: "ZZZ" }, pollIntervalMs: 0, maxAttempts: 3, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("WAIT_TIMEOUT");
    expect((res.error?.details as { attempts: number }).attempts).toBe(3);
    expect(chat).not.toHaveBeenCalled();
  });

  it("matches a runtimeStatusIs:exited wait condition", async () => {
    const { ctx } = ctxWith({
      status: () => [{ terminalId: "t1", agentState: "exited" }],
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "summarize the run", format: "text", wait: { runtimeStatusIs: "exited" }, pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { finished: boolean }).finished).toBe(true);
  });

  it("rejects a modelJudge wait condition as unsupported", async () => {
    const { ctx, chat } = ctxWith({});
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", wait: { modelJudge: "is it done?" }, pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("UNSUPPORTED_CONDITION");
    expect(chat).not.toHaveBeenCalled();
  });

  it("fails MCP_UNAVAILABLE when Daintree is not connected", async () => {
    const { ctx } = ctxWith({ connected: false });
    const res = await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("MCP_UNAVAILABLE");
  });

  it("gate mode: no instruction returns booleans and never calls the model", async () => {
    const { ctx, chat, json } = ctxWith({
      status: () => [{ terminalId: "t1", agentState: "exited" }],
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect(chat).not.toHaveBeenCalled();
    expect(json).not.toHaveBeenCalled();
    expect((res.result as { finished: boolean }).finished).toBe(true);
  });

  it("gate mode tolerates an exited terminal carrying an exitCode (#22)", async () => {
    // The new exitCode field threads through readSignals without breaking the
    // finished gate or triggering a model call.
    const { ctx, chat, json } = ctxWith({
      status: () => [{ terminalId: "t1", agentState: "exited", exitCode: 1 }],
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { finished: boolean }).finished).toBe(true);
    expect(chat).not.toHaveBeenCalled();
    expect(json).not.toHaveBeenCalled();
  });

  it("gate mode with an unmet wait still returns booleans (not WAIT_TIMEOUT)", async () => {
    const { ctx } = ctxWith({ output: () => "never matches" });
    const res = await extract.handler(
      { terminalIds: ["t1"], wait: { contains: "NEVER" }, format: "text", pollIntervalMs: 0, maxAttempts: 2, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { matched: boolean }).matched).toBe(false);
    expect((res.result as { finished: boolean }).finished).toBe(false);
  });

  it("matches runtimeStatusIs:exited only once ALL terminals have exited", async () => {
    // Poll 1: t2 still running → not met. Poll 2: both exited → met.
    let poll = 0;
    const { ctx } = ctxWith({
      status: () => {
        const both = poll++ >= 1;
        return [
          { terminalId: "t1", agentState: "exited" },
          { terminalId: "t2", agentState: both ? "exited" : "working" },
        ];
      },
    });
    const res = await extract.handler(
      { terminalIds: ["t1", "t2"], format: "text", wait: { runtimeStatusIs: "exited" }, pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { attempts: number }).attempts).toBe(2);
    expect((res.result as { finished: boolean }).finished).toBe(true);
  });

  it("classifies correctly when Daintree returns status/output only in the text body (#108)", async () => {
    // No structuredContent at all — the real Daintree shape. The fallback parse
    // must still surface the exited state and the scrollback tail.
    const { ctx } = ctxWith({
      textOnly: true,
      status: () => [{ terminalId: "t1", agentState: "exited" }],
      output: () => "BUILD OK done",
    });
    const res = await extract.handler(
      { terminalIds: ["t1"], format: "text", wait: { all: [{ runtimeStatusIs: "exited" }, { contains: "BUILD OK" }] }, pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { matched: boolean }).matched).toBe(true);
    expect((res.result as { finished: boolean }).finished).toBe(true);
  });

  it("does NOT satisfy a runtimeStatusIs:exited wait on a total status miss (#108)", async () => {
    // A total miss must not promote the (absent) terminal to runtimeStatus
    // "exited" — otherwise a wait on exit would fire against an empty tail.
    const { ctx } = ctxWith({ status: () => [] });
    const res = await extract.handler(
      { terminalIds: ["t1"], format: "text", wait: { runtimeStatusIs: "exited" }, pollIntervalMs: 0, maxAttempts: 2, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { matched: boolean }).matched).toBe(false);
    expect((res.result as { finished: boolean }).finished).toBe(false);
  });

  it("does NOT report finished on a total status miss — empty byId is not a clean exit (#108)", async () => {
    // readStatuses returns ok:true but zero terminals (the pre-fix empty-read
    // symptom). That must NOT be reported as every terminal having exited.
    const { ctx } = ctxWith({ status: () => [] });
    const res = await extract.handler(
      { terminalIds: ["t1"], format: "text", pollIntervalMs: 0, maxAttempts: 2, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { finished: boolean }).finished).toBe(false);
  });

  it("feeds the instruction and terminal tail to the extraction model", async () => {
    const chat = vi.fn().mockResolvedValue({ content: "answer" });
    const { ctx } = ctxWith({ output: () => "the build log content", chat });
    await extract.handler(
      { terminalIds: ["t1"], instruction: "find the error code", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    const sent = chat.mock.calls[0][1];
    const userMsg = sent.messages.find((m: { role: string }) => m.role === "user").content;
    expect(userMsg).toContain("find the error code");
    expect(userMsg).toContain("the build log content");
    expect(sent.maxTokens).toBe(400);
  });

  it("requests the inline output tail on terminal.getStatus", async () => {
    const { ctx } = ctxWith({});
    await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    const callTool = ctx.mcp.callTool as unknown as ReturnType<typeof vi.fn>;
    const statusCall = callTool.mock.calls.find((c) => c[0] === "terminal.getStatus");
    expect(statusCall?.[1]?.includeOutput).toEqual({ lines: 50, stripAnsi: true });
  });

  it("uses recentOutput and skips terminal.getOutput when the inline tail covers tailBytes", async () => {
    const tail = "BUILD OK and the rest of the log";
    const { ctx, chat } = ctxWith({
      status: () => [{ terminalId: "t1", agentState: "working", recentOutput: tail }],
      chat: vi.fn().mockResolvedValue({ content: "ok" }),
    });
    await extract.handler(
      // tailBytes small enough that the inline tail already covers it.
      { terminalIds: ["t1"], instruction: "did it pass", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 8, maxTokens: 400 },
      ctx,
    );
    const callTool = ctx.mcp.callTool as unknown as ReturnType<typeof vi.fn>;
    expect(callTool.mock.calls.filter((c) => c[0] === "terminal.getOutput")).toHaveLength(0);
    // The tail handed to the model is the last `tailBytes` chars of recentOutput.
    const userMsg = chat.mock.calls[0][1].messages.find(
      (m: { role: string }) => m.role === "user",
    ).content;
    expect(userMsg).toContain(tail.slice(-8));
  });

  it("falls back to terminal.getOutput when recentOutput is shorter than tailBytes", async () => {
    const { ctx } = ctxWith({
      // Inline tail present but far shorter than the requested 12000 bytes.
      status: () => [{ terminalId: "t1", agentState: "working", recentOutput: "short" }],
      output: () => "deep scrollback from getOutput",
    });
    await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    const callTool = ctx.mcp.callTool as unknown as ReturnType<typeof vi.fn>;
    expect(callTool.mock.calls.filter((c) => c[0] === "terminal.getOutput")).toHaveLength(1);
  });

  it("falls back to terminal.getOutput when recentOutput is absent", async () => {
    const { ctx } = ctxWith({
      status: () => [{ terminalId: "t1", agentState: "working" }],
      output: () => "deep scrollback from getOutput",
    });
    await extract.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    const callTool = ctx.mcp.callTool as unknown as ReturnType<typeof vi.fn>;
    expect(callTool.mock.calls.filter((c) => c[0] === "terminal.getOutput")).toHaveLength(1);
  });
});

describe("ExtractArgs validation", () => {
  it("requires jsonSchema when format=json and an instruction is given", () => {
    const parsed = ExtractArgs.safeParse({
      terminalIds: ["t1"],
      instruction: "extract",
      format: "json",
    });
    expect(parsed.success).toBe(false);
  });

  it("accepts json format with a jsonSchema", () => {
    const parsed = ExtractArgs.safeParse({
      terminalIds: ["t1"],
      instruction: "extract",
      format: "json",
      jsonSchema: "{ ok: boolean }",
    });
    expect(parsed.success).toBe(true);
  });

  it("ExtractAsyncArgs requires an instruction", () => {
    const parsed = ExtractAsyncArgs.safeParse({ terminalIds: ["t1"] });
    expect(parsed.success).toBe(false);
  });
});

describe("terminal.extract.async — background", () => {
  it("returns a requestId immediately without blocking", async () => {
    const { ctx } = ctxWith({});
    const res = await extractAsync.handler(
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      ctx,
    );
    expect(res.ok).toBe(true);
    expect((res.result as { requestId: string }).requestId).toBeTruthy();
  });

  it("publishes a done event via model_worker after extracting", async () => {
    const { ctx, publish } = ctxWith({
      chat: vi.fn().mockResolvedValue({ content: "the extracted fact" }),
    });
    await runAsyncExtraction(
      ctx,
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      "req_1",
    );
    expect(publish).toHaveBeenCalledTimes(1);
    const arg = publish.mock.calls[0][0];
    expect(arg.source).toBe("model_worker");
    expect(arg.severity).toBe("done");
    expect(arg.evidence[0]).toContain("the extracted fact");
  });

  it("publishes an attention/fail event when the verdict fails", async () => {
    const { ctx, publish } = ctxWith({
      chat: vi.fn().mockResolvedValue({ content: "tests failed" }),
      json: vi.fn().mockResolvedValue({ pass: false, reason: "the suite is red" }),
    });
    await runAsyncExtraction(
      ctx,
      {
        terminalIds: ["t1"],
        instruction: "did the tests pass",
        format: "text",
        verdictInstruction: "tests passed",
        pollIntervalMs: 0,
        maxAttempts: 5,
        tailBytes: 12000,
        maxTokens: 400,
      },
      "req_2",
    );
    const arg = publish.mock.calls[0][0];
    expect(arg.severity).toBe("attention");
    expect(arg.title).toContain("fail");
    expect(arg.summary).toBe("the suite is red");
  });

  it("publishes an attention event (no extraction) when the wait times out", async () => {
    const { ctx, publish, chat } = ctxWith({ output: () => "still running" });
    await runAsyncExtraction(
      ctx,
      { terminalIds: ["t1"], instruction: "x", format: "text", wait: { contains: "DONE" }, pollIntervalMs: 0, maxAttempts: 2, tailBytes: 12000, maxTokens: 400 },
      "req_wait",
    );
    const arg = publish.mock.calls[0][0];
    expect(arg.source).toBe("model_worker");
    expect(arg.severity).toBe("attention");
    expect(arg.title).toContain("timed out");
    expect(chat).not.toHaveBeenCalled();
  });

  it("publishes an error event when extraction throws", async () => {
    const { ctx, publish } = ctxWith({
      chat: vi.fn().mockRejectedValue(new Error("model exploded")),
    });
    await runAsyncExtraction(
      ctx,
      { terminalIds: ["t1"], instruction: "x", format: "text", pollIntervalMs: 0, maxAttempts: 5, tailBytes: 12000, maxTokens: 400 },
      "req_3",
    );
    const arg = publish.mock.calls[0][0];
    expect(arg.source).toBe("model_worker");
    expect(arg.severity).toBe("error");
    expect(arg.summary).toContain("model exploded");
  });
});

describe("terminal extraction — cancellation (#81)", () => {
  it("threads the turn signal into the terminal.getStatus / getOutput MCP reads", async () => {
    const controller = new AbortController();
    const { ctx } = ctxWith({
      // recentOutput shorter than tailBytes forces the deep getOutput fallback too.
      status: () => [{ terminalId: "t1", agentState: "working", recentOutput: "hi" }],
      output: () => "deep output",
    });
    (ctx as unknown as { signal: AbortSignal }).signal = controller.signal;

    await extract.handler(
      {
        terminalIds: ["t1"],
        instruction: "x",
        format: "text",
        pollIntervalMs: 0,
        maxAttempts: 1,
        tailBytes: 12000,
        maxTokens: 400,
      },
      ctx,
    );

    const callMock = ctx.mcp.callTool as unknown as ReturnType<typeof vi.fn>;
    const statusCall = callMock.mock.calls.find((c) => c[0] === "terminal.getStatus");
    const outputCall = callMock.mock.calls.find((c) => c[0] === "terminal.getOutput");
    // The signal rides as the 3rd arg of callTool, so a slow Daintree read is
    // torn down on Escape rather than completing in the background.
    expect(statusCall?.[2]).toBe(controller.signal);
    expect(outputCall?.[2]).toBe(controller.signal);
  });

  it("threads the turn signal into the extraction model call", async () => {
    const controller = new AbortController();
    const chat = vi.fn().mockResolvedValue({ content: "extracted" });
    const { ctx } = ctxWith({ chat });
    (ctx as unknown as { signal: AbortSignal }).signal = controller.signal;

    await extract.handler(
      {
        terminalIds: ["t1"],
        instruction: "x",
        format: "text",
        pollIntervalMs: 0,
        maxAttempts: 1,
        tailBytes: 12000,
        maxTokens: 400,
      },
      ctx,
    );

    expect(chat).toHaveBeenCalled();
    expect(chat.mock.calls[0][1].signal).toBe(controller.signal);
  });

  it("stops polling promptly when the turn signal aborts mid-wait", async () => {
    const controller = new AbortController();
    let statusReads = 0;
    const { ctx } = ctxWith({
      status: () => {
        statusReads++;
        // Simulate the user cancelling after a couple of polls.
        if (statusReads >= 2) controller.abort();
        return [{ terminalId: "t1", agentState: "working", recentOutput: "still running" }];
      },
    });
    (ctx as unknown as { signal: AbortSignal }).signal = controller.signal;

    const res = await extract.handler(
      {
        terminalIds: ["t1"],
        instruction: "did it pass",
        format: "text",
        wait: { contains: "NEVER MATCHES" },
        pollIntervalMs: 0,
        maxAttempts: 30,
        tailBytes: 12000,
        maxTokens: 400,
      },
      ctx,
    );

    // The wait never matched and the turn was cancelled — a clean WAIT_TIMEOUT,
    // and crucially the poll did NOT run out all 30 attempts: it halted on abort.
    expect(res.ok).toBe(false);
    expect((res as { error?: { code?: string } }).error?.code).toBe("WAIT_TIMEOUT");
    expect(statusReads).toBeLessThan(5);
  });

  it("the fire-and-forget async path strips the turn signal so it outlives the turn", async () => {
    // An already-aborted turn must NOT short-circuit a detached background poll.
    const controller = new AbortController();
    controller.abort();
    let statusReads = 0;
    const publish = vi.fn().mockReturnValue({ id: "evt", count: 1 });
    const { ctx } = ctxWith({
      publish,
      status: () => {
        statusReads++;
        return [{ terminalId: "t1", agentState: "exited", recentOutput: "done" }];
      },
    });
    (ctx as unknown as { signal: AbortSignal }).signal = controller.signal;

    const res = await extractAsync.handler(
      {
        terminalIds: ["t1"],
        instruction: "summarize",
        format: "text",
        pollIntervalMs: 0,
        maxAttempts: 5,
        tailBytes: 12000,
        maxTokens: 400,
      },
      ctx,
    );
    expect(res.ok).toBe(true);
    // Let the detached promise settle.
    await new Promise((r) => setTimeout(r, 0));

    // Had the signal NOT been stripped, pollUntil would break before any read
    // (aborted at entry) — statusReads would be 0. It actually polled and published.
    expect(statusReads).toBeGreaterThanOrEqual(1);
    expect(publish).toHaveBeenCalled();
  });
});
