import { UiBridge, type UiBridgeEvent } from "../src/ui/bridge.js";
import type { PendingConfirm } from "../src/ui/types.js";

describe("UiBridge", () => {
  it("resolves a confirmation promise when the UI answers", async () => {
    const bridge = new UiBridge();
    let pending: PendingConfirm | undefined;
    bridge.subscribe((e) => {
      if (e.type === "confirm") pending = e.pending;
    });

    const approved = bridge.requestConfirm({
      toolName: "git.commit",
      risk: "git",
      summary: "commit staged changes",
      args: {},
    });
    expect(pending).toBeDefined();
    pending!.resolve(true);
    expect(await approved).toBe(true);
  });

  it("resolves false on decline", async () => {
    const bridge = new UiBridge();
    let pending: PendingConfirm | undefined;
    bridge.subscribe((e) => {
      if (e.type === "confirm") pending = e.pending;
    });
    const p = bridge.requestConfirm({
      toolName: "x",
      risk: "terminal",
      summary: "s",
      args: {},
    });
    pending!.resolve(false);
    expect(await p).toBe(false);
  });

  it("republishes agent-loop events onto the bridge", () => {
    const bridge = new UiBridge();
    const seen: UiBridgeEvent[] = [];
    bridge.subscribe((e) => seen.push(e));

    const sink = bridge.agentEvents();
    sink.assistantStart();
    sink.assistantToken("hi");
    sink.toolCall({ id: "c1", name: "fs.read", args: { path: "a" }, startedAt: 0 });
    sink.toolResult({ id: "c1", name: "fs.read", result: { ok: true, summary: "ok" }, endedAt: 1 });
    sink.error("nope");

    expect(seen.map((e) => e.type)).toEqual([
      "assistant:start",
      "assistant:token",
      "tool:call",
      "tool:result",
      "log",
    ]);
    const log = seen[4];
    expect(log.type === "log" && log.level).toBe("error");
  });

  it("settlePendingConfirms unblocks outstanding confirms (teardown safety)", async () => {
    const bridge = new UiBridge();
    bridge.subscribe(() => {});
    const a = bridge.requestConfirm({
      toolName: "x",
      risk: "git",
      summary: "s",
      args: {},
    });
    const b = bridge.requestConfirm({
      toolName: "y",
      risk: "git",
      summary: "s",
      args: {},
    });
    bridge.settlePendingConfirms(false);
    expect(await a).toBe(false);
    expect(await b).toBe(false);
  });

  it("a confirm resolves at most once", async () => {
    const bridge = new UiBridge();
    let pending: PendingConfirm | undefined;
    bridge.subscribe((e) => {
      if (e.type === "confirm") pending = e.pending;
    });
    const p = bridge.requestConfirm({
      toolName: "x",
      risk: "git",
      summary: "s",
      args: {},
    });
    pending!.resolve(true);
    pending!.resolve(false); // ignored — already settled
    bridge.settlePendingConfirms(false); // also a no-op now
    expect(await p).toBe(true);
  });

  it("unsubscribe stops delivery", () => {
    const bridge = new UiBridge();
    const seen: UiBridgeEvent[] = [];
    const off = bridge.subscribe((e) => seen.push(e));
    bridge.emit({ type: "assistant:start" });
    off();
    bridge.emit({ type: "assistant:start" });
    expect(seen).toHaveLength(1);
  });
});
