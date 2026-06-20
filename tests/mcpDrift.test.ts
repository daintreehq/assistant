import { describe, it, expect } from "vitest";
import {
  DaintreeMcpClient,
  DAINTREE_GRANT_TOOL_NAMES,
  toolsAdvertiseGrantSupport,
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

  it("documents the shipped fleet-arming MCP tools (#136)", () => {
    for (const name of ["terminal.arm", "terminal.disarm", "terminal.disarmAll"]) {
      expect(DOCUMENTED_MCP_TOOL_NAMES).toContain(name);
    }
  });

  it("excludes the documented negative examples (non-existent / local-only tools)", () => {
    for (const bogus of [
      "terminal.listStatus",
      "terminal.waitForAny",
      "terminal.focus",
      // terminal.arm/disarm/disarmAll ARE on the live MCP surface (#10696) and are
      // documented above. But terminal.armByState (a renderer-scoped action id) and
      // the fleet.armByState internal store call are NOT advertised by the live
      // server, so listing either here would emit a permanent false-positive drift
      // warning.
      "fleet.armByState",
      "terminal.armByState",
    ]) {
      expect(DOCUMENTED_MCP_TOOL_NAMES).not.toContain(bogus);
    }
  });

  it("has no duplicate entries (duplicates would emit duplicate drift warnings)", () => {
    expect(new Set(DOCUMENTED_MCP_TOOL_NAMES).size).toBe(
      DOCUMENTED_MCP_TOOL_NAMES.length,
    );
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

  it("reports one warning per missing documented tool", async () => {
    const missing = DOCUMENTED_MCP_TOOL_NAMES.slice(0, 3);
    const live = DOCUMENTED_MCP_TOOL_NAMES.filter((n) => !missing.includes(n));
    const client = makeClient(live);
    const st = await client.connect();
    expect(st.driftWarnings?.length).toBe(3);
    for (const name of missing) {
      expect(st.driftWarnings?.some((w) => w.includes(name))).toBe(true);
    }
  });

  it("treats an empty live tool list as unknown, not total drift", async () => {
    const client = makeClient([]);
    const st = await client.connect();
    expect(st.connected).toBe(true);
    expect(st.driftWarnings).toBeUndefined();
  });
});

describe("Daintree grant-support capability seam (#24)", () => {
  it("DAINTREE_GRANT_TOOL_NAMES is empty until Daintree exposes an external grants API", () => {
    expect(Array.isArray(DAINTREE_GRANT_TOOL_NAMES)).toBe(true);
    expect(DAINTREE_GRANT_TOOL_NAMES.length).toBe(0);
  });

  it("toolsAdvertiseGrantSupport is false with the empty default allowlist, regardless of live tools", () => {
    expect(toolsAdvertiseGrantSupport([{ name: "session.grant.create" }])).toBe(
      false,
    );
    expect(toolsAdvertiseGrantSupport([])).toBe(false);
  });

  it("toolsAdvertiseGrantSupport lights up once the allowlist is populated and a live tool matches", () => {
    const allow = ["session.grant.create"];
    expect(
      toolsAdvertiseGrantSupport([{ name: "session.grant.create" }], allow),
    ).toBe(true);
    expect(toolsAdvertiseGrantSupport([{ name: "terminal.list" }], allow)).toBe(
      false,
    );
  });

  it("hasDaintreeGrantSupport() is false today even when connected with a full tool set", async () => {
    const client = makeClient([...DOCUMENTED_MCP_TOOL_NAMES]);
    await client.connect();
    expect(client.isConnected()).toBe(true);
    expect(client.hasDaintreeGrantSupport()).toBe(false);
  });

  it("hasDaintreeGrantSupport() is false when disconnected", () => {
    const client = new DaintreeMcpClient({} as AppConfig);
    expect(client.isConnected()).toBe(false);
    expect(client.hasDaintreeGrantSupport()).toBe(false);
  });
});
