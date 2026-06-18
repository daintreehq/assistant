import { describe, it, expect, vi, afterEach } from "vitest";
import { workflowTools } from "../src/tools/workflowTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const create = workflowTools.find((t) => t.name === "workflow.create")!;
const get = workflowTools.find((t) => t.name === "workflow.get")!;
const list = workflowTools.find((t) => t.name === "workflow.list")!;
const update = workflowTools.find((t) => t.name === "workflow.update")!;

function ctx(): ToolContext {
  const db = new Db(":memory:");
  return { db, actor: "main" } as unknown as ToolContext;
}

afterEach(() => {
  // Some tests opt into fake timers; never leak them into the next test.
  vi.useRealTimers();
});

type CreateResult = { result: { id: string; workflow: Record<string, unknown> } };
type GetResult = { result: { workflow: Record<string, unknown> } };
type ListResult = { result: { workflows: Array<Record<string, unknown>> } };

describe("workflow.create", () => {
  it("creates a pending run and serializes array + action fields", async () => {
    const c = ctx();
    const res = await create.handler(
      {
        issueNumber: 25,
        issueTitle: "Add a durable workflow ledger",
        terminalIds: ["term_1", "term_2"],
        nextAction: { label: "Start work", toolName: "workflow.update" },
      },
      c,
    );
    expect(res.ok).toBe(true);
    const { id, workflow } = (res as CreateResult).result;
    expect(id).toMatch(/^wfr_[0-9a-f]{8}$/);
    expect(workflow.status).toBe("pending");
    expect(workflow.terminalIds).toEqual(["term_1", "term_2"]);
    expect((workflow.nextAction as { label: string }).label).toBe("Start work");

    // Stored as serialized JSON in the underlying record.
    const stored = c.db.getWorkflowRun(id)!;
    expect(JSON.parse(stored.terminalIdsJson!)).toEqual(["term_1", "term_2"]);
    expect(stored.status).toBe("pending");
  });

  it("honors an explicit initial status", async () => {
    const c = ctx();
    const res = await create.handler({ issueNumber: 1, status: "active" }, c);
    expect((res as CreateResult).result.workflow.status).toBe("active");
  });

  it("stamps completedAt when created directly in a terminal status", async () => {
    const c = ctx();
    const res = await create.handler({ issueNumber: 1, status: "done" }, c);
    expect((res as CreateResult).result.workflow.completedAt).toBeGreaterThan(0);
  });

  it("leaves completedAt unset for a non-terminal initial status", async () => {
    const c = ctx();
    const res = await create.handler({ issueNumber: 1, status: "active" }, c);
    expect((res as CreateResult).result.workflow.completedAt).toBeUndefined();
  });
});

describe("workflow.get", () => {
  it("returns a deserialized view for a known id", async () => {
    const c = ctx();
    const created = (await create.handler(
      { issueNumber: 9, watcherIds: ["wch_1"] },
      c,
    )) as CreateResult;
    const res = await get.handler({ id: created.result.id }, c);
    expect(res.ok).toBe(true);
    expect((res as GetResult).result.workflow.watcherIds).toEqual(["wch_1"]);
  });

  it("fails for an unknown id", async () => {
    const res = await get.handler({ id: "wfr_missing" }, ctx());
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("WORKFLOW_NOT_FOUND");
  });
});

describe("workflow.list", () => {
  it("filters by status", async () => {
    const c = ctx();
    await create.handler({ issueNumber: 1, status: "active" }, c);
    await create.handler({ issueNumber: 2, status: "blocked" }, c);
    await create.handler({ issueNumber: 3, status: "active" }, c);

    const active = (await list.handler({ status: "active" }, c)) as ListResult;
    expect(active.result.workflows).toHaveLength(2);
    expect(active.result.workflows.every((w) => w.status === "active")).toBe(true);

    const all = (await list.handler({}, c)) as ListResult;
    expect(all.result.workflows).toHaveLength(3);
  });
});

describe("workflow.update", () => {
  it("patches a field and leaves others intact", async () => {
    const c = ctx();
    const created = (await create.handler(
      { issueNumber: 5, issueTitle: "keep me", terminalIds: ["term_1"] },
      c,
    )) as CreateResult;
    const id = created.result.id;

    const res = await update.handler({ id, prNumber: 77 }, c);
    expect(res.ok).toBe(true);
    const w = (res as GetResult).result.workflow;
    expect(w.prNumber).toBe(77);
    // Untouched fields survive the patch.
    expect(w.issueTitle).toBe("keep me");
    expect(w.terminalIds).toEqual(["term_1"]);
  });

  it("replaces array fields wholesale", async () => {
    const c = ctx();
    const created = (await create.handler(
      { issueNumber: 5, terminalIds: ["term_1", "term_2"] },
      c,
    )) as CreateResult;
    const res = await update.handler(
      { id: created.result.id, terminalIds: ["term_9"] },
      c,
    );
    expect((res as GetResult).result.workflow.terminalIds).toEqual(["term_9"]);
  });

  it("stamps completedAt the first time a run reaches a terminal status", async () => {
    // Fake timers give the two transitions distinct timestamps, so a buggy
    // re-stamp on the second transition would change the value and fail.
    vi.useFakeTimers();
    vi.setSystemTime(1000);
    const c = ctx();
    const created = (await create.handler({ issueNumber: 5, status: "active" }, c)) as CreateResult;
    const id = created.result.id;

    vi.setSystemTime(2000);
    const done = await update.handler({ id, status: "done" }, c);
    const completedAt = (done as GetResult).result.workflow.completedAt as number;
    expect(completedAt).toBe(2000);

    // A later status change at a different time does not overwrite the stamp.
    vi.setSystemTime(3000);
    const reopened = await update.handler({ id, status: "failed" }, c);
    expect((reopened as GetResult).result.workflow.completedAt).toBe(2000);
  });

  it("does not stamp completedAt for a non-terminal status", async () => {
    const c = ctx();
    const created = (await create.handler({ issueNumber: 5 }, c)) as CreateResult;
    const res = await update.handler(
      { id: created.result.id, status: "blocked" },
      c,
    );
    expect((res as GetResult).result.workflow.completedAt).toBeUndefined();
  });

  it("fails for an unknown id", async () => {
    const res = await update.handler({ id: "wfr_missing", status: "done" }, ctx());
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("WORKFLOW_NOT_FOUND");
  });
});
