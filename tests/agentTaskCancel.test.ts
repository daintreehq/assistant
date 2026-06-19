import { describe, it, expect, vi } from "vitest";
import { agentTaskTools } from "../src/tools/agentTaskTools.js";
import type { ToolContext } from "../src/tools/types.js";

const spawn = agentTaskTools.find((t) => t.name === "agentTask.spawnForEdits")!;

function ctxWith(
  opts: {
    signal?: AbortSignal;
    callTool?: ReturnType<typeof vi.fn>;
    connected?: boolean;
    insertWatcher?: ReturnType<typeof vi.fn>;
  } = {},
) {
  const callTool =
    opts.callTool ??
    vi.fn(async () => ({
      isError: false,
      text: "",
      structuredContent: { terminalId: "term_1" },
    }));
  const insertWatcher =
    opts.insertWatcher ??
    vi.fn(() => ({
      id: "wch_1",
      title: "watch",
      cadenceMs: 1000,
      modelTier: "small",
      nextCheckAt: 0,
    }));
  const ctx = {
    mcp: { isConnected: () => opts.connected ?? true, callTool },
    db: { insertWatcher },
    config: { tier: "system" },
    actor: "main",
    signal: opts.signal,
    daemonActive: () => true,
  } as unknown as ToolContext;
  return { ctx, callTool, insertWatcher };
}

const baseArgs = { title: "do it", taskPrompt: "make a change" } as never;

describe("agentTask.spawnForEdits cancellation (#81)", () => {
  it("returns CANCELLED without launching when the signal is already aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    const { ctx, callTool } = ctxWith({ signal: controller.signal });

    const res = await spawn.handler(baseArgs, ctx);

    expect(res.ok).toBe(false);
    expect((res as { error?: { code?: string } }).error?.code).toBe("CANCELLED");
    // No agent was spawned for a turn the user already cancelled.
    expect(callTool).not.toHaveBeenCalled();
  });

  it("forwards the turn signal to the agent.launch MCP call", async () => {
    const controller = new AbortController();
    const { ctx, callTool } = ctxWith({ signal: controller.signal });

    await spawn.handler(baseArgs, ctx);

    expect(callTool).toHaveBeenCalledWith(
      "agent.launch",
      expect.any(Object),
      controller.signal,
    );
  });

  it("maps a launch torn down by the abort to CANCELLED (not AGENT_LAUNCH_FAILED)", async () => {
    const controller = new AbortController();
    const callTool = vi.fn(async () => {
      // The user pressed Escape; the SDK rejects with a timeout-shaped error.
      controller.abort();
      throw new Error("Request timed out");
    });
    const { ctx } = ctxWith({ signal: controller.signal, callTool });

    const res = await spawn.handler(baseArgs, ctx);

    expect(res.ok).toBe(false);
    expect((res as { error?: { code?: string } }).error?.code).toBe("CANCELLED");
  });

  it("does NOT mask a post-launch bookkeeping failure as CANCELLED", async () => {
    // Launch SUCCEEDS (the agent is really spawned), then the watcher insert throws
    // while the signal happens to be aborted. The old broad catch would have
    // reported CANCELLED, hiding the launched agent; the scoped catch lets the real
    // error propagate (the registry wraps it as TOOL_THREW) instead.
    const controller = new AbortController();
    const callTool = vi.fn(async () => {
      controller.abort();
      return { isError: false, text: "", structuredContent: { terminalId: "term_1" } };
    });
    const insertWatcher = vi.fn(() => {
      throw new Error("sqlite boom");
    });
    const { ctx } = ctxWith({ signal: controller.signal, callTool, insertWatcher });

    await expect(
      spawn.handler({ ...(baseArgs as object), watcher: { create: true } } as never, ctx),
    ).rejects.toThrow("sqlite boom");
  });
});
