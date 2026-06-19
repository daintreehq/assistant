import { describe, it, expect, vi } from "vitest";
import { DaintreeMcpClient, type LowLevelMcpClient } from "../src/mcp/client.js";
import type { AppConfig } from "../src/config.js";

/**
 * A LowLevelMcpClient fake that records the options (3rd arg of callTool / 2nd of
 * listTools) so we can assert the abort signal is forwarded, and can be told to
 * throw a timeout-shaped error (what the SDK raises for both real timeouts and an
 * explicit cancellation).
 */
function fakeClient(opts: { throwOnCall?: boolean; throwOnList?: boolean } = {}) {
  const callOptions: Array<unknown> = [];
  const listOptions: Array<unknown> = [];
  const client: LowLevelMcpClient = {
    listTools: vi.fn(async (_params?: unknown, options?: unknown) => {
      listOptions.push(options);
      if (opts.throwOnList) throw new Error("Request timed out");
      return { tools: [{ name: "terminal.list", inputSchema: { type: "object" } }] };
    }),
    callTool: vi.fn(
      async (_args: unknown, _schema?: unknown, options?: unknown) => {
        callOptions.push(options);
        if (opts.throwOnCall) throw new Error("Request timed out");
        return { content: [{ type: "text", text: "ok" }], isError: false };
      },
    ),
    close: async () => {},
  };
  return { client, callOptions, listOptions };
}

function makeClient(fake: LowLevelMcpClient): DaintreeMcpClient {
  return new DaintreeMcpClient({} as AppConfig, { clientOverride: fake });
}

describe("DaintreeMcpClient cancellation (#81)", () => {
  it("forwards the abort signal as RequestOptions to the low-level callTool", async () => {
    const { client, callOptions } = fakeClient();
    const mcp = makeClient(client);
    const controller = new AbortController();
    await mcp.callTool("terminal.list", {}, controller.signal);
    expect(callOptions[0]).toEqual({ signal: controller.signal });
  });

  it("omits RequestOptions on callTool when no signal is given", async () => {
    const { client, callOptions } = fakeClient();
    const mcp = makeClient(client);
    await mcp.callTool("terminal.list", {});
    expect(callOptions[0]).toBeUndefined();
  });

  it("forwards the abort signal as RequestOptions to the low-level listTools", async () => {
    const { client, listOptions } = fakeClient();
    const mcp = makeClient(client);
    const controller = new AbortController();
    await mcp.listTools(true, controller.signal);
    // warmToolCache (from construction) already listed once with no options; the
    // forced list with a signal is the most recent call.
    expect(listOptions.at(-1)).toEqual({ signal: controller.signal });
  });

  it("does NOT degrade the connection when a callTool is torn down by an abort", async () => {
    const { client } = fakeClient({ throwOnCall: true });
    const mcp = makeClient(client);
    const controller = new AbortController();
    controller.abort();

    await expect(
      mcp.callTool("terminal.list", {}, controller.signal),
    ).rejects.toThrow();
    // A user abort says nothing about transport health — the connection stays up.
    expect(mcp.isConnected()).toBe(true);
  });

  it("DOES degrade the connection on a real (non-abort) callTool failure", async () => {
    const { client } = fakeClient({ throwOnCall: true });
    const mcp = makeClient(client);

    await expect(mcp.callTool("terminal.list", {})).rejects.toThrow();
    // Without a fired signal this is a genuine transport failure → degrade.
    expect(mcp.isConnected()).toBe(false);
  });

  it("does NOT degrade the connection when a forced listTools is aborted", async () => {
    const { client } = fakeClient({ throwOnList: true });
    const mcp = makeClient(client);
    const controller = new AbortController();
    controller.abort();

    await expect(mcp.listTools(true, controller.signal)).rejects.toThrow();
    expect(mcp.isConnected()).toBe(true);
  });
});
