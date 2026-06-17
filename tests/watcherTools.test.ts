import { describe, it, expect } from "vitest";
import { watcherTools } from "../src/tools/watcherTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const create = watcherTools.find((t) => t.name === "watcher.terminal.create")!;

function ctxWith(daemonActive?: () => boolean): ToolContext {
  const db = new Db(":memory:");
  return {
    db,
    actor: "main",
    daemonActive,
  } as unknown as ToolContext;
}

const args = {
  terminalIds: ["term_1"],
  title: "build",
  goal: "wait for green",
};

describe("watcher.terminal.create lifecycle notice", () => {
  it("warns that supervision pauses on close when the scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => true));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
    expect(res.summary).not.toContain("no scheduler is running");
  });

  it("warns it will not check when no scheduler is running", async () => {
    const res = await create.handler(args, ctxWith(() => false));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not check");
  });

  it("assumes the scheduler is active (pauses-on-close wording) when daemonActive is absent", async () => {
    const res = await create.handler(args, ctxWith(undefined));
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
  });
});
