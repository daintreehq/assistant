import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { grantTools } from "../src/tools/grantTools.js";
import { ToolRegistry } from "../src/tools/registry.js";
import { Queue } from "../src/queue.js";
import { Db } from "../src/storage/db.js";
import type { ToolActor, ToolContext } from "../src/tools/types.js";

const create = grantTools.find((t) => t.name === "grant.create")!;
const list = grantTools.find((t) => t.name === "grant.list")!;
const revoke = grantTools.find((t) => t.name === "grant.revoke")!;

let db: Db;

function ctx(actor: ToolActor = "main"): ToolContext {
  return { db, actor } as unknown as ToolContext;
}

const validArgs = {
  actorId: "wch_1",
  actorType: "watcher" as const,
  allowedRiskClasses: ["git"],
  ttlMs: 60_000,
  maxUses: 3,
};

describe("grant tools", () => {
  beforeEach(() => {
    db = new Db(":memory:");
  });
  afterEach(() => {
    db.close();
  });

  it("grant.create mints a live grant for the main actor", async () => {
    const res = await create.handler(validArgs, ctx());
    expect(res.ok).toBe(true);
    const grants = db.listGrants("wch_1");
    expect(grants).toHaveLength(1);
    expect(grants[0].maxUses).toBe(3);
    expect(grants[0].usesRemaining).toBe(3);
    expect(JSON.parse(grants[0].allowedRiskClassesJson!)).toEqual(["git"]);
    // Grants minted by the assistant are local-only until Daintree exposes a
    // native grants API; the source field makes that provenance explicit.
    expect(grants[0].source).toBe("local");
  });

  it("grant.create is forbidden for a non-interactive actor", async () => {
    const res = await create.handler(validArgs, ctx("timer"));
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("GRANT_ACTOR_FORBIDDEN");
    expect(db.listGrants("wch_1")).toHaveLength(0);
  });

  it("grant.create rejects an empty scope", async () => {
    const res = await create.handler(
      { actorId: "wch_1", actorType: "watcher", ttlMs: 60_000, maxUses: 1 },
      ctx(),
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("GRANT_EMPTY_SCOPE");
  });

  it("grant.create refuses to grant the grant tools themselves", async () => {
    const res = await create.handler(
      {
        actorId: "wch_1",
        actorType: "watcher",
        allowedToolNames: ["grant.create"],
        ttlMs: 60_000,
        maxUses: 1,
      },
      ctx(),
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("GRANT_UNGRANTABLE_TOOL");
  });

  it("grant.list reports live grants and filters by actor", async () => {
    await create.handler(validArgs, ctx());
    await create.handler({ ...validArgs, actorId: "wch_2" }, ctx());

    const all = await list.handler({}, ctx());
    expect((all.result as { grants: unknown[] }).grants).toHaveLength(2);

    const scoped = await list.handler({ actorId: "wch_1" }, ctx());
    expect((scoped.result as { grants: unknown[] }).grants).toHaveLength(1);
  });

  it("rejects an over-long ttl via the schema, persisting no grant", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(grantTools);
    const dispatchCtx = {
      db,
      actor: "main",
      config: { tier: "operator" },
      queue: new Queue(db),
    } as unknown as ToolContext;

    const res = await reg.dispatch(
      "grant.create",
      { ...validArgs, ttlMs: Number.MAX_SAFE_INTEGER },
      dispatchCtx,
    );
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("INVALID_ARGS");
    // The Zod cap fires before insertGrant, so no ghost row is left behind.
    expect(db.listGrants("wch_1")).toHaveLength(0);
  });

  it("grant.revoke revokes a live grant (main only) and 404s otherwise", async () => {
    const created = await create.handler(validArgs, ctx());
    const id = (created.result as { id: string }).id;

    // A non-interactive actor cannot revoke.
    const denied = await revoke.handler({ id }, ctx("watcher"));
    expect(denied.error?.code).toBe("GRANT_ACTOR_FORBIDDEN");

    const res = await revoke.handler({ id }, ctx());
    expect(res.ok).toBe(true);
    expect(db.listGrants("wch_1")).toHaveLength(0);

    // Second revoke finds nothing live.
    const again = await revoke.handler({ id }, ctx());
    expect(again.error?.code).toBe("GRANT_NOT_FOUND");
  });
});
