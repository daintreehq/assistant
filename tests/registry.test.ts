import { z } from "zod";
import { ToolRegistry } from "../src/tools/registry.js";
import { buildAllTools } from "../src/tools/index.js";
import { ok, fail, type ToolContext, type ToolDef } from "../src/tools/types.js";
import { Db } from "../src/storage/db.js";
import { loadConfig, type AppConfig } from "../src/config.js";
import os from "node:os";
import path from "node:path";
import fs from "node:fs";

function makeStateDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "registry-test-"));
}

function makeCtx(
  db: Db,
  config: AppConfig,
  confirm: ToolContext["confirm"],
): ToolContext {
  return {
    config,
    db,
    actor: "main",
    confirm,
    mcp: {} as any,
    queue: {} as any,
    router: {} as any,
    projectPath: config.projectPath,
    log: () => {},
  };
}

const readTool: ToolDef = {
  name: "test.read",
  description: "A read-only test tool.",
  risk: "read",
  readOnly: true,
  parameters: { type: "object", properties: {}, additionalProperties: false },
  async handler() {
    return ok("read ran");
  },
};

const ProjectArgs = z.object({ name: z.string() });
const projectTool: ToolDef = {
  name: "test.project",
  description: "A mutating project test tool.",
  risk: "project",
  schema: ProjectArgs,
  parameters: {
    type: "object",
    properties: { name: { type: "string" } },
    required: ["name"],
    additionalProperties: false,
  },
  async handler() {
    return ok("project ran");
  },
};

describe("ToolRegistry.dispatch", () => {
  let db: Db;
  let config: AppConfig;

  beforeEach(() => {
    db = new Db(":memory:");
    config = loadConfig({ tier: "operator", stateDir: makeStateDir() });
  });

  afterEach(() => {
    db.close();
  });

  it("returns UNKNOWN_TOOL for an unregistered tool", async () => {
    const reg = new ToolRegistry();
    const ctx = makeCtx(db, config, vi.fn());
    const res = await reg.dispatch("nope.missing", {}, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("UNKNOWN_TOOL");
  });

  it("returns INVALID_ARGS when args fail the zod schema", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn().mockResolvedValue(true));
    const res = await reg.dispatch("test.project", { name: 123 }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("INVALID_ARGS");
  });

  it("runs a read tool and writes an audit row", async () => {
    const reg = new ToolRegistry();
    reg.register(readTool);
    const confirm = vi.fn();
    const ctx = makeCtx(db, config, confirm);
    const res = await reg.dispatch("test.read", {}, ctx);
    expect(res.ok).toBe(true);
    expect(confirm).not.toHaveBeenCalled();

    const audit = db.listAudit();
    expect(audit.length).toBe(1);
    expect(audit[0].toolName).toBe("test.read");
    expect(audit[0].outcome).toBe("ok");
  });

  it("returns USER_DECLINED when confirm() returns false for a project tool", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const confirm = vi.fn().mockResolvedValue(false);
    const ctx = makeCtx(db, config, confirm);
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("USER_DECLINED");
    expect(confirm).toHaveBeenCalledTimes(1);
  });

  it("returns TIER_DENIED for a project tool under the supervisor tier", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    config.tier = "supervisor";
    const confirm = vi.fn().mockResolvedValue(true);
    const ctx = makeCtx(db, config, confirm);
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("TIER_DENIED");
    expect(confirm).not.toHaveBeenCalled();
  });
});

const OPENAI_NAME_RE = /^[a-zA-Z0-9_-]{1,64}$/;

function watcherCreateTool(name: string): ToolDef {
  return {
    name,
    description: "A multi-segment dotted test tool.",
    risk: "read",
    readOnly: true,
    parameters: { type: "object", properties: {}, additionalProperties: false },
    async handler() {
      return ok("ran");
    },
  };
}

describe("ToolRegistry wire-name alias layer", () => {
  it("projects every real tool to an OpenAI-legal wire name with no dots", () => {
    const all = buildAllTools();
    const reg = new ToolRegistry();
    reg.registerAll(all);
    const tools = reg.toOpenAITools();
    // No silent drops: every registered tool must be projected.
    expect(tools).toHaveLength(all.length);
    for (const t of tools) {
      expect(t.function.name).toMatch(OPENAI_NAME_RE);
      expect(t.function.name).not.toContain(".");
      // Every projected wire name must round-trip back to a real internal name.
      expect(reg.resolveWireName(t.function.name)).toBeDefined();
    }
  });

  it("throws when a sanitized wire name exceeds 64 characters", () => {
    const reg = new ToolRegistry();
    // 30 dotted segments -> a __-joined wire name far longer than 64 chars.
    const longName = Array.from({ length: 30 }, (_, i) => `seg${i}`).join(".");
    reg.register(watcherCreateTool(longName));
    expect(() => reg.toOpenAITools()).toThrow(/does not match/i);
  });

  it("round-trips a dotted name to its wire name and back", () => {
    const reg = new ToolRegistry();
    reg.register(readTool); // test.read
    const tools = reg.toOpenAITools();
    expect(tools[0].function.name).toBe("test__read");
    expect(reg.resolveWireName("test__read")).toBe("test.read");
  });

  it("sanitizes multi-dot names across every segment", () => {
    const reg = new ToolRegistry();
    reg.register(watcherCreateTool("watcher.terminal.create"));
    const tools = reg.toOpenAITools();
    expect(tools[0].function.name).toBe("watcher__terminal__create");
    expect(reg.resolveWireName("watcher__terminal__create")).toBe(
      "watcher.terminal.create",
    );
  });

  it("throws when two internal names collide on the same wire name", () => {
    const reg = new ToolRegistry();
    reg.register(watcherCreateTool("fs.read"));
    reg.register(watcherCreateTool("fs__read"));
    expect(() => reg.toOpenAITools()).toThrow(/collision/i);
  });

  it("returns undefined for an unknown wire name", () => {
    const reg = new ToolRegistry();
    reg.register(readTool);
    reg.toOpenAITools();
    expect(reg.resolveWireName("nope__missing")).toBeUndefined();
  });

  it("only includes filtered tools in the alias map", () => {
    const reg = new ToolRegistry();
    reg.register(readTool); // test.read
    reg.register(projectTool); // test.project
    const tools = reg.toOpenAITools(["test.read"]);
    expect(tools).toHaveLength(1);
    expect(tools[0].function.name).toBe("test__read");
    expect(reg.resolveWireName("test__read")).toBe("test.read");
    expect(reg.resolveWireName("test__project")).toBeUndefined();
  });

  it("still dispatches by internal dotted name after projection", async () => {
    const db = new Db(":memory:");
    const config = loadConfig({ tier: "operator", stateDir: makeStateDir() });
    const reg = new ToolRegistry();
    reg.register(readTool);
    reg.toOpenAITools();
    const ctx = makeCtx(db, config, vi.fn());
    const res = await reg.dispatch("test.read", {}, ctx);
    expect(res.ok).toBe(true);
    db.close();
  });
});
