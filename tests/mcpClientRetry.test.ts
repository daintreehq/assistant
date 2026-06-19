import { describe, it, expect, vi } from "vitest";
import { DaintreeMcpClient, type LowLevelMcpClient } from "../src/mcp/client.js";
import type { AppConfig } from "../src/config.js";

/** A LowLevelMcpClient whose callTool fails (with a transient transport error) a
 *  configurable number of times before succeeding, recording each options arg. */
function fakeClient(failFirst = 0, failError: () => unknown = () => new Error("fetch failed")) {
  let calls = 0;
  const callOptions: Array<unknown> = [];
  const client: LowLevelMcpClient = {
    listTools: vi.fn(async () => ({
      tools: [{ name: "terminal.list", inputSchema: { type: "object" } }],
    })),
    callTool: vi.fn(async (_args: unknown, _schema?: unknown, options?: unknown) => {
      calls++;
      callOptions.push(options);
      if (calls <= failFirst) throw failError();
      return { content: [{ type: "text", text: "ok" }], isError: false };
    }),
    close: async () => {},
  };
  return { client, callOptions, getCalls: () => calls };
}

function makeClient(fake: LowLevelMcpClient): DaintreeMcpClient {
  return new DaintreeMcpClient({} as AppConfig, { clientOverride: fake });
}

describe("DaintreeMcpClient read retry + timeout (#123)", () => {
  it("retries a transient transport error and then succeeds without degrading", async () => {
    const { client, getCalls } = fakeClient(2);
    const mcp = makeClient(client);
    const res = await mcp.callTool("terminal.getStatus", {}, undefined, {
      retries: 2,
      timeoutMs: 20_000,
    });
    expect(res.text).toBe("ok");
    expect(getCalls()).toBe(3); // 2 failures + 1 success
    expect(mcp.isConnected()).toBe(true);
  });

  it("threads the timeout into the low-level RequestOptions", async () => {
    const { client, callOptions } = fakeClient(0);
    const mcp = makeClient(client);
    await mcp.callTool("terminal.getStatus", {}, undefined, { timeoutMs: 12_345 });
    expect(callOptions[0]).toMatchObject({ timeout: 12_345 });
  });

  it("degrades only after the retry budget is exhausted", async () => {
    const { client, getCalls } = fakeClient(99);
    const mcp = makeClient(client);
    await expect(
      mcp.callTool("terminal.getStatus", {}, undefined, { retries: 2 }),
    ).rejects.toThrow("fetch failed");
    expect(getCalls()).toBe(3); // 1 initial + 2 retries
    expect(mcp.isConnected()).toBe(false);
  });

  it("does NOT retry by default (mutating callers stay single-shot)", async () => {
    const { client, getCalls } = fakeClient(99);
    const mcp = makeClient(client);
    await expect(mcp.callTool("terminal.sendInput", {})).rejects.toThrow("fetch failed");
    expect(getCalls()).toBe(1);
    expect(mcp.isConnected()).toBe(false);
  });

  it("does NOT retry a non-transient error even when retries are allowed", async () => {
    const { client, getCalls } = fakeClient(99, () => new Error("invalid params"));
    const mcp = makeClient(client);
    await expect(
      mcp.callTool("terminal.getStatus", {}, undefined, { retries: 2 }),
    ).rejects.toThrow("invalid params");
    expect(getCalls()).toBe(1); // not retried — not a transport hiccup
    expect(mcp.isConnected()).toBe(false);
  });
});
