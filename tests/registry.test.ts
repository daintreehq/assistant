import { z } from "zod";
import { ToolRegistry } from "../src/tools/registry.js";
import { buildAllTools } from "../src/tools/index.js";
import { ok, fail, type ToolContext, type ToolDef } from "../src/tools/types.js";
import { WatchCondition } from "../src/schemas.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
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
  actor: ToolContext["actor"] = "main",
): ToolContext {
  return {
    config,
    db,
    actor,
    confirm,
    mcp: {} as any,
    queue: new Queue(db),
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

  it("turns a failed union into the menu of valid keys, not 'Invalid input'", async () => {
    // Regression for the watcher loop: passing an empty object for a union-typed
    // arg used to surface only "field: Invalid input", giving the model nothing to
    // correct toward. It must now list the discriminating keys to pick from.
    const UnionArgs = z.object({
      when: z.union([
        z.object({ stateIs: z.string() }).strict(),
        z.object({ contains: z.string() }).strict(),
      ]),
    });
    const unionTool: ToolDef = {
      name: "test.union",
      description: "A tool with a union-typed arg.",
      risk: "read",
      readOnly: true,
      schema: UnionArgs,
      parameters: { type: "object", properties: {}, additionalProperties: false },
      async handler() {
        return ok("ran");
      },
    };
    const reg = new ToolRegistry();
    reg.register(unionTool);
    const ctx = makeCtx(db, config, vi.fn());
    const res = await reg.dispatch("test.union", { when: {} }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("INVALID_ARGS");
    expect(res.summary).toContain("when: the value matched none of the allowed shapes");
    expect(res.summary).toContain("stateIs");
    expect(res.summary).toContain("contains");
    expect(res.summary).not.toContain("Invalid input");
  });

  it("lists the full WatchCondition menu for an empty condition (the real failure)", async () => {
    // Exercise the actual recursive WatchCondition union (not a toy 2-branch stand-in)
    // so the menu stays in sync with the DSL: all leaves plus the all/any/not combinators.
    const WatcherArgs = z.object({ stopWhen: WatchCondition.optional() });
    const watcherTool: ToolDef = {
      name: "test.watch",
      description: "Tool carrying the real WatchCondition union.",
      risk: "read",
      readOnly: true,
      schema: WatcherArgs,
      parameters: { type: "object", properties: {}, additionalProperties: false },
      async handler() {
        return ok("ran");
      },
    };
    const reg = new ToolRegistry();
    reg.register(watcherTool);
    const ctx = makeCtx(db, config, vi.fn());
    const res = await reg.dispatch("test.watch", { stopWhen: {} }, ctx);
    expect(res.ok).toBe(false);
    for (const key of [
      "stateIs",
      "runtimeStatusIs",
      "contains",
      "regex",
      "noOutputForMs",
      "modelJudge",
      "all",
      "any",
      "not",
    ]) {
      expect(res.summary).toContain(key);
    }
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

  it("dispatches artifact.read through the full registry and enforces its limit cap", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(buildAllTools());
    reg.assertSafe(); // artifact.read is read-only, so it must pass the no-file-edit guard.
    const store = new Map<string, string>([["artifact_x", "0123456789"]]);
    const ctx = { ...makeCtx(db, config, vi.fn()), artifactStore: store };

    const ok = await reg.dispatch("artifact.read", { artifactId: "artifact_x", limit: 4 }, ctx);
    expect(ok.ok).toBe(true);
    expect((ok.result as { content: string }).content).toBe("0123");

    // The schema caps limit at MAX_READ_CHARS (3500) so a read can't itself overflow.
    const tooBig = await reg.dispatch(
      "artifact.read",
      { artifactId: "artifact_x", limit: 7000 },
      ctx,
    );
    expect(tooBig.ok).toBe(false);
    expect(tooBig.error?.code).toBe("INVALID_ARGS");
  });

  it("stamps ctx.runId onto the audit row, and leaves it absent when unset", async () => {
    const reg = new ToolRegistry();
    reg.register(readTool);
    // A dispatch within a run carries the run id through to its audit row.
    const withRun = { ...makeCtx(db, config, vi.fn()), runId: "run_dead00" };
    await reg.dispatch("test.read", {}, withRun);
    // A scheduler-built ctx has no runId, so the row's runId stays absent.
    await reg.dispatch("test.read", {}, makeCtx(db, config, vi.fn()));

    const audit = db.listAudit();
    const stamped = audit.find((r) => r.runId === "run_dead00");
    expect(stamped).toBeDefined();
    const unstamped = audit.find((r) => r.id !== stamped!.id);
    expect(unstamped!.runId ?? undefined).toBeUndefined();
  });

  it("forwards the tool's consequence to confirm() for the approval sheet", async () => {
    const reg = new ToolRegistry();
    const tool: ToolDef = {
      ...projectTool,
      name: "test.project.consequence",
      consequence: "Creates a worktree on disk that you may later need to clean up.",
    };
    reg.register(tool);
    const confirm = vi.fn().mockResolvedValue(true);
    const ctx = makeCtx(db, config, confirm);
    await reg.dispatch("test.project.consequence", { name: "x" }, ctx);
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(confirm.mock.calls[0][0]).toMatchObject({
      toolName: "test.project.consequence",
      consequence: "Creates a worktree on disk that you may later need to clean up.",
    });
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

  it("auto-approves a confirm-required tool without prompting when autoApprove is on", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    config.autoApprove = true;
    const confirm = vi.fn().mockResolvedValue(false);
    const ctx = makeCtx(db, config, confirm);
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(true);
    // The confirm sheet is skipped entirely — no Y/N prompt.
    expect(confirm).not.toHaveBeenCalled();
    const audit = db.listAudit();
    expect(audit[0].outcome).toBe("ok");
  });

  it("still TIER_DENIES under autoApprove — the tier gate runs before approval", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    config.autoApprove = true;
    config.tier = "supervisor"; // supervisor cannot run a project-risk tool at all
    const confirm = vi.fn();
    const ctx = makeCtx(db, config, confirm);
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("TIER_DENIED");
    expect(confirm).not.toHaveBeenCalled();
  });

  it("does NOT auto-approve for a non-main actor even with autoApprove on", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    config.autoApprove = true;
    const confirm = vi.fn().mockResolvedValue(true);
    const ctx = makeCtx(db, config, confirm, "timer");
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    // Non-interactive actors are governed by scoped grants, not autoApprove.
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("CONFIRMATION_REQUIRED");
    expect(confirm).not.toHaveBeenCalled();
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

  it("denies a confirm-required tool to a non-main actor and surfaces a low-severity event", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const confirm = vi.fn().mockResolvedValue(true);
    const ctx = makeCtx(db, config, confirm, "timer");
    const res = await reg.dispatch("test.project", { name: "x" }, ctx);

    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("CONFIRMATION_REQUIRED");
    // A non-interactive actor is never prompted.
    expect(confirm).not.toHaveBeenCalled();

    const audit = db.listAudit();
    expect(audit[0].outcome).toBe("denied");

    const events = new Queue(db).digest();
    expect(events).toHaveLength(1);
    expect(events[0].source).toBe("system");
    expect(events[0].severity).toBe("info");
    expect(events[0].dedupeKey).toBe("denied:timer:test.project");
    expect(events[0].title).toContain("test.project");
  });

  it("collapses repeated autonomous denials of the same tool into one count-bumped event", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn(), "watcher");
    await reg.dispatch("test.project", { name: "x" }, ctx);
    await reg.dispatch("test.project", { name: "y" }, ctx);

    const events = new Queue(db).digest();
    expect(events).toHaveLength(1);
    expect(events[0].count).toBe(2);
    expect(events[0].dedupeKey).toBe("denied:watcher:test.project");
  });

  it("lets a non-main actor with a valid scoped grant run a confirm-required tool, audited grant_ok", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const confirm = vi.fn();
    const ctx = makeCtx(db, config, confirm, "watcher");
    ctx.actorId = "wch_1";
    const grant = db.insertGrant({
      actorId: "wch_1",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(true);
    // A non-interactive actor is still never prompted.
    expect(confirm).not.toHaveBeenCalled();

    const audit = db.listAudit();
    expect(audit[0].toolName).toBe("test.project");
    expect(audit[0].outcome).toBe("grant_ok");
    // The grant_ok row carries the authorizing grant's provenance + id so audit
    // can distinguish a local grant from a (future) Daintree session grant.
    expect(audit[0].grantSource).toBe("local");
    expect(audit[0].grantId).toBe(grant.id);

    // The single use was consumed — the next call is denied as usual.
    const res2 = await reg.dispatch("test.project", { name: "y" }, ctx);
    expect(res2.error?.code).toBe("CONFIRMATION_REQUIRED");
  });

  it("stamps the grant's actual source on the audit row, not a hardcoded 'local'", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn(), "watcher");
    ctx.actorId = "wch_d";
    // A (future) Daintree-backed grant: prove audit provenance reflects it.
    const grant = db.insertGrant({
      actorId: "wch_d",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
      source: "daintree",
    });
    expect(grant.source).toBe("daintree");

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(true);
    const audit = db.listAudit()[0];
    expect(audit.outcome).toBe("grant_ok");
    expect(audit.grantSource).toBe("daintree");
    expect(audit.grantId).toBe(grant.id);
  });

  it("authorizes by tool name as well as risk class", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn(), "timer");
    ctx.actorId = "tmr_1";
    db.insertGrant({
      actorId: "tmr_1",
      actorType: "timer",
      allowedRiskClassesJson: null,
      allowedToolNamesJson: JSON.stringify(["test.project"]),
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(true);
    expect(db.listAudit()[0].outcome).toBe("grant_ok");
  });

  it("does not let a grant scoped to a different actor authorize the call", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn(), "watcher");
    ctx.actorId = "wch_1";
    db.insertGrant({
      actorId: "wch_OTHER",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("CONFIRMATION_REQUIRED");
    const denied = db.listAudit()[0];
    expect(denied.outcome).toBe("denied");
    // A non-grant outcome never carries grant provenance.
    expect(denied.grantSource ?? null).toBeNull();
    expect(denied.grantId ?? null).toBeNull();
  });

  it("an expired grant does not authorize the call", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctx = makeCtx(db, config, vi.fn(), "watcher");
    ctx.actorId = "wch_1";
    db.insertGrant({
      actorId: "wch_1",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() - 1, // already expired
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.error?.code).toBe("CONFIRMATION_REQUIRED");
  });

  it("a grant never overrides a tier denial", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    config.tier = "supervisor"; // 'project' is not allowed at all
    const ctx = makeCtx(db, config, vi.fn(), "watcher");
    ctx.actorId = "wch_1";
    db.insertGrant({
      actorId: "wch_1",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.error?.code).toBe("TIER_DENIED");
    // The grant use must NOT have been consumed by a tier-denied call.
    expect(db.listGrants("wch_1")[0].usesRemaining).toBe(1);
  });

  it("does not let a grant of a different actor type authorize the call", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    // Same id, but the grant is for a watcher while the actor is a timer.
    const ctx = makeCtx(db, config, vi.fn(), "timer");
    ctx.actorId = "wch_1";
    db.insertGrant({
      actorId: "wch_1",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["project"]),
      allowedToolNamesJson: null,
      expiresAt: Date.now() + 60_000,
      maxUses: 1,
    });

    const res = await reg.dispatch("test.project", { name: "x" }, ctx);
    expect(res.error?.code).toBe("CONFIRMATION_REQUIRED");
    // The mismatched grant must not have been consumed.
    expect(db.getGrant(db.listGrants("wch_1")[0].id)?.usesRemaining).toBe(1);
  });

  it("keeps distinct actors' denial events from collapsing via the actor id", async () => {
    const reg = new ToolRegistry();
    reg.register(projectTool);
    const ctxA = makeCtx(db, config, vi.fn(), "watcher");
    ctxA.actorId = "wch_a";
    const ctxB = makeCtx(db, config, vi.fn(), "watcher");
    ctxB.actorId = "wch_b";
    await reg.dispatch("test.project", { name: "x" }, ctxA);
    await reg.dispatch("test.project", { name: "y" }, ctxB);

    const events = new Queue(db).digest();
    expect(events).toHaveLength(2);
    expect(events.map((e) => e.dedupeKey).sort()).toEqual([
      "denied:watcher:wch_a:test.project",
      "denied:watcher:wch_b:test.project",
    ]);
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
