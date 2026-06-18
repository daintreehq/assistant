import { describe, it, expect, afterEach } from "vitest";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { z } from "zod";
import {
  logDebug,
  rotateDebugLog,
  DEBUG_LOG_DIRNAME,
  DEBUG_LOG_FILE,
  MAX_ARCHIVES,
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

function project(): string {
  const p = fs.mkdtempSync(path.join(os.tmpdir(), "daintree-debuglog-"));
  created.push(p);
  return p;
}

const logsDir = (p: string) => path.join(p, DEBUG_LOG_DIRNAME);
const liveLog = (p: string) => path.join(logsDir(p), DEBUG_LOG_FILE);
const readLive = (p: string) => fs.readFileSync(liveLog(p), "utf8");

describe("logDebug", () => {
  it("is a no-op when debugLog is disabled", () => {
    const projectPath = project();
    logDebug({ debugLog: false, projectPath }, "tool.call", { tool: "fs.read" });
    expect(fs.existsSync(logsDir(projectPath))).toBe(false);
  });

  it("renders short scalars inline and structured values as untruncated blocks", () => {
    const projectPath = project();
    const cfg = { debugLog: true, projectPath };
    const longContent = "y".repeat(5000);

    logDebug(cfg, "model.response", {
      tier: "large",
      finishReason: "stop",
      content: longContent,
      toolCalls: [{ name: "fs.read", args: { path: "a.ts" } }],
    });

    const txt = readLive(projectPath);
    // Short scalars sit on the header line.
    expect(txt).toMatch(/model\.response  tier=large finishReason=stop/);
    // Structured + long values become indented blocks, kept WHOLE (no truncation).
    expect(txt).toContain("  content:");
    expect(txt).toContain(longContent);
    expect(txt).toContain("  toolCalls:");
    expect(txt).toContain('"fs.read"');
  });
});

describe("rotateDebugLog", () => {
  it("no-ops with nothing to rotate, then archives the live log on start", () => {
    const projectPath = project();
    const cfg = { debugLog: true, projectPath };

    rotateDebugLog(cfg); // nothing yet — must not create the dir
    expect(fs.existsSync(logsDir(projectPath))).toBe(false);

    logDebug(cfg, "tool.call", { tool: "fs.read" });
    rotateDebugLog(cfg);

    expect(fs.existsSync(liveLog(projectPath))).toBe(false);
    const archives = fs
      .readdirSync(logsDir(projectPath))
      .filter((f) => /^debug-.*\.log$/.test(f));
    expect(archives).toHaveLength(1);
  });

  it(`keeps only the most recent ${MAX_ARCHIVES} archives`, () => {
    const projectPath = project();
    const dir = logsDir(projectPath);
    fs.mkdirSync(dir, { recursive: true });
    for (let i = 0; i < MAX_ARCHIVES + 5; i++) {
      const n = String(i).padStart(3, "0");
      fs.writeFileSync(path.join(dir, `debug-2026-01-01T00-00-${n}.log`), `old ${n}`);
    }
    logDebug({ debugLog: true, projectPath }, "tool.call", { marker: "fresh" });
    rotateDebugLog({ debugLog: true, projectPath });

    const archives = fs
      .readdirSync(dir)
      .filter((f) => /^debug-.*\.log$/.test(f));
    expect(archives).toHaveLength(MAX_ARCHIVES);
    expect(archives.some((f) => f.includes("00-000"))).toBe(false); // oldest pruned
    const survivedFresh = archives.some((f) =>
      fs.readFileSync(path.join(dir, f), "utf8").includes("fresh"),
    );
    expect(survivedFresh).toBe(true);
  });
});

describe("ModelRouter tracing", () => {
  function cfg(projectPath: string): AppConfig {
    return {
      debugLog: true,
      projectPath,
      largeModel: "L",
      mediumModel: "M",
      smallModel: "S",
    } as unknown as AppConfig;
  }

  it("logs model.request and model.response for json calls", async () => {
    const projectPath = project();
    const fakeFw = {
      json: async () => ({ verdict: "ok" }),
    } as never;
    const router = new ModelRouter(cfg(projectPath), fakeFw);

    await router.json(
      "small",
      { messages: [{ role: "user", content: "classify this" }] },
      z.object({ verdict: z.string() }),
    );

    const txt = readLive(projectPath);
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
    const projectPath = project();
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
      config: { tier: "system", debugLog: true, projectPath } as unknown as AppConfig,
      db,
      queue: new Queue(db),
      actor: "main",
      confirm: async () => true,
      log: () => {},
    } as unknown as ToolContext;

    await reg.dispatch("demo.echo", { hello: "world" }, ctx);

    const txt = readLive(projectPath);
    expect(txt).toContain("tool.call");
    expect(txt).toContain("tool=demo.echo");
    expect(txt).toContain("outcome=ok");
    expect(txt).toContain('"hello": "world"');
    db.close();
  });
});
