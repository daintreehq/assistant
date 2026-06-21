import { describe, it, expect, vi } from "vitest";
import { contextTools } from "../src/tools/contextTools.js";
import type { ToolContext } from "../src/tools/types.js";

const read = contextTools.find((t) => t.name === "terminal.read")!;
const summarize = contextTools.find((t) => t.name === "terminal.summarize")!;

/** An output result shaped like Daintree's terminal.getOutput (text-only when set). */
function outputRes(content: string, textOnly = false) {
  return textOnly
    ? { isError: false, text: content, structuredContent: undefined }
    : { isError: false, text: "", structuredContent: { content } };
}

interface CtxParts {
  connected?: boolean;
  output?: string;
  outputError?: boolean;
  textOnly?: boolean;
  chat?: ReturnType<typeof vi.fn>;
}

function ctxWith(parts: CtxParts = {}): {
  ctx: ToolContext;
  callTool: ReturnType<typeof vi.fn>;
  chat: ReturnType<typeof vi.fn>;
} {
  const chat = parts.chat ?? vi.fn().mockResolvedValue({ content: "a summary" });
  const callTool = vi.fn(async (name: string) => {
    if (name === "terminal.getOutput") {
      if (parts.outputError) return { isError: true, text: "boom", structuredContent: {} };
      return outputRes(parts.output ?? "raw scrollback line 1\nline 2", parts.textOnly);
    }
    return { isError: true, text: "", structuredContent: {} };
  });
  const ctx = {
    mcp: { isConnected: () => parts.connected ?? true, callTool },
    router: { chat },
    queue: { digest: () => [], format: () => "" },
    actor: "main",
  } as unknown as ToolContext;
  return { ctx, callTool, chat };
}

describe("terminal.read — raw verbatim, no model", () => {
  it("returns scrollback content verbatim and never calls the model", async () => {
    const { ctx, callTool, chat } = ctxWith({ output: "the exact agent answer" });
    const res = await read.handler({ terminalId: "t1", maxLines: 200 }, ctx);
    expect(res.ok).toBe(true);
    expect(res.summary).toBe("the exact agent answer");
    expect((res.result as { content: string }).content).toBe("the exact agent answer");
    // No router.chat — this is the whole point of the verbatim path.
    expect(chat).not.toHaveBeenCalled();
    // It forwarded maxLines to terminal.getOutput.
    const call = callTool.mock.calls.find((c) => c[0] === "terminal.getOutput");
    expect(call?.[1]).toMatchObject({ terminalId: "t1", maxLines: 200 });
  });

  it("reads from the raw text body when structuredContent is absent (Daintree's real shape)", async () => {
    const { ctx } = ctxWith({ output: "body-only scrollback", textOnly: true });
    const res = await read.handler({ terminalId: "t1", maxLines: 200 }, ctx);
    expect((res.result as { content: string }).content).toBe("body-only scrollback");
  });

  it("caps the returned text to the last tailBytes characters", async () => {
    const { ctx } = ctxWith({ output: "0123456789" });
    const res = await read.handler({ terminalId: "t1", maxLines: 200, tailBytes: 4 }, ctx);
    expect((res.result as { content: string }).content).toBe("6789");
  });

  it("fails cleanly when MCP is disconnected", async () => {
    const { ctx, callTool } = ctxWith({ connected: false });
    const res = await read.handler({ terminalId: "t1", maxLines: 200 }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("MCP_UNAVAILABLE");
    expect(callTool).not.toHaveBeenCalled();
  });

  it("surfaces a terminal.getOutput error", async () => {
    const { ctx } = ctxWith({ outputError: true });
    const res = await read.handler({ terminalId: "t1", maxLines: 200 }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("TERMINAL_OUTPUT");
  });
});

describe("terminal.summarize — truncation legibility", () => {
  it("flags a summarizer token-cap truncation and steers to terminal.read", async () => {
    const chat = vi.fn().mockResolvedValue({ content: "partial summary", finishReason: "length" });
    const { ctx } = ctxWith({ chat });
    const res = await summarize.handler({ terminalId: "t1" }, ctx);
    expect(res.ok).toBe(true);
    expect((res.result as { truncated: boolean }).truncated).toBe(true);
    expect(res.summary).toContain("terminal.read");
    expect(res.summary).toContain("cut off");
  });

  it("does not flag truncation on a clean finishReason", async () => {
    const chat = vi.fn().mockResolvedValue({ content: "a summary", finishReason: "stop" });
    const { ctx } = ctxWith({ chat });
    const res = await summarize.handler({ terminalId: "t1" }, ctx);
    expect((res.result as { truncated: boolean }).truncated).toBe(false);
    expect(res.summary).toBe("a summary");
  });
});
