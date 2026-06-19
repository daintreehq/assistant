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
    db: {
      insertWatcher,
      // #98's launch saga: spawnForEdits now records a durable launch row before
      // calling agent.launch. Stub the saga methods so the cancellation tests
      // exercise the fresh-launch path against this fake db — no active record
      // (so it takes the fresh branch), insert hands back an id, updates are no-ops.
      findActiveAgentLaunch: () => undefined,
      insertAgentLaunch: (rec: Record<string, unknown>) => ({
        id: "launch_1",
        stage: "launch_requested",
        ...rec,
      }),
      updateAgentLaunch: () => {},
    },
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
    // while the signal happens to be aborted. The abort handling is scoped to the
    // launch call, so it must NOT report CANCELLED here — that would hide the agent
    // that is actually running. The launch saga (#98) treats a watcher-attach failure
    // as non-fatal: the result stays a success (the agent IS up) but surfaces the
    // problem as a watcherWarning, leaving the saga recoverable for a re-attach.
    const controller = new AbortController();
    const callTool = vi.fn(async () => {
      controller.abort();
      return { isError: false, text: "", structuredContent: { terminalId: "term_1" } };
    });
    const insertWatcher = vi.fn(() => {
      throw new Error("sqlite boom");
    });
    const { ctx } = ctxWith({ signal: controller.signal, callTool, insertWatcher });

    const res = await spawn.handler(
      { ...(baseArgs as object), watcher: { create: true } } as never,
      ctx,
    );

    // The launched agent is not hidden, and the abort is not masking the failure.
    expect(res.ok).toBe(true);
    expect((res as { error?: { code?: string } }).error?.code).not.toBe("CANCELLED");
    const payload = (res as { result?: Record<string, unknown> }).result ?? {};
    expect(payload.terminalId).toBe("term_1");
    expect(payload.watcherId).toBeUndefined();
    expect(payload.watcherWarning).toContain(
      "watcher could not be attached: sqlite boom",
    );
  });
});
