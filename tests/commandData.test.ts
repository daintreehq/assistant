import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { App } from "../src/cli/app.js";
import { handleUiCommand, runDoctor } from "../src/cli/commandData.js";
import { handleSlashCommand } from "../src/cli/commands.js";
import { render } from "../src/cli/render.js";
import { HOST_TERMINAL_CLEAR } from "../src/cli/terminalClear.js";
import { COMMAND_REGISTRY, helpLines } from "../src/commandRegistry.js";
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

  it("/audit tags a grant_ok row with the grant's source", async () => {
    app.db.insertAudit({
      actor: "watcher",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "grant_ok",
      durationMs: 5,
      summary: "committed",
      grantSource: "local",
      grantId: "grt_x",
    });
    app.db.insertAudit({
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read",
    });
    const r = await handleUiCommand("/audit", app);
    expect(r.switchPanel).toBe("audit");
    expect(r.text).toContain("grant_ok[local]");
    // A plain ok row is not tagged with a source bracket.
    const readLine = r.text!.split("\n").find((l) => l.includes("fs.read"))!;
    expect(readLine).toContain(" ok ");
    expect(readLine).not.toContain("[");
  });

  it("/audit renders a sourceless grant_ok row (pre-v4 rows) without a bracket", async () => {
    // A grant_ok row whose grantSource is absent (as migrated pre-v4 rows are)
    // must render plain 'grant_ok', never 'grant_ok[undefined]'.
    app.db.insertAudit({
      actor: "watcher",
      toolName: "git.push",
      argsJson: "{}",
      outcome: "grant_ok",
      durationMs: 2,
      summary: "pushed",
    });
    const r = await handleUiCommand("/audit", app);
    const line = r.text!.split("\n").find((l) => l.includes("git.push"))!;
    expect(line).toContain("grant_ok ");
    expect(line).not.toContain("grant_ok[");
  });

  it("/audit export json returns a filtered JSON array", async () => {
    app.db.insertAudit({
      ts: 1000,
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read",
    });
    app.db.insertAudit({
      ts: 2000,
      actor: "watcher",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "error",
      durationMs: 2,
      summary: "boom",
    });
    const r = await handleUiCommand("/audit export json actor=main", app);
    expect(r.switchPanel).toBe("audit");
    expect(r.title).toContain("json");
    const parsed = JSON.parse(r.text!) as Array<{ toolName: string }>;
    expect(parsed).toHaveLength(1);
    expect(parsed[0].toolName).toBe("fs.read");
  });

  it("/audit export csv returns a header and data rows", async () => {
    app.db.insertAudit({
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read",
    });
    const r = await handleUiCommand("/audit export csv", app);
    expect(r.title).toContain("csv");
    expect(r.text!.split("\r\n")[0]).toContain("id,ts,actor,toolName");
    expect(r.text).toContain("fs.read");
  });

  it("/audit export reports a usage error for a bad format", async () => {
    const r = await handleUiCommand("/audit export xml", app);
    expect(r.switchPanel).toBe("audit");
    expect(r.text).toContain("Usage");
  });

  it("/audit [n] still lists recent calls, unaffected by export", async () => {
    app.db.insertAudit({
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read",
    });
    const r = await handleUiCommand("/audit 5", app);
    expect(r.title).toContain("last");
    expect(r.text).toContain("fs.read");
  });

  it("/explain with no runs reports the empty index", async () => {
    const r = await handleUiCommand("/explain", app);
    expect(r.handled).toBe(true);
    expect(r.title).toContain("recent runs");
    expect(r.text).toContain("no runs recorded");
  });

  it("/explain lists recent run ids when runs exist", async () => {
    app.db.insertRunEvent({ runId: "run_a", seq: 0, type: "assistant:start" });
    app.db.insertRunEvent({ runId: "run_a", seq: 1, type: "assistant:end", payload: JSON.stringify({ content: "hi" }) });
    const r = await handleUiCommand("/explain", app);
    expect(r.text).toContain("run_a");
    expect(r.text).toContain("/explain <runId>");
  });

  it("/explain <runId> reconstructs the timeline with reasoning, tool call, and audit detail", async () => {
    app.db.insertAudit({
      id: "aud_1",
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 7,
      summary: "read a file",
      runId: "run_x",
    });
    app.db.insertRunEvent({ runId: "run_x", seq: 0, type: "assistant:start" });
    app.db.insertRunEvent({ runId: "run_x", seq: 1, type: "tool:call", payload: JSON.stringify({ id: "c1", name: "fs.read", args: { path: "a.ts" } }) });
    app.db.insertRunEvent({ runId: "run_x", seq: 2, type: "tool:result", payload: JSON.stringify({ id: "c1", name: "fs.read", ok: true, summary: "read a file", auditId: "aud_1" }) });
    app.db.insertRunEvent({ runId: "run_x", seq: 3, type: "assistant:end", payload: JSON.stringify({ content: "done", reasoning: "thought about it" }) });

    const r = await handleUiCommand("/explain run_x", app);
    expect(r.title).toContain("run_x");
    expect(r.switchPanel).toBeUndefined(); // routes through the transcript card, not a panel
    expect(r.text).toContain("fs.read");
    expect(r.text).toContain("7ms"); // audit-enriched outcome/duration
    expect(r.text).toContain("thought about it"); // surfaced reasoning
    expect(r.text).toContain("done");
  });

  it("/explain <unknownId> reports no events without crashing", async () => {
    const r = await handleUiCommand("/explain run_missing", app);
    expect(r.handled).toBe(true);
    expect(r.text).toContain("No events found");
  });

  it("/explain tolerates a malformed event payload", async () => {
    app.db.insertRunEvent({ runId: "run_bad", seq: 0, type: "tool:call", payload: "{not json" });
    const r = await handleUiCommand("/explain run_bad", app);
    expect(r.handled).toBe(true);
    // The unparsable payload yields an empty name placeholder, not a thrown error.
    expect(r.text).toContain("tool");
  });

  it("/explain surfaces a truncated payload as a notice, not an empty entry", async () => {
    app.db.insertRunEvent({
      runId: "run_trunc",
      seq: 0,
      type: "assistant:end",
      payload: JSON.stringify({ truncated: true, bytes: 9000, preview: "the start of a very long answer" }),
    });
    const r = await handleUiCommand("/explain run_trunc", app);
    expect(r.text).toContain("truncated");
    expect(r.text).toContain("the start of a very long answer");
  });

  it("/explain uses the full arg as the runId (ignores trailing tokens gracefully)", async () => {
    app.db.insertRunEvent({ runId: "run_a", seq: 0, type: "assistant:start" });
    // A spurious trailing token must not resolve to a different/empty run.
    const r = await handleUiCommand("/explain run_a", app);
    expect(r.title).toContain("run_a");
    expect(r.text).not.toContain("No events found");
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

  it("/models reports model routing (issue #50: was handled but undiscoverable)", async () => {
    const r = await handleUiCommand("/models", app);
    expect(r.handled).toBe(true);
    expect(r.title).toBe("Models");
    expect((r.text ?? "").length).toBeGreaterThan(0);
  });

  it("/help opens the help panel and lists every registry command", async () => {
    const r = await handleUiCommand("/help", app);
    expect(r.switchPanel).toBe("help");
    // The whole point of #50: the help surface enumerates exactly the registry,
    // so a check for one or two commands can't pass while others are dropped.
    for (const line of helpLines()) expect(r.text).toContain(line);
    expect(r.text).toContain("/models");
  });

  it("/clear resets the session and flags the transcript for clearing (#114)", async () => {
    app.session.injectNote("some history");
    expect(app.session.getMessages().length).toBeGreaterThan(3);

    const r = await handleUiCommand("/clear", app);

    expect(r.handled).toBe(true);
    expect(r.title).toBe("Clear");
    expect(r.clearTranscript).toBe(true);
    expect(r.text).toContain("starting fresh");
    // The session really reset to its 3 control messages — no leftover history.
    expect(app.session.getMessages().length).toBe(3);
    expect(app.session.getMessages().some((m) => m.content?.includes("some history"))).toBe(false);
  });

  it("every registry command is actually handled by the Ink handler (no list/switch drift)", async () => {
    for (const cmd of COMMAND_REGISTRY) {
      const r = await handleUiCommand(`/${cmd.name}`, app);
      expect(r.handled).toBe(true);
      // The switch's default branch reports unknowns; a registered command must
      // never fall through to it.
      expect(r.title).not.toBe("Unknown command");
    }
  });
});

describe("handleSlashCommand (REPL slash commands)", () => {
  let app: App;
  let warnSpy: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    app = makeApp();
    // Funnel every render method through a silenced line() so the drift loop
    // doesn't spew to stdout, while still recording warn() calls.
    vi.spyOn(render, "line").mockImplementation(() => {});
    warnSpy = vi.spyOn(render, "warn");
  });
  afterEach(async () => {
    vi.restoreAllMocks();
    await app.shutdown();
    fs.rmSync(lastStateDir, { recursive: true, force: true });
  });

  it("every registry command is actually handled by the REPL handler (no list/switch drift)", async () => {
    for (const cmd of COMMAND_REGISTRY) {
      warnSpy.mockClear();
      const r = await handleSlashCommand(`/${cmd.name}`, app);
      expect(r.handled).toBe(true);
      // The REPL default branch warns "Unknown command /x" — a registered
      // command must never trigger it (other warns, e.g. offline reconnect, ok).
      const unknownWarned = warnSpy.mock.calls.some(([m]) =>
        String(m).includes("Unknown command"),
      );
      expect(unknownWarned).toBe(false);
    }
  });

  it("/clear resets the session and reports success (#114)", async () => {
    const successSpy = vi.spyOn(render, "success").mockImplementation(() => {});
    app.session.injectNote("repl history");
    expect(app.session.getMessages().length).toBeGreaterThan(3);

    const r = await handleSlashCommand("/clear", app);

    expect(r.handled).toBe(true);
    expect(app.session.getMessages().length).toBe(3);
    expect(successSpy.mock.calls.some(([m]) => String(m).includes("cleared"))).toBe(true);
  });

  it("/clear wipes the host terminal scrollback on a TTY (#137)", async () => {
    vi.spyOn(render, "success").mockImplementation(() => {});
    const realIsTTY = process.stdout.isTTY;
    // Force a TTY so the scrollback wipe isn't gated out, then capture writes.
    (process.stdout as unknown as { isTTY: boolean }).isTTY = true;
    const writeSpy = vi
      .spyOn(process.stdout, "write")
      .mockImplementation(() => true);

    try {
      await handleSlashCommand("/clear", app);
      const wroteClear = writeSpy.mock.calls.some(([chunk]) =>
        String(chunk).includes(HOST_TERMINAL_CLEAR),
      );
      expect(wroteClear).toBe(true);
    } finally {
      writeSpy.mockRestore();
      (process.stdout as unknown as { isTTY: boolean }).isTTY = realIsTTY;
    }
  });

  it("/clear writes no scrollback escape when stdout is not a TTY (#137)", async () => {
    vi.spyOn(render, "success").mockImplementation(() => {});
    const realIsTTY = process.stdout.isTTY;
    (process.stdout as unknown as { isTTY: boolean | undefined }).isTTY =
      undefined;
    const writeSpy = vi
      .spyOn(process.stdout, "write")
      .mockImplementation(() => true);

    try {
      await handleSlashCommand("/clear", app);
      const wroteClear = writeSpy.mock.calls.some(([chunk]) =>
        String(chunk).includes(HOST_TERMINAL_CLEAR),
      );
      expect(wroteClear).toBe(false);
    } finally {
      writeSpy.mockRestore();
      (process.stdout as unknown as { isTTY: boolean | undefined }).isTTY =
        realIsTTY;
    }
  });

  it("an unregistered command warns Unknown command", async () => {
    await handleSlashCommand("/frobnicate", app);
    const unknownWarned = warnSpy.mock.calls.some(([m]) =>
      String(m).includes("Unknown command"),
    );
    expect(unknownWarned).toBe(true);
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
