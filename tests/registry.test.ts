import { z } from "zod";
import { ToolRegistry } from "../src/tools/registry.js";
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
