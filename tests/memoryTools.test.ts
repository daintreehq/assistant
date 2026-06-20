import { describe, it, expect } from "vitest";
import { memoryTools } from "../src/tools/memoryTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const recall = memoryTools.find((t) => t.name === "memory.recall")!;
const list = memoryTools.find((t) => t.name === "memory.list")!;
const save = memoryTools.find((t) => t.name === "memory.save")!;
const forget = memoryTools.find((t) => t.name === "memory.forget")!;
const pin = memoryTools.find((t) => t.name === "memory.pin")!;
const unpin = memoryTools.find((t) => t.name === "memory.unpin")!;

function ctx(): ToolContext {
  const db = new Db(":memory:");
  return { db, actor: "main" } as unknown as ToolContext;
}

type SaveResult = { result: { id: string; memory: { source: string; pinned: boolean } } };
type RecallResult = { result: { memories: Array<{ id: string; content: string }> } };
type ListResult = { result: { memories: Array<{ id: string; pinned: boolean }> } };

describe("memory.save", () => {
  it("persists a memory recallable by content", async () => {
    const c = ctx();
    const res = await save.handler({ content: "always run tsc directly, not via rtk" }, c);
    expect(res.ok).toBe(true);
    const { id, memory } = (res as SaveResult).result;
    expect(id).toMatch(/^mem_[0-9a-f]{8}$/);
    expect(memory.source).toBe("assistant");

    const found = await recall.handler({ query: "tsc" }, c);
    expect((found as RecallResult).result.memories.map((m) => m.id)).toContain(id);
  });

  it("accepts an explicit user source", async () => {
    const c = ctx();
    const res = await save.handler({ content: "deploy from main", source: "user" }, c);
    expect((res as SaveResult).result.memory.source).toBe("user");
  });

  it("rejects the reserved 'compact' source via schema validation", () => {
    // "compact" is internal-only; the tool's Zod schema must not accept it.
    expect(save.schema!.safeParse({ content: "x", source: "compact" }).success).toBe(false);
    expect(save.schema!.safeParse({ content: "x", source: "user" }).success).toBe(true);
  });

  it("is a local-risk (non-read-risk) tool", () => {
    expect(save.risk).toBe("local");
    expect(save.risk).not.toBe("read");
  });
});

describe("memory.recall / memory.list", () => {
  it("recall is read-only and returns empty for a blank query without error", async () => {
    const c = ctx();
    await save.handler({ content: "a fact" }, c);
    expect(recall.risk).toBe("read");
    const res = await recall.handler({ query: "   " }, c);
    expect(res.ok).toBe(true);
    expect((res as RecallResult).result.memories).toEqual([]);
  });

  it("recall tolerates FTS operators / quotes without throwing", async () => {
    const c = ctx();
    await save.handler({ content: "watch for operators and quotes" }, c);
    for (const query of ['"', 'watch "for"', "watch OR operators", "(unbalanced"]) {
      const res = await recall.handler({ query }, c);
      expect(res.ok).toBe(true);
    }
  });

  it("list filters to pinnedOnly and reflects pin state", async () => {
    const c = ctx();
    const a = (await save.handler({ content: "alpha" }, c)) as SaveResult;
    await save.handler({ content: "beta" }, c);
    await pin.handler({ id: a.result.id }, c);

    const pinned = (await list.handler({ pinnedOnly: true }, c)) as ListResult;
    expect(pinned.result.memories.map((m) => m.id)).toEqual([a.result.id]);
    expect(pinned.result.memories[0].pinned).toBe(true);

    const all = (await list.handler({}, c)) as ListResult;
    expect(all.result.memories.length).toBe(2);
  });
});

describe("memory.forget / pin / unpin", () => {
  it("forget makes a memory unrecallable", async () => {
    const c = ctx();
    const saved = (await save.handler({ content: "ephemeral note" }, c)) as SaveResult;
    const res = await forget.handler({ id: saved.result.id }, c);
    expect(res.ok).toBe(true);
    const after = (await recall.handler({ query: "ephemeral" }, c)) as RecallResult;
    expect(after.result.memories).toEqual([]);
  });

  it("forget returns MEMORY_NOT_FOUND for an already-forgotten id", async () => {
    const c = ctx();
    const saved = (await save.handler({ content: "twice" }, c)) as SaveResult;
    await forget.handler({ id: saved.result.id }, c);
    const second = await forget.handler({ id: saved.result.id }, c);
    expect(second.ok).toBe(false);
    expect(second.error?.code).toBe("MEMORY_NOT_FOUND");
  });

  it("pin then unpin round-trips", async () => {
    const c = ctx();
    const saved = (await save.handler({ content: "keepme" }, c)) as SaveResult;
    const pinned = (await pin.handler({ id: saved.result.id }, c)) as SaveResult;
    expect(pinned.result.memory.pinned).toBe(true);
    const unpinned = (await unpin.handler({ id: saved.result.id }, c)) as SaveResult;
    expect(unpinned.result.memory.pinned).toBe(false);
  });

  it("pin on an unknown id returns MEMORY_NOT_FOUND", async () => {
    const c = ctx();
    const res = await pin.handler({ id: "mem_deadbeef" }, c);
    expect(res.ok).toBe(false);
    expect(res.error?.code).toBe("MEMORY_NOT_FOUND");
  });
});
