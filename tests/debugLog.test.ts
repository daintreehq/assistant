import { describe, it, expect, afterEach } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { z } from "zod";
import {
  logDebug,
  startDebugLog,
  currentDebugLogPath,
  MAX_LOG_AGE_MS,
} from "../src/debugLog.js";
import { ModelRouter } from "../src/models/router.js";
import { ToolRegistry } from "../src/tools/registry.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { AppConfig } from "../src/config.js";
import type { ToolContext, ToolDef } from "../src/tools/types.js";

const created: string[] = [];
afterEach(() => {
  for (const d of created.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

/** A logDir path that does NOT exist yet, so we can assert it's only created on write. */
function freshLogDir(): string {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "daintree-debuglog-"));
  created.push(base);
  return path.join(base, "logs");
}

const SESSION_RE = /\d{4}-\d{2}-\d{2}-[\w.-]+\.log$/;

describe("logDebug", () => {
  it("is a no-op when debugLog is disabled", () => {
    const logDir = freshLogDir();
    logDebug({ debugLog: false, logDir }, "tool.call", { tool: "fs.read" });
    expect(fs.existsSync(logDir)).toBe(false);
  });

  it("writes to a dated session file and keeps short scalars inline / big values whole", () => {
    const logDir = freshLogDir();
    const cfg = { debugLog: true, logDir };
    const longContent = "y".repeat(5000);

    logDebug(cfg, "model.response", {
      tier: "large",
      finishReason: "stop",
      content: longContent,
      toolCalls: [{ name: "fs.read", args: { path: "a.ts" } }],
    });

    const file = currentDebugLogPath()!;
    expect(path.basename(file)).toMatch(SESSION_RE);
    const txt = fs.readFileSync(file, "utf8");
    expect(txt).toMatch(/model\.response  tier=large finishReason=stop/);
    expect(txt).toContain("  content:");
    expect(txt).toContain(longContent); // untruncated
    expect(txt).toContain('"fs.read"');
  });
});

describe("startDebugLog", () => {
  function fullCfg(logDir: string): AppConfig {
    return {
      debugLog: true,
      logDir,
      projectPath: "/Users/dev/some-project",
      projectId: "proj-7",
      tier: "system",
      largeModel: "L",
      mediumModel: "M",
      smallModel: "S",
      mcpUrl: "http://127.0.0.1:45454/mcp",
      offline: false,
      autoApprove: false,
      stateDir: "/state",
    } as unknown as AppConfig;
  }

  it("opens a <date>-<id>.log, returns its path, and writes a session header", () => {
    const logDir = freshLogDir();
    const file = startDebugLog(fullCfg(logDir), "ses_ab12cd34");

    expect(file).toBeTruthy();
    expect(path.basename(file!)).toMatch(/^\d{4}-\d{2}-\d{2}-ses_ab12cd34\.log$/);
    expect(file).toBe(currentDebugLogPath());
    const txt = fs.readFileSync(file!, "utf8");
    expect(txt).toContain("session.start");
    expect(txt).toContain("project=/Users/dev/some-project");
    expect(txt).toContain("tier=system");
    expect(txt).toContain("smallModel=S");
  });

  it("returns undefined and writes nothing when disabled", () => {
    const logDir = freshLogDir();
    const file = startDebugLog({ debugLog: false, logDir } as unknown as AppConfig);
    expect(file).toBeUndefined();
    expect(fs.existsSync(logDir)).toBe(false);
  });

  it("deletes logs older than 7 days at boot, keeps recent ones", () => {
    const logDir = freshLogDir();
    fs.mkdirSync(logDir, { recursive: true });
    const stale = path.join(logDir, "2026-01-01-old.log");
    const recent = path.join(logDir, "2026-06-17-recent.log");
    fs.writeFileSync(stale, "old");
    fs.writeFileSync(recent, "recent");
    // Age the stale file well past the 7-day cutoff via mtime.
    const old = new Date(Date.now() - MAX_LOG_AGE_MS - 86_400_000);
    fs.utimesSync(stale, old, old);

    startDebugLog(fullCfg(logDir), "ses_new");

    expect(fs.existsSync(stale)).toBe(false); // pruned
    expect(fs.existsSync(recent)).toBe(true); // within 7 days, kept
    expect(SESSION_RE.test(currentDebugLogPath()!)).toBe(true);
  });
});

describe("ModelRouter tracing", () => {
  function cfg(logDir: string): AppConfig {
    return {
      debugLog: true,
      logDir,
      largeModel: "L",
      mediumModel: "M",
      smallModel: "S",
    } as unknown as AppConfig;
  }

  it("logs model.request and model.response for json calls", async () => {
    const logDir = freshLogDir();
    const fakeFw = { json: async () => ({ verdict: "ok" }) } as never;
    const router = new ModelRouter(cfg(logDir), fakeFw);

    await router.json(
      "small",
      { messages: [{ role: "user", content: "classify this" }] },
      z.object({ verdict: z.string() }),
    );

    const txt = fs.readFileSync(currentDebugLogPath()!, "utf8");
    expect(txt).toContain("model.request");
    expect(txt).toContain("kind=json");
    expect(txt).toContain("model=S");
    expect(txt).toContain("classify this");
    expect(txt).toContain("model.response");
    expect(txt).toContain('"verdict": "ok"');
  });
});

describe("ToolRegistry tracing", () => {
  it("logs tool.call with args and result on every dispatch", async () => {
    const logDir = freshLogDir();
    const db = new Db(":memory:");
    const reg = new ToolRegistry();
    const tool: ToolDef = {
      name: "demo.echo",
      description: "echo",
      risk: "read",
      readOnly: true,
      parameters: { type: "object", properties: {}, additionalProperties: true },
      handler: async (args) => ({ ok: true, summary: "done", result: { echoed: args } }),
    };
    reg.register(tool);

    const ctx = {
      config: { tier: "system", debugLog: true, logDir } as unknown as AppConfig,
      db,
      queue: new Queue(db),
      actor: "main",
      confirm: async () => true,
      log: () => {},
    } as unknown as ToolContext;

    await reg.dispatch("demo.echo", { hello: "world" }, ctx);

    const txt = fs.readFileSync(currentDebugLogPath()!, "utf8");
    expect(txt).toContain("tool.call");
    expect(txt).toContain("tool=demo.echo");
    expect(txt).toContain("outcome=ok");
    expect(txt).toContain('"hello": "world"');
    db.close();
  });
});
