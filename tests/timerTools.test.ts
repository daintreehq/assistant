import { describe, it, expect } from "vitest";
import { timerTools } from "../src/tools/timerTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

const schedule = timerTools.find((t) => t.name === "timer.schedule")!;

function ctxWith(daemonActive?: () => boolean): ToolContext {
  const db = new Db(":memory:");
  return {
    db,
    actor: "main",
    daemonActive,
  } as unknown as ToolContext;
}

describe("timer.schedule lifecycle notice", () => {
  it("warns that supervision pauses on close when the scheduler is running", async () => {
    const res = await schedule.handler(
      { title: "ping", delayMs: 1000, payload: { type: "enqueue" } },
      ctxWith(() => true),
    );
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
    expect(res.summary).not.toContain("no scheduler is running");
  });

  it("warns it will not fire when no scheduler is running", async () => {
    const res = await schedule.handler(
      { title: "ping", delayMs: 1000, payload: { type: "enqueue" } },
      ctxWith(() => false),
    );
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("no scheduler is running");
    expect(res.summary).toContain("will not fire");
  });

  it("assumes the scheduler is active (pauses-on-close wording) when daemonActive is absent", async () => {
    const res = await schedule.handler(
      { title: "ping", delayMs: 1000, payload: { type: "enqueue" } },
      ctxWith(undefined),
    );
    expect(res.ok).toBe(true);
    expect(res.summary).toContain("pauses when you close the assistant");
  });
});

describe("timer.schedule payload validation", () => {
  it("rejects a run_check payload — the ungrounded timer type is no longer creatable", () => {
    // run_check was dropped from the discriminated union: it never observed real
    // state. The schema must reject it so the model can't schedule new ones.
    const parsed = schedule.schema!.safeParse({
      title: "check the build",
      delayMs: 1000,
      payload: { type: "run_check", checkPrompt: "is the build done?" },
    });
    expect(parsed.success).toBe(false);
  });

  it("still accepts enqueue and call_safe_tool payloads", () => {
    expect(
      schedule.schema!.safeParse({
        title: "remind",
        delayMs: 1000,
        payload: { type: "enqueue", message: "hi" },
      }).success,
    ).toBe(true);
    expect(
      schedule.schema!.safeParse({
        title: "run tool",
        delayMs: 1000,
        payload: { type: "call_safe_tool", toolCall: { toolName: "x.y" } },
      }).success,
    ).toBe(true);
  });
});
