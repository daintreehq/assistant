import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { App } from "../src/cli/app.js";
import { handleUiCommand, runDoctor } from "../src/cli/commandData.js";
import type { LowLevelMcpClient } from "../src/mcp/client.js";

let lastStateDir = "";
function makeApp(): App {
  lastStateDir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-cmd-"));
  return App.create({
    overrides: {
      offline: true,
      stateDir: lastStateDir,
      projectPath: lastStateDir,
      tier: "operator",
    },
  });
}

describe("handleUiCommand (structured slash commands)", () => {
  let app: App;
  beforeEach(() => {
    app = makeApp();
  });
  afterEach(async () => {
    await app.shutdown();
    fs.rmSync(lastStateDir, { recursive: true, force: true });
  });

  it("/status reports MCP + config", async () => {
    const r = await handleUiCommand("/status", app);
    expect(r.handled).toBe(true);
    expect(r.title).toBe("Status");
    expect(r.text).toContain("Daintree MCP");
    expect(r.text).toContain("disconnected");
  });

  it("/permissions <tier> switches tier", async () => {
    const r = await handleUiCommand("/permissions supervisor", app);
    expect(r.text).toContain("supervisor");
    expect(app.config.tier).toBe("supervisor");
  });

  it("/permissions rejects an unknown tier", async () => {
    const before = app.config.tier;
    const r = await handleUiCommand("/permissions wizard", app);
    expect(r.text).toContain("Unknown tier");
    expect(app.config.tier).toBe(before);
  });

  it("/inbox switches to the inbox panel", async () => {
    const r = await handleUiCommand("/inbox", app);
    expect(r.switchPanel).toBe("inbox");
    expect(r.title).toContain("Inbox");
  });

  it("/quit signals exit", async () => {
    const r = await handleUiCommand("/quit", app);
    expect(r.quit).toBe(true);
  });

  it("unknown command is reported, not crashed", async () => {
    const r = await handleUiCommand("/frobnicate", app);
    expect(r.title).toBe("Unknown command");
  });

  it("/tools lists the registry", async () => {
    const r = await handleUiCommand("/tools", app);
    expect(r.title).toMatch(/^Tools/);
    expect((r.text ?? "").length).toBeGreaterThan(0);
  });
});

describe("runDoctor MCP probe", () => {
  function appWithMcp(client: LowLevelMcpClient): { app: App; dir: string } {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-doc-"));
    const app = App.create({
      overrides: { offline: true, stateDir: dir, projectPath: dir, tier: "operator" },
      mcpOptions: { clientOverride: client },
    });
    return { app, dir };
  }

  it("calls actions.getContext as a live read-only probe when connected", async () => {
    const calls: string[] = [];
    const { app, dir } = appWithMcp({
      listTools: async () => ({
        tools: [{ name: "actions.getContext", inputSchema: { type: "object" } }],
      }),
      callTool: async ({ name }) => {
        calls.push(name);
        return { content: [{ type: "text", text: "ctx" }], isError: false };
      },
    });
    try {
      const checks = await runDoctor(app);
      const probe = checks.find((c) => c.label === "mcp probe");
      expect(probe?.ok).toBe(true);
      expect(calls).toContain("actions.getContext");
    } finally {
      await app.shutdown();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("reports the probe as failed when the tool is not advertised", async () => {
    const { app, dir } = appWithMcp({
      listTools: async () => ({ tools: [{ name: "terminal.list", inputSchema: {} }] }),
      callTool: async () => ({ content: [], isError: false }),
    });
    try {
      const probe = (await runDoctor(app)).find((c) => c.label === "mcp probe");
      expect(probe?.ok).toBe(false);
      expect(probe?.detail).toContain("not advertised");
    } finally {
      await app.shutdown();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("reports the probe as failed when the tool returns isError", async () => {
    const { app, dir } = appWithMcp({
      listTools: async () => ({
        tools: [{ name: "actions.getContext", inputSchema: {} }],
      }),
      callTool: async () => ({
        content: [{ type: "text", text: "forbidden" }],
        isError: true,
      }),
    });
    try {
      const probe = (await runDoctor(app)).find((c) => c.label === "mcp probe");
      expect(probe?.ok).toBe(false);
      expect(probe?.detail).toContain("forbidden");
    } finally {
      await app.shutdown();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("reports a connection failure (not a tier issue) when listTools throws", async () => {
    const { app, dir } = appWithMcp({
      listTools: async () => {
        throw new Error("ECONNRESET");
      },
      callTool: async () => ({ content: [], isError: false }),
    });
    try {
      const probe = (await runDoctor(app)).find((c) => c.label === "mcp probe");
      expect(probe?.ok).toBe(false);
      expect(probe?.detail).toContain("probe failed");
      expect(probe?.detail).not.toContain("not advertised");
    } finally {
      await app.shutdown();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("adds no probe check when MCP is disconnected", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "dt-doc-"));
    const app = App.create({
      overrides: { offline: true, stateDir: dir, projectPath: dir, tier: "operator" },
    });
    try {
      const probe = (await runDoctor(app)).find((c) => c.label === "mcp probe");
      expect(probe).toBeUndefined();
    } finally {
      await app.shutdown();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
