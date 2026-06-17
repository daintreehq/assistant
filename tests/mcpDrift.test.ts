import { describe, it, expect } from "vitest";
import {
  DaintreeMcpClient,
  type LowLevelMcpClient,
} from "../src/mcp/client.js";
import type { AppConfig } from "../src/config.js";
import { DOCUMENTED_MCP_TOOL_NAMES } from "../src/models/prompts/daintreeMcp.js";

/** A LowLevelMcpClient fake whose listTools() returns the given names. */
function fakeClient(toolNames: string[]): LowLevelMcpClient {
  return {
    listTools: async () => ({
      tools: toolNames.map((name) => ({ name, inputSchema: { type: "object" } })),
    }),
    callTool: async () => ({ content: [], isError: false }),
    close: async () => {},
  };
}

function makeClient(toolNames: string[]): DaintreeMcpClient {
  return new DaintreeMcpClient({} as AppConfig, {
    clientOverride: fakeClient(toolNames),
  });
}

describe("DOCUMENTED_MCP_TOOL_NAMES (#7)", () => {
  it("is a non-empty array of strings", () => {
    expect(Array.isArray(DOCUMENTED_MCP_TOOL_NAMES)).toBe(true);
    expect(DOCUMENTED_MCP_TOOL_NAMES.length).toBeGreaterThan(0);
    for (const name of DOCUMENTED_MCP_TOOL_NAMES) {
      expect(typeof name).toBe("string");
    }
  });

  it("excludes the documented negative examples (non-existent / local-only tools)", () => {
    for (const bogus of [
      "terminal.listStatus",
      "terminal.waitForAny",
      "terminal.focus",
    ]) {
      expect(DOCUMENTED_MCP_TOOL_NAMES).not.toContain(bogus);
    }
  });
});

describe("DaintreeMcpClient drift detection (#7)", () => {
  it("reports no drift when the live server advertises a superset of documented tools", async () => {
    const client = makeClient([...DOCUMENTED_MCP_TOOL_NAMES, "extra.live.tool"]);
    const st = await client.connect();
    expect(st.connected).toBe(true);
    expect(st.driftWarnings).toBeUndefined();
    expect(st.toolCount).toBe(DOCUMENTED_MCP_TOOL_NAMES.length + 1);
  });

  it("warns about a documented tool missing from the live server, without failing the connection", async () => {
    const missing = DOCUMENTED_MCP_TOOL_NAMES[0];
    const live = DOCUMENTED_MCP_TOOL_NAMES.filter((n) => n !== missing);
    const client = makeClient(live);
    const st = await client.connect();
    // Drift is warning-only: the connection stays up.
    expect(st.connected).toBe(true);
    expect(st.driftWarnings).toBeDefined();
    expect(st.driftWarnings?.length).toBe(1);
    expect(st.driftWarnings?.[0]).toContain(missing);
  });

  it("treats an empty live tool list as unknown, not total drift", async () => {
    const client = makeClient([]);
    const st = await client.connect();
    expect(st.connected).toBe(true);
    expect(st.driftWarnings).toBeUndefined();
  });
});
